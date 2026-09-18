package server

import (
	"net/http"
	"net/http/httptest"
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

func TestRequestClientKeyIsolatesCredentialsAndEventStreams(t *testing.T) {
	first := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/service", nil)
	first.RemoteAddr = "127.0.0.1:1"
	first.Header.Set("Authorization", "Bearer token-a")
	second := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/service", nil)
	second.RemoteAddr = "127.0.0.1:2"
	second.Header.Set("Authorization", "Bearer token-b")
	if requestClientKey(first) == requestClientKey(second) {
		t.Fatal("distinct credentials shared a rate-limit bucket")
	}
	events := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/api/v1/events", nil)
	events.RemoteAddr = "127.0.0.1:1"
	events.Header.Set("Authorization", "Bearer token-a")
	if requestClientKey(first) == requestClientKey(events) {
		t.Fatal("event stream shared the request bucket")
	}
	if key := requestClientKey(first); key == "127.0.0.1" || key == "token-a" {
		t.Fatalf("rate-limit key leaked the credential value: %q", key)
	}
}
