package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// echo server standing in for a site
func startEcho(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// fake SOCKS5 proxy with login and password
func startSocksUpstream(t *testing.T, user, pass string) string {
	addr, _ := startKillableSocks(t, user, pass)
	return addr
}

// startKillableSocks also returns a function that kills the proxy:
// it closes the listener and every open connection with a plain FIN.
func startKillableSocks(t *testing.T, user, pass string) (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	kill := func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	}
	t.Cleanup(kill)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 256)
				io.ReadFull(c, b[:2])
				io.ReadFull(c, b[:b[1]])
				c.Write([]byte{5, 2})
				io.ReadFull(c, b[:2])
				u := make([]byte, b[1])
				io.ReadFull(c, u)
				io.ReadFull(c, b[:1])
				p := make([]byte, b[0])
				io.ReadFull(c, p)
				if string(u) != user || string(p) != pass {
					c.Write([]byte{1, 1})
					return
				}
				c.Write([]byte{1, 0})
				io.ReadFull(c, b[:4])
				var host string
				switch b[3] {
				case 1:
					io.ReadFull(c, b[:4])
					host = net.IP(b[:4]).String()
				case 3:
					io.ReadFull(c, b[:1])
					l := b[0]
					io.ReadFull(c, b[:l])
					host = string(b[:l])
				}
				io.ReadFull(c, b[:2])
				port := binary.BigEndian.Uint16(b[:2])
				up, err := net.Dial("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
				if err != nil {
					c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
				go io.Copy(up, c)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String(), kill
}

// fake HTTP CONNECT proxy with Basic auth
func startHTTPUpstream(t *testing.T, user, pass string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				if req.Header.Get("Proxy-Authorization") != want {
					io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n")
					return
				}
				up, err := net.Dial("tcp", req.Host)
				if err != nil {
					io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				go io.Copy(up, br)
				io.Copy(c, up)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func deadAddr(t *testing.T) string {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func startBridge(t *testing.T, proxiesFile string) (string, *Pool, *Stats) {
	stats := NewStats()
	pool := &Pool{stats: stats, path: proxiesFile, defScheme: "socks5", dialTimeout: 2 * time.Second, checkTarget: "127.0.0.1:1", maxAttempts: 5}
	if _, err := pool.Load(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go (&Server{pool: pool, stats: stats}).Serve(ln)
	return ln.Addr().String(), pool, stats
}

func roundTrip(t *testing.T, bridge, target, msg string) error {
	c, err := net.DialTimeout("tcp", bridge, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err := socks5Connect(c, "", "", target); err != nil {
		return err
	}
	if _, err := io.WriteString(c, msg); err != nil {
		return err
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if string(got) != msg {
		return fmt.Errorf("got %q", got)
	}
	return nil
}

func writeFile(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), "proxies.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParse(t *testing.T) {
	list, bad := parseProxyList("# comment\n1.2.3.4:1000:u:p:with:colons, 5.6.7.8:2000:a:b\nhttp://9.9.9.9:3128:x:y\n\n10.0.0.1:80\nbroken:line:x\n", "socks5")
	if len(list) != 4 || len(bad) != 1 {
		t.Fatalf("list=%d bad=%v", len(list), bad)
	}
	if list[0].Pass != "p:with:colons" || list[0].Addr != "1.2.3.4:1000" || list[0].Scheme != "socks5" {
		t.Fatalf("%+v", list[0])
	}
	if list[2].Scheme != "http" || list[3].User != "" {
		t.Fatalf("%+v %+v", list[2], list[3])
	}
	if !strings.Contains(bad[0], "broken") {
		t.Fatal(bad)
	}
}

func TestFailoverSocks(t *testing.T) {
	echo := startEcho(t)
	good := startSocksUpstream(t, "user", "pa:ss")
	wrongPass := startSocksUpstream(t, "user", "other")
	file := writeFile(t, fmt.Sprintf("%s:u:p\n%s:user:pa:ss,%s:user:pa:ss\n", deadAddr(t), wrongPass, good))
	_ = wrongPass
	bridge, pool, stats := startBridge(t, file)

	if err := roundTrip(t, bridge, echo, "hello through the proxy"); err != nil {
		t.Fatal(err)
	}
	if cur := pool.current(); cur.Addr != good {
		t.Fatalf("active %s, want %s", cur.Addr, good)
	}
	if stats.failovers.Load() < 1 || stats.dialFails.Load() != 2 {
		t.Fatalf("failovers=%d dialFails=%d", stats.failovers.Load(), stats.dialFails.Load())
	}
	// the second connection goes straight through the working proxy
	if err := roundTrip(t, bridge, echo, "second"); err != nil {
		t.Fatal(err)
	}
	if stats.dialFails.Load() != 2 {
		t.Fatalf("extra attempts: %d", stats.dialFails.Load())
	}
	time.Sleep(50 * time.Millisecond)
	if stats.bytesUp.Load() == 0 || stats.bytesDown.Load() == 0 {
		t.Fatal("traffic not counted")
	}
}

func TestHTTPUpstream(t *testing.T) {
	echo := startEcho(t)
	good := startHTTPUpstream(t, "u", "p@ss")
	bridge, _, _ := startBridge(t, writeFile(t, "http://"+good+":u:p@ss\n"))
	if err := roundTrip(t, bridge, echo, "http ok"); err != nil {
		t.Fatal(err)
	}
}

func TestTargetErrorKeepsProxy(t *testing.T) {
	good := startSocksUpstream(t, "u", "p")
	bridge, pool, stats := startBridge(t, writeFile(t, good+":u:p\n"))
	err := roundTrip(t, bridge, deadAddr(t), "x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if pool.current().Status() == statusDead || stats.targetFails.Load() != 1 {
		t.Fatalf("proxy marked down because of the site: %v", err)
	}
}

func TestHealthCheckRevives(t *testing.T) {
	echo := startEcho(t)
	good := startSocksUpstream(t, "u", "p")
	_, pool, _ := startBridge(t, writeFile(t, deadAddr(t)+":u:p\n"+good+":u:p\n"))
	pool.checkTarget = echo
	pool.CheckAll(context.Background())
	list, _ := pool.snapshot()
	if list[0].Status() != statusDead || list[1].Status() != statusAlive || pool.current() != list[1] {
		t.Fatalf("statuses %v %v, active %s", list[0].Status(), list[1].Status(), pool.current().Addr)
	}
}

func TestReload(t *testing.T) {
	good := startSocksUpstream(t, "u", "p")
	file := writeFile(t, good+":u:p\n")
	_, pool, _ := startBridge(t, file)
	first := pool.current()
	first.conns.Add(7)
	time.Sleep(10 * time.Millisecond)
	os.WriteFile(file, []byte("1.1.1.1:1:a:b\n"+good+":u:p\n"), 0o600)
	changed, _ := pool.Load()
	list, _ := pool.snapshot()
	if !changed || len(list) != 2 || list[1] != first || pool.current() != first {
		t.Fatal("reload lost stats or the active proxy")
	}
}

func TestDropWhenProxyDies(t *testing.T) {
	echo := startEcho(t)
	first, kill := startKillableSocks(t, "u", "p")
	second := startSocksUpstream(t, "u", "p")
	bridge, pool, stats := startBridge(t, writeFile(t, first+":u:p\n"+second+":u:p\n"))
	pool.checkTarget = echo

	c, err := net.Dial("tcp", bridge)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := socks5Connect(c, "", "", echo); err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "ping")
	io.ReadFull(c, make([]byte, 4))
	kill()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	io.ReadAll(c) // the proxy side closed the connection

	deadline := time.Now().Add(5 * time.Second)
	for stats.drops.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if stats.drops.Load() != 1 {
		t.Fatalf("drop not counted, drops=%d", stats.drops.Load())
	}
	if pool.current().Addr != second {
		t.Fatalf("did not switch to the second proxy, active %s", pool.current().Addr)
	}
	if err := roundTrip(t, bridge, echo, "still works"); err != nil {
		t.Fatal(err)
	}
}

func TestNormalCloseIsNotDrop(t *testing.T) {
	// the site closes the connection after answering, the proxy is alive
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "bye")
			c.Close()
		}
	}()
	good := startSocksUpstream(t, "u", "p")
	bridge, pool, stats := startBridge(t, writeFile(t, good+":u:p\n"))
	pool.checkTarget = ln.Addr().String()
	c, err := net.Dial("tcp", bridge)
	if err != nil {
		t.Fatal(err)
	}
	if err := socks5Connect(c, "", "", ln.Addr().String()); err != nil {
		t.Fatal(err)
	}
	io.ReadAll(c)
	c.Close()
	time.Sleep(300 * time.Millisecond)
	if stats.drops.Load() != 0 || pool.current().Status() != statusAlive {
		t.Fatalf("normal close counted as a drop: drops=%d", stats.drops.Load())
	}
}

// startHTTPBridge starts the local HTTP entry on top of the same pool.
func startHTTPBridge(t *testing.T, proxiesFile string) (string, *Pool, *Stats) {
	_, pool, stats := startBridge(t, proxiesFile)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go (&HTTPServer{Server{pool: pool, stats: stats}}).Serve(ln)
	return ln.Addr().String(), pool, stats
}

func TestHTTPEntryConnect(t *testing.T) {
	echo := startEcho(t)
	good := startSocksUpstream(t, "u", "p")
	bridge, _, stats := startHTTPBridge(t, writeFile(t, good+":u:p\n"))

	c, err := net.DialTimeout("tcp", bridge, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	c, err = httpConnect(c, "", "", echo)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(c, "via the http entry")
	got := make([]byte, len("via the http entry"))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != "via the http entry" {
		t.Fatalf("got %q err %v", got, err)
	}
	if stats.totalConns.Load() != 1 {
		t.Fatalf("connection not counted: %d", stats.totalConns.Load())
	}
}

func TestHTTPEntryPlainRequest(t *testing.T) {
	var gotURI, gotProxyAuth string
	site := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI, gotProxyAuth = r.RequestURI, r.Header.Get("Proxy-Authorization")
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%s %s", r.Method, body)
	})}
	sln, _ := net.Listen("tcp", "127.0.0.1:0")
	go site.Serve(sln)
	t.Cleanup(func() { site.Close() })

	good := startSocksUpstream(t, "u", "p")
	bridge, _, _ := startHTTPBridge(t, writeFile(t, good+":u:p\n"))

	proxyURL, _ := url.Parse("http://x:y@" + bridge)
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	for i := 0; i < 2; i++ { // the second request checks that keep-alive does not break the client
		resp, err := client.Post("http://"+sln.Addr().String()+"/path?q=1", "text/plain", strings.NewReader("body"))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) != "POST body" {
			t.Fatalf("response %q", body)
		}
	}
	if gotURI != "/path?q=1" || gotProxyAuth != "" {
		t.Fatalf("site got uri=%q proxy-auth=%q", gotURI, gotProxyAuth)
	}
}

func TestHTTPEntryTargetDown(t *testing.T) {
	good := startSocksUpstream(t, "u", "p")
	bridge, pool, _ := startHTTPBridge(t, writeFile(t, good+":u:p\n"))
	c, err := net.DialTimeout("tcp", bridge, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	_, err = httpConnect(c, "", "", deadAddr(t))
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("want 502, got %v", err)
	}
	if pool.current().Status() == statusDead {
		t.Fatal("proxy marked down because of the site")
	}
}
