package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultRateWindow = time.Minute
	defaultRateBurst  = 600
)

type rateBucket struct {
	windowStart time.Time
	count       int
	lastSeen    time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]rateBucket
	window  time.Duration
	burst   int
	now     func() time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{buckets: make(map[string]rateBucket), window: defaultRateWindow, burst: defaultRateBurst, now: time.Now}
}

func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.buckets[key]
	if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) >= l.window {
		bucket.windowStart = now
		bucket.count = 0
	}
	bucket.lastSeen = now
	if bucket.count >= l.burst {
		remaining := l.window - now.Sub(bucket.windowStart)
		if remaining < time.Second {
			remaining = time.Second
		}
		l.buckets[key] = bucket
		return false, remaining
	}
	bucket.count++
	l.buckets[key] = bucket
	if len(l.buckets) > 2048 {
		for candidate, value := range l.buckets {
			if now.Sub(value.lastSeen) > 2*l.window {
				delete(l.buckets, candidate)
			}
		}
	}
	return true, 0
}

func requestClientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil && host != "" {
		return host
	}
	if value := strings.TrimSpace(r.RemoteAddr); value != "" {
		return value
	}
	return "unknown"
}
