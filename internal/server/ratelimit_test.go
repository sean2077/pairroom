package server

import (
	"testing"
	"time"
)

func TestRateLimiterWindow(t *testing.T) {
	now := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	limiter := newRateLimiter()
	limiter.window = time.Minute
	limiter.burst = 2
	limiter.now = func() time.Time { return now }
	if allowed, _ := limiter.allow("client"); !allowed {
		t.Fatal("first request was rejected")
	}
	if allowed, _ := limiter.allow("client"); !allowed {
		t.Fatal("second request was rejected")
	}
	if allowed, retry := limiter.allow("client"); allowed || retry <= 0 {
		t.Fatalf("third request should be limited: allowed=%v retry=%s", allowed, retry)
	}
	now = now.Add(time.Minute)
	if allowed, _ := limiter.allow("client"); !allowed {
		t.Fatal("request remained limited after window reset")
	}
}
