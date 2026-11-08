package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// targetError means the proxy works but could not reach the site.
// Such a proxy is not marked down.
type targetError struct {
	code byte // SOCKS5 reply code for the client
	msg  string
}

func (e *targetError) Error() string { return e.msg }

var socksReplyText = map[byte]string{
	1: "general failure",
	2: "not allowed by ruleset",
	3: "network unreachable",
	4: "host unreachable",
	5: "connection refused",
	6: "TTL expired",
	7: "command not supported",
	8: "address type not supported",
}

// dialVia opens a tunnel to target through proxy px.
func dialVia(ctx context.Context, px *Proxy, target string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	d := &net.Dialer{KeepAlive: 30 * time.Second}
	var conn net.Conn
	var err error
	if px.Scheme == "https" {
		host, _, _ := net.SplitHostPort(px.Addr)
		td := &tls.Dialer{NetDialer: d, Config: &tls.Config{ServerName: host}}
		conn, err = td.DialContext(ctx, "tcp", px.Addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", px.Addr)
	}
	if err != nil {
		return nil, fmt.Errorf("connect to proxy: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	switch px.Scheme {
	case "socks5":
		err = socks5Connect(conn, px.User, px.Pass, target)
	default:
		conn, err = httpConnect(conn, px.User, px.Pass, target)
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

func socks5Connect(c net.Conn, user, pass, target string) error {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return &targetError{code: 1, msg: "invalid address " + target}
	}
	port, _ := strconv.Atoi(portStr)

	method := byte(0)
	if user != "" {
		method = 2
	}
	if _, err := c.Write([]byte{5, 1, method}); err != nil {
		return fmt.Errorf("socks5: %w", err)
	}
	buf := make([]byte, 256)
	if _, err := io.ReadFull(c, buf[:2]); err != nil {
		return fmt.Errorf("socks5 greeting: %w", err)
	}
	if buf[0] != 5 {
		return errors.New("socks5: reply is not SOCKS5")
	}
	if buf[1] != method {
		return errors.New("socks5: proxy does not accept this auth method")
	}
	if method == 2 {
		if len(user) > 255 || len(pass) > 255 {
			return errors.New("socks5: login or password longer than 255 bytes")
		}
		req := []byte{1, byte(len(user))}
		req = append(req, user...)
		req = append(req, byte(len(pass)))
		req = append(req, pass...)
		if _, err := c.Write(req); err != nil {
			return fmt.Errorf("socks5 auth: %w", err)
		}
		if _, err := io.ReadFull(c, buf[:2]); err != nil {
			return fmt.Errorf("socks5 auth: %w", err)
		}
		if buf[1] != 0 {
			return errors.New("socks5: wrong login or password")
		}
	}

	req := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 1)
			req = append(req, ip4...)
		} else {
			req = append(req, 4)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return &targetError{code: 1, msg: "host name too long"}
		}
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port))
	if _, err := c.Write(req); err != nil {
		return fmt.Errorf("socks5 request: %w", err)
	}
	if _, err := io.ReadFull(c, buf[:4]); err != nil {
		return fmt.Errorf("socks5 reply: %w", err)
	}
	if rep := buf[1]; rep != 0 {
		text := socksReplyText[rep]
		if text == "" {
			text = fmt.Sprintf("code %d", rep)
		}
		return &targetError{code: rep, msg: fmt.Sprintf("%s: %s", target, text)}
	}
	var skip int
	switch buf[3] {
	case 1:
		skip = 4
	case 4:
		skip = 16
	case 3:
		if _, err := io.ReadFull(c, buf[:1]); err != nil {
			return fmt.Errorf("socks5 reply: %w", err)
		}
		skip = int(buf[0])
	default:
		return errors.New("socks5: unknown address type in reply")
	}
	if _, err := io.ReadFull(c, buf[:skip+2]); err != nil {
		return fmt.Errorf("socks5 reply: %w", err)
	}
	return nil
}

func httpConnect(c net.Conn, user, pass, target string) (net.Conn, error) {
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if user != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass)) + "\r\n"
	}
	req += "\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return c, fmt.Errorf("http connect: %w", err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return c, fmt.Errorf("http connect reply: %w", err)
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
	case resp.StatusCode == http.StatusProxyAuthRequired:
		return c, errors.New("http: wrong login or password (407)")
	case resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504:
		return c, &targetError{code: 4, msg: fmt.Sprintf("%s: %s", target, resp.Status)}
	case resp.StatusCode == 403:
		return c, &targetError{code: 2, msg: fmt.Sprintf("%s: %s", target, resp.Status)}
	default:
		return c, &targetError{code: 1, msg: fmt.Sprintf("%s: %s", target, resp.Status)}
	}
	if br.Buffered() > 0 {
		return &bufConn{Conn: c, r: br}, nil
	}
	return c, nil
}

// bufConn returns the bytes bufio has already read past the response headers.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return b.Conn.Close()
}
