package ui

import (
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	maxFailures   = 10
	failureWindow = 15 * time.Minute
	freeAttempts  = 5
	maxBackoff    = 15 * time.Minute
)

var trustedProxies = func() []netip.Prefix {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(os.Getenv("ARGUS_TRUSTED_PROXIES"), func(r rune) bool { return r == ',' || r == ' ' }) {
		if p, err := netip.ParsePrefix(f); err == nil {
			out = append(out, p)
		} else if a, err := netip.ParseAddr(f); err == nil {
			out = append(out, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return out
}()

func trusted(ip string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && slices.ContainsFunc(trustedProxies, func(p netip.Prefix) bool { return p.Contains(a.Unmap()) })
}

type LoginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
	slots    chan struct{}
}

func newLoginLimiter() *LoginLimiter {
	return &LoginLimiter{failures: map[string][]time.Time{}, slots: make(chan struct{}, 4)}
}

func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if !trusted(host) {
		return host
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		h := strings.TrimSpace(hops[i])
		if _, err := netip.ParseAddr(h); err != nil {
			break
		}
		if host = h; !trusted(h) {
			break
		}
	}
	return host
}

func accountKey(org, user string) string { return "account:" + org + "/" + strings.ToLower(user) }

func (l *LoginLimiter) backoff(key string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.recent(key)
	if len(ts) < freeAttempts {
		return 0
	}
	d := min(time.Second<<min(len(ts)-freeAttempts, 20), maxBackoff)
	return time.Until(ts[len(ts)-1].Add(d))
}

func (l *LoginLimiter) recent(ip string) []time.Time {
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

func (l *LoginLimiter) Blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) >= maxFailures
}

func (l *LoginLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.failures) > 4096 {
		for k := range l.failures {
			l.recent(k)
		}
	}
	l.failures[ip] = append(l.recent(ip), time.Now())
}

func (l *LoginLimiter) Succeed(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}
