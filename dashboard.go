package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

type proxyView struct {
	Index       int        `json:"index"`
	Scheme      string     `json:"scheme"`
	Addr        string     `json:"addr"`
	User        string     `json:"user"`
	Status      string     `json:"status"`
	Active      bool       `json:"active"`
	LatencyMs   int64      `json:"latencyMs"`
	LastError   string     `json:"lastError"`
	LastErrorAt *time.Time `json:"lastErrorAt"`
	LastCheck   *time.Time `json:"lastCheck"`
	Conns       int64      `json:"conns"`
	ActiveConns int64      `json:"activeConns"`
	Up          int64      `json:"up"`
	Down        int64      `json:"down"`
	DialFails   int64      `json:"dialFails"`
	Drops       int64      `json:"drops"`
}

type statsView struct {
	Listen      string      `json:"listen"`
	HTTPListen  string      `json:"httpListen"`
	ProxiesFile string      `json:"proxiesFile"`
	UptimeSec   int64       `json:"uptimeSec"`
	TotalConns  int64       `json:"totalConns"`
	ActiveConns int64       `json:"activeConns"`
	BytesUp     int64       `json:"bytesUp"`
	BytesDown   int64       `json:"bytesDown"`
	DialFails   int64       `json:"dialFails"`
	TargetFails int64       `json:"targetFails"`
	Drops       int64       `json:"drops"`
	Failovers   int64       `json:"failovers"`
	Proxies     []proxyView `json:"proxies"`
	Events      []Event     `json:"events"`
	Series      []Sample    `json:"series"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func dashboardHandler(ctx context.Context, listen, httpListen string, pool *Pool, stats *Stats) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		list, active := pool.snapshot()
		v := statsView{
			Listen:      listen,
			HTTPListen:  httpListen,
			ProxiesFile: pool.path,
			UptimeSec:   int64(time.Since(stats.start).Seconds()),
			TotalConns:  stats.totalConns.Load(),
			ActiveConns: stats.activeConns.Load(),
			BytesUp:     stats.bytesUp.Load(),
			BytesDown:   stats.bytesDown.Load(),
			DialFails:   stats.dialFails.Load(),
			TargetFails: stats.targetFails.Load(),
			Drops:       stats.drops.Load(),
			Failovers:   stats.failovers.Load(),
			Proxies:     []proxyView{},
			Events:      stats.Events(),
			Series:      stats.Series(),
		}
		for i, px := range list {
			px.mu.Lock()
			pv := proxyView{
				Index:       i,
				Scheme:      px.Scheme,
				Addr:        px.Addr,
				User:        px.User,
				Status:      px.status.String(),
				Active:      i == active,
				LatencyMs:   px.latency.Milliseconds(),
				LastError:   px.lastErr,
				LastErrorAt: timePtr(px.lastErrAt),
				LastCheck:   timePtr(px.lastCheck),
			}
			px.mu.Unlock()
			pv.Conns = px.conns.Load()
			pv.ActiveConns = px.active.Load()
			pv.Up = px.up.Load()
			pv.Down = px.down.Load()
			pv.DialFails = px.dialFails.Load()
			pv.Drops = px.drops.Load()
			v.Proxies = append(v.Proxies, pv)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("POST /api/switch", func(w http.ResponseWriter, r *http.Request) {
		idx, err := strconv.Atoi(r.URL.Query().Get("index"))
		list, _ := pool.snapshot()
		if err != nil || idx < 0 || idx >= len(list) {
			http.Error(w, "bad index", http.StatusBadRequest)
			return
		}
		pool.setActive(list[idx], "set from the dashboard", false)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/check", func(w http.ResponseWriter, r *http.Request) {
		go pool.CheckAll(ctx)
		w.WriteHeader(http.StatusAccepted)
	})
	return mux
}
