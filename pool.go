package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

type proxyStatus int32

const (
	statusUnknown proxyStatus = iota
	statusAlive
	statusDead
)

func (s proxyStatus) String() string {
	switch s {
	case statusAlive:
		return "alive"
	case statusDead:
		return "dead"
	}
	return "unknown"
}

var errNoProxies = errors.New("proxy list is empty")

type Proxy struct {
	Scheme string // socks5, http, https
	Addr   string // host:port
	User   string
	Pass   string

	mu        sync.Mutex
	status    proxyStatus
	lastErr   string
	lastErrAt time.Time
	lastCheck time.Time
	latency   time.Duration

	probeMu sync.Mutex
	probeAt time.Time
	probeOK bool

	conns     atomic.Int64
	active    atomic.Int64
	up        atomic.Int64
	down      atomic.Int64
	dialFails atomic.Int64
	drops     atomic.Int64
}

func (p *Proxy) key() string {
	return p.Scheme + "://" + p.User + ":" + p.Pass + "@" + p.Addr
}

// Label is the name for logs and the dashboard, without the password.
func (p *Proxy) Label() string {
	if p.User != "" {
		return p.Addr + " (" + p.User + ")"
	}
	return p.Addr
}

func (p *Proxy) Status() proxyStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// parseProxyList parses the file: proxies separated by newlines, commas,
// semicolons or spaces. Lines starting with # are skipped.
func parseProxyList(data, defScheme string) ([]*Proxy, []string) {
	var out []*Proxy
	var bad []string
	seen := map[string]bool{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		items := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == ';' || unicode.IsSpace(r)
		})
		for _, item := range items {
			p, err := parseProxy(item, defScheme)
			if err != nil {
				bad = append(bad, fmt.Sprintf("%s: %v", maskProxy(item), err))
				continue
			}
			if seen[p.key()] {
				continue
			}
			seen[p.key()] = true
			out = append(out, p)
		}
	}
	return out, bad
}

// parseProxy accepts [scheme://]ip:port[:login:pass]. The password may contain colons.
func parseProxy(s, defScheme string) (*Proxy, error) {
	scheme := defScheme
	if i := strings.Index(s, "://"); i >= 0 {
		scheme = strings.ToLower(s[:i])
		s = s[i+3:]
	}
	switch scheme {
	case "socks5", "socks5h", "socks":
		scheme = "socks5"
	case "http", "https":
	default:
		return nil, fmt.Errorf("unknown type %q", scheme)
	}
	parts := strings.SplitN(s, ":", 4)
	p := &Proxy{Scheme: scheme}
	switch len(parts) {
	case 2:
	case 4:
		p.User, p.Pass = parts[2], parts[3]
	default:
		return nil, errors.New("expected ip:port:login:pass or ip:port")
	}
	if parts[0] == "" {
		return nil, errors.New("empty address")
	}
	port, err := strconv.Atoi(parts[1])
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid port")
	}
	p.Addr = net.JoinHostPort(parts[0], parts[1])
	return p, nil
}

func maskProxy(s string) string {
	parts := strings.SplitN(s, ":", 4)
	if len(parts) == 4 {
		return strings.Join(parts[:3], ":") + ":***"
	}
	return s
}

type Pool struct {
	stats       *Stats
	path        string
	defScheme   string
	dialTimeout time.Duration
	checkTarget string
	maxAttempts int

	mu      sync.RWMutex
	proxies []*Proxy
	active  int

	fileSig string // changed only by Load
	allDead atomic.Bool
	checkMu sync.Mutex
}

// Load re-reads the file if it changed. Proxies that stay in the list
// keep their stats.
func (p *Pool) Load() (bool, error) {
	st, err := os.Stat(p.path)
	if err != nil {
		if p.fileSig != "missing" {
			p.fileSig = "missing"
			p.replace(nil)
			p.stats.Event(levelWarn, "File %s not found, proxy list is empty", p.path)
			return true, nil
		}
		return false, nil
	}
	sig := fmt.Sprintf("%d-%d", st.ModTime().UnixNano(), st.Size())
	if sig == p.fileSig {
		return false, nil
	}
	data, err := os.ReadFile(p.path)
	if err != nil {
		return false, err
	}
	p.fileSig = sig
	list, bad := parseProxyList(string(data), p.defScheme)
	for _, b := range bad {
		p.stats.Event(levelWarn, "Skipped proxy %s", b)
	}
	p.replace(list)
	p.stats.Event(levelInfo, "Loaded proxies: %d (%s)", len(list), p.path)
	return true, nil
}

func (p *Pool) replace(list []*Proxy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := map[string]*Proxy{}
	for _, x := range p.proxies {
		old[x.key()] = x
	}
	activeKey := ""
	if p.active < len(p.proxies) {
		activeKey = p.proxies[p.active].key()
	}
	p.active = 0
	for i, x := range list {
		if o, ok := old[x.key()]; ok {
			list[i] = o
		}
		if x.key() == activeKey {
			p.active = i
		}
	}
	p.proxies = list
}

// name labels a proxy in events: position in the list, address and login.
func (p *Pool) name(px *Proxy) string {
	list, _ := p.snapshot()
	for i, x := range list {
		if x == px {
			return fmt.Sprintf("#%d %s", i+1, px.Label())
		}
	}
	return px.Label()
}

func (p *Pool) snapshot() ([]*Proxy, int) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.proxies, p.active
}

func (p *Pool) current() *Proxy {
	list, active := p.snapshot()
	if active < len(list) {
		return list[active]
	}
	return nil
}

func (p *Pool) setActive(px *Proxy, reason string, failover bool) bool {
	p.mu.Lock()
	idx := -1
	for i, x := range p.proxies {
		if x == px {
			idx = i
		}
	}
	if idx < 0 || idx == p.active {
		p.mu.Unlock()
		return false
	}
	from := "-"
	if p.active < len(p.proxies) {
		from = fmt.Sprintf("#%d %s", p.active+1, p.proxies[p.active].Label())
	}
	p.active = idx
	p.mu.Unlock()
	if failover {
		p.stats.failovers.Add(1)
	}
	p.stats.Event(levelWarn, "Active proxy: %s → #%d %s (%s)", from, idx+1, px.Label(), reason)
	return true
}

func (p *Pool) markDead(px *Proxy, err error) {
	px.mu.Lock()
	prev := px.status
	px.status = statusDead
	px.lastErr = err.Error()
	px.lastErrAt = time.Now()
	px.mu.Unlock()
	if prev != statusDead {
		p.stats.Event(levelError, "Proxy %s is down: %v", p.name(px), err)
	}

	list, active := p.snapshot()
	if active >= len(list) || list[active] != px {
		return
	}
	for i := 1; i < len(list); i++ {
		c := list[(active+i)%len(list)]
		if c.Status() != statusDead {
			p.setActive(c, "previous one failed", true)
			return
		}
	}
	if !p.allDead.Swap(true) {
		p.stats.Event(levelError, "All proxies are down")
	}
}

func (p *Pool) markAlive(px *Proxy, latency time.Duration) {
	px.mu.Lock()
	prev := px.status
	px.status = statusAlive
	if latency > 0 {
		px.latency = latency
	}
	px.mu.Unlock()
	p.allDead.Store(false)
	if prev == statusDead {
		p.stats.Event(levelInfo, "Proxy %s is back up", p.name(px))
	}
}

// ensureActive moves off a dead active proxy to the next alive one in the list.
func (p *Pool) ensureActive() {
	list, active := p.snapshot()
	if active >= len(list) || list[active].Status() != statusDead {
		return
	}
	for i := 1; i < len(list); i++ {
		c := list[(active+i)%len(list)]
		if c.Status() == statusAlive {
			p.setActive(c, "previous one is down", true)
			return
		}
	}
}

// Dial connects to target through the active proxy. If the proxy fails, it tries
// the next ones in the list: alive ones first, then the ones marked down.
func (p *Pool) Dial(ctx context.Context, target string) (net.Conn, *Proxy, error) {
	list, active := p.snapshot()
	if len(list) == 0 {
		return nil, nil, errNoProxies
	}
	var order, dead []*Proxy
	for i := range list {
		px := list[(active+i)%len(list)]
		if px.Status() == statusDead {
			dead = append(dead, px)
		} else {
			order = append(order, px)
		}
	}
	order = append(order, dead...)
	if p.maxAttempts > 0 && len(order) > p.maxAttempts {
		order = order[:p.maxAttempts]
	}

	var lastErr error
	for _, px := range order {
		if ctx.Err() != nil {
			break
		}
		conn, err := dialVia(ctx, px, target, p.dialTimeout)
		if err == nil {
			p.markAlive(px, 0)
			if cur := p.current(); cur != px && (cur == nil || cur.Status() == statusDead) {
				p.setActive(px, "previous one is down", true)
			}
			return conn, px, nil
		}
		var te *targetError
		if errors.As(err, &te) {
			// The proxy is alive, the problem is on the site's side.
			p.stats.targetFails.Add(1)
			return nil, px, err
		}
		px.dialFails.Add(1)
		p.stats.dialFails.Add(1)
		p.markDead(px, err)
		lastErr = err
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return nil, nil, lastErr
}

const probeEvery = 10 * time.Second

// Probe checks one proxy at most once per probeEvery; other callers
// wait and get the latest result.
func (p *Pool) Probe(px *Proxy) bool {
	px.probeMu.Lock()
	defer px.probeMu.Unlock()
	if time.Since(px.probeAt) < probeEvery {
		return px.probeOK
	}
	ok := p.checkOne(context.Background(), px)
	px.probeAt, px.probeOK = time.Now(), ok
	return ok
}

func (p *Pool) checkOne(ctx context.Context, px *Proxy) bool {
	t0 := time.Now()
	c, err := dialVia(ctx, px, p.checkTarget, p.dialTimeout)
	px.mu.Lock()
	px.lastCheck = time.Now()
	px.mu.Unlock()
	if err != nil {
		p.markDead(px, err)
		return false
	}
	c.Close()
	p.markAlive(px, time.Since(t0))
	return true
}

// CheckAll checks every proxy by connecting to checkTarget.
func (p *Pool) CheckAll(ctx context.Context) {
	if !p.checkMu.TryLock() {
		return
	}
	defer p.checkMu.Unlock()
	list, _ := p.snapshot()
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, px := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(px *Proxy) {
			defer wg.Done()
			defer func() { <-sem }()
			p.checkOne(ctx, px)
		}(px)
	}
	wg.Wait()
	p.ensureActive()
}

func (p *Pool) Run(ctx context.Context, checkEvery, watchEvery time.Duration) {
	go p.CheckAll(ctx)
	tc := time.NewTicker(checkEvery)
	tw := time.NewTicker(watchEvery)
	defer tc.Stop()
	defer tw.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tc.C:
			go p.CheckAll(ctx)
		case <-tw.C:
			changed, err := p.Load()
			if err != nil {
				p.stats.Event(levelError, "Cannot read %s: %v", p.path, err)
			}
			if changed {
				go p.CheckAll(ctx)
			}
		}
	}
}
