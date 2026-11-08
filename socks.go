package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	handshakeTimeout = 15 * time.Second
	halfCloseIdle    = 60 * time.Second
)

// Server is the local SOCKS5 entry: no auth, CONNECT only.
type Server struct {
	pool  *Pool
	stats *Stats
}

func (s *Server) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	target, err := readSocksRequest(c)
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})

	up, px, err := s.pool.Dial(context.Background(), target)
	if err != nil {
		code := byte(1)
		var te *targetError
		if errors.As(err, &te) {
			code = te.code
		}
		writeReply(c, code)
		return
	}
	defer up.Close()
	if err := writeReply(c, 0); err != nil {
		return
	}
	s.tunnel(c, up, px, target)
}

// tunnel counts the connection in stats and relays data until it closes.
func (s *Server) tunnel(c, up net.Conn, px *Proxy, target string) {
	s.stats.totalConns.Add(1)
	s.stats.activeConns.Add(1)
	px.conns.Add(1)
	px.active.Add(1)
	defer s.stats.activeConns.Add(-1)
	defer px.active.Add(-1)

	s.relay(c, up, px, target)
}

// checkDrop decides whether the connection was dropped. An error on the proxy
// side (reset, timeout) is a drop right away. A clean close by the proxy looks
// the same as a normal close by the site, so we first check if the proxy is alive.
func (s *Server) checkDrop(px *Proxy, target string, err error) {
	alive := s.pool.Probe(px)
	if err == nil {
		if alive {
			return
		}
		err = errors.New("proxy closed the connection and stopped responding")
	}
	s.stats.drops.Add(1)
	px.drops.Add(1)
	s.stats.Event(levelWarn, "Dropped %s via %s: %v", target, s.pool.name(px), err)
}

func readSocksRequest(c net.Conn) (string, error) {
	buf := make([]byte, 256)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return "", err
	}
	if buf[0] != 5 {
		return "", errors.New("not SOCKS5")
	}
	n := int(buf[1])
	if _, err := io.ReadFull(c, buf[:n]); err != nil {
		return "", err
	}
	noAuth := false
	for _, m := range buf[:n] {
		if m == 0 {
			noAuth = true
		}
	}
	if !noAuth {
		c.Write([]byte{5, 0xff})
		return "", errors.New("client requires authentication")
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", err
	}

	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return "", err
	}
	cmd, atyp := buf[1], buf[3]
	var host string
	switch atyp {
	case 1:
		if _, err := io.ReadFull(c, buf[:4]); err != nil {
			return "", err
		}
		host = net.IP(buf[:4]).String()
	case 4:
		if _, err := io.ReadFull(c, buf[:16]); err != nil {
			return "", err
		}
		host = net.IP(buf[:16]).String()
	case 3:
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return "", err
		}
		l := int(buf[0])
		if _, err := io.ReadFull(c, buf[:l]); err != nil {
			return "", err
		}
		host = string(buf[:l])
	default:
		writeReply(c, 8)
		return "", fmt.Errorf("address type %d", atyp)
	}
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(buf[:2])
	if cmd != 1 {
		writeReply(c, 7)
		return "", fmt.Errorf("command %d not supported", cmd)
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

func writeReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}

var bufPool = sync.Pool{New: func() any { b := make([]byte, 32*1024); return &b }}

type pipeResult struct {
	download bool
	readErr  error
	writeErr error
}

// relay copies data both ways. If the connection breaks on the proxy side
// (reset, timeout) or the proxy closes it first, it calls checkDrop.
// A close by the client is never a drop.
func (s *Server) relay(client, up net.Conn, px *Proxy, target string) {
	var half atomic.Bool
	res := make(chan pipeResult, 2)
	go func() {
		r, w := pipe(client, up, &half, func(n int64) { px.down.Add(n); s.stats.bytesDown.Add(n) })
		closeWrite(client)
		res <- pipeResult{download: true, readErr: r, writeErr: w}
	}()
	go func() {
		r, w := pipe(up, client, &half, func(n int64) { px.up.Add(n); s.stats.bytesUp.Add(n) })
		closeWrite(up)
		res <- pipeResult{readErr: r, writeErr: w}
	}()

	first := <-res
	upClosedFirst := first.download && first.readErr == nil && first.writeErr == nil
	if upClosedFirst {
		// The proxy closed first: either a normal close or it died. Check now,
		// without waiting for the client to close its side.
		go s.checkDrop(px, target, nil)
	}
	if first.readErr != nil || first.writeErr != nil {
		client.Close()
		up.Close()
	} else {
		// One side is done. Wait for the other, but not forever.
		half.Store(true)
		deadline := time.Now().Add(halfCloseIdle)
		client.SetReadDeadline(deadline)
		up.SetReadDeadline(deadline)
	}
	second := <-res

	var upErr error
	for _, r := range []pipeResult{first, second} {
		// proxy side: a read error in download or a write error in upload
		err := r.writeErr
		if r.download {
			err = r.readErr
		}
		if err == nil || errors.Is(err, net.ErrClosed) {
			continue
		}
		if half.Load() && errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		if upErr == nil {
			upErr = err
		}
	}
	if upErr != nil && !upClosedFirst {
		go s.checkDrop(px, target, upErr)
	}
}

func pipe(dst, src net.Conn, half *atomic.Bool, count func(int64)) (readErr, writeErr error) {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		if half.Load() {
			src.SetReadDeadline(time.Now().Add(halfCloseIdle))
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return nil, werr
			}
			count(int64(n))
		}
		if err != nil {
			if err == io.EOF {
				return nil, nil
			}
			return err, nil
		}
	}
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}
