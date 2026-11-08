package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// HTTPServer is the local HTTP proxy entry, without auth, for CLI tools that
// don't speak SOCKS5. It uses the same pool. CONNECT becomes a tunnel. A plain
// request with an absolute URL (http://host/path) is sent to the site with
// Connection: close, so one client connection serves one request.
type HTTPServer struct {
	Server
}

func (s *HTTPServer) Serve(ln net.Listener) error {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.handleHTTP(c)
	}
}

func (s *HTTPServer) handleHTTP(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}

	var target string
	if req.Method == http.MethodConnect {
		target = req.Host
	} else {
		if req.URL.Scheme != "http" || req.URL.Host == "" {
			httpError(c, http.StatusBadRequest, "this is a proxy: send an absolute http:// URL or CONNECT")
			return
		}
		target = req.URL.Host
	}
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(strings.Trim(target, "[]"), "80")
	}

	up, px, err := s.pool.Dial(context.Background(), target)
	if err != nil {
		var te *targetError
		if errors.As(err, &te) {
			httpError(c, http.StatusBadGateway, err.Error())
		} else {
			httpError(c, http.StatusServiceUnavailable, "no proxy answered: "+err.Error())
		}
		return
	}
	defer up.Close()
	c.SetDeadline(time.Time{})

	if req.Method == http.MethodConnect {
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
	} else {
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Proxy-Connection")
		req.Close = true
		up.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		if err := req.Write(up); err != nil { // writes origin-form: GET /path HTTP/1.1
			httpError(c, http.StatusBadGateway, "send request: "+err.Error())
			return
		}
		up.SetWriteDeadline(time.Time{})
	}

	var client net.Conn = c
	if br.Buffered() > 0 {
		client = &bufConn{Conn: c, r: br}
	}
	s.tunnel(client, up, px, target)
}

func httpError(c net.Conn, code int, msg string) {
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(msg), msg)
}
