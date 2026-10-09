package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	maxFailures   = 10
	failureWindow = 15 * time.Minute
)

type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	slots    chan struct{}
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string][]time.Time{}, slots: make(chan struct{}, 4)}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (l *loginLimiter) recent(ip string) []time.Time {
	cut := time.Now().Add(-failureWindow)
	ts := l.failures[ip]
	for len(ts) > 0 && ts[0].Before(cut) {
		ts = ts[1:]
	}
	if len(ts) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = ts
	}
	return ts
}

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) >= maxFailures
}

func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) > 4096 {
		for k := range l.failures {
			l.recent(k)
		}
	}
	l.failures[ip] = append(l.recent(ip), time.Now())
}

func (l *loginLimiter) succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}
