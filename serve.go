package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	defaultSocks     = "127.0.0.1:1080"
	defaultDashboard = "127.0.0.1:1081"
	defaultHTTP      = "127.0.0.1:1082"
)

const proxiesTemplate = `# codep proxy list. The file is re-read automatically, no restart needed.
#
# Format: ip:port:login:password, one per line or several separated by commas.
# Without auth: ip:port. Add a scheme to set the type for one line:
#   socks5://1.2.3.4:1080:login:password
#   http://1.2.3.4:3128:login:password     (HTTP CONNECT)
#   https://1.2.3.4:443:login:password     (HTTP CONNECT over TLS)
# Lines without a scheme use the -type flag (socks5 by default).
# Lines starting with # are ignored.

`

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", defaultSocks, "local SOCKS5 address")
	httpListen := fs.String("http-listen", defaultHTTP, "local HTTP proxy address, empty disables it")
	dash := fs.String("dashboard", defaultDashboard, "dashboard address, empty disables it")
	file := fs.String("proxies", defaultProxiesPath(), "proxy list file")
	scheme := fs.String("type", "socks5", "default proxy type: socks5, http, https")
	dialTimeout := fs.Duration("dial-timeout", 10*time.Second, "timeout for connecting through one proxy")
	checkEvery := fs.Duration("check-interval", 30*time.Second, "how often to health-check all proxies")
	checkTarget := fs.String("check-target", "1.1.1.1:443", "address to connect to when checking a proxy")
	attempts := fs.Int("attempts", 5, "how many proxies to try for one connection")
	fs.Parse(args)

	switch *scheme {
	case "socks5", "http", "https":
	default:
		return fmt.Errorf("unknown -type %q", *scheme)
	}
	path, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	if err := ensureProxiesFile(path); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stats := NewStats()
	pool := &Pool{
		stats:       stats,
		path:        path,
		defScheme:   *scheme,
		dialTimeout: *dialTimeout,
		checkTarget: *checkTarget,
		maxAttempts: *attempts,
	}
	if _, err := pool.Load(); err != nil {
		stats.Event(levelError, "Cannot read %s: %v", path, err)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", *listen, err)
	}
	defer ln.Close()
	go stats.RunSampler(ctx)
	go pool.Run(ctx, *checkEvery, 3*time.Second)
	go (&Server{pool: pool, stats: stats}).Serve(ln)

	entries := "SOCKS5 on " + *listen
	if *httpListen != "" {
		hln, err := net.Listen("tcp", *httpListen)
		if err != nil {
			return fmt.Errorf("cannot listen on %s for the HTTP proxy: %w", *httpListen, err)
		}
		defer hln.Close()
		go (&HTTPServer{Server{pool: pool, stats: stats}}).Serve(hln)
		entries += ", HTTP proxy on " + *httpListen
	}
	if *dash != "" {
		dln, err := net.Listen("tcp", *dash)
		if err != nil {
			return fmt.Errorf("cannot listen on %s for the dashboard: %w", *dash, err)
		}
		go http.Serve(dln, dashboardHandler(ctx, *listen, *httpListen, pool, stats))
		entries += ", dashboard http://" + *dash
	}
	stats.Event(levelInfo, "codep %s: %s", version, entries)

	<-ctx.Done()
	stats.Event(levelInfo, "Stopping")
	return nil
}

// ensureProxiesFile creates an empty list with format notes on first run.
// The file holds passwords, so it is readable only by the owner.
func ensureProxiesFile(path string) error {
	if _, err := os.Stat(path); err == nil || !os.IsNotExist(err) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(proxiesTemplate), 0o600)
}
