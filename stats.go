package main

import (
	"context"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const (
	levelInfo  = "info"
	levelWarn  = "warn"
	levelError = "error"

	maxEvents = 300
	seriesLen = 300 // seconds of throughput history (5 minutes)
)

type Event struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

// Sample is the throughput of one second, in bytes per second.
type Sample struct {
	T    int64 `json:"t"` // unix ms
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

type Stats struct {
	start time.Time

	totalConns  atomic.Int64
	activeConns atomic.Int64
	bytesUp     atomic.Int64
	bytesDown   atomic.Int64
	dialFails   atomic.Int64 // proxy did not answer or rejected auth
	targetFails atomic.Int64 // proxy is alive but could not reach the site
	drops       atomic.Int64 // connection broke on the proxy side
	failovers   atomic.Int64 // automatic proxy switches

	mu     sync.Mutex
	events []Event
	series []Sample
}

func NewStats() *Stats {
	return &Stats{start: time.Now()}
}

func (s *Stats) Event(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%-5s %s", level, msg)
	s.mu.Lock()
	s.events = append(s.events, Event{Time: time.Now(), Level: level, Msg: msg})
	if len(s.events) > maxEvents {
		n := copy(s.events, s.events[len(s.events)-maxEvents:])
		s.events = s.events[:n]
	}
	s.mu.Unlock()
}

// Events returns events, newest first.
func (s *Stats) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	for i, e := range s.events {
		out[len(s.events)-1-i] = e
	}
	return out
}

func (s *Stats) Series() []Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Sample(nil), s.series...)
}

func (s *Stats) RunSampler(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	prevUp, prevDown := s.bytesUp.Load(), s.bytesDown.Load()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			up, down := s.bytesUp.Load(), s.bytesDown.Load()
			smp := Sample{T: now.UnixMilli(), Up: up - prevUp, Down: down - prevDown}
			prevUp, prevDown = up, down
			s.mu.Lock()
			s.series = append(s.series, smp)
			if len(s.series) > seriesLen {
				n := copy(s.series, s.series[len(s.series)-seriesLen:])
				s.series = s.series[:n]
			}
			s.mu.Unlock()
		}
	}
}
