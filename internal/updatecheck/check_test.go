package updatecheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func fixture(current string, transport roundTripFunc) (*Checker, func(time.Duration)) {
	c := New(current)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	c.now = func() time.Time { return time.Unix(0, clock.Load()) }
	c.client.Transport = transport
	return c, func(d time.Duration) { clock.Add(int64(d)) }
}
func check(t *testing.T, c *Checker, manual bool) Result {
	t.Helper()
	value, err := c.Check(context.Background(), manual)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestVersionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		latest, current string
		want            bool
	}{
		{"v5.10.0", "5.9.0", true}, {"v6.0.0", "5.99.99", true},
		{"5.9.1", "v5.9.0", true}, {"v5.9.0", "5.9.0", false},
		{"5.8.99", "5.9.0", false}, {"5.9.0+new", "5.9.0+old", false},
		{"5.10.0+build.3", "5.9.0", true}, {"5.10.0-rc.1", "5.9.0", false},
		{"5.10.0", "dev", false}, {"5.010.0", "5.9.0", false},
		{"999999999999999999999.0.0", "99999999999999999999.0.0", true},
		{"https://evil.example/6.0.0", "5.9.0", false}, {"v6.0.0\n", "5.9.0", false},
	} {
		t.Run(tc.latest+"/"+tc.current, func(t *testing.T) {
			if got := newer(tc.latest, tc.current); got != tc.want {
				t.Fatalf("newer = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReleaseCheckCacheAndConditionalRequest(t *testing.T) {
	var calls atomic.Int32
	c, advance := fixture("5.9.0", func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != releasesAPI || r.Method != "GET" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		for _, key := range []string{"Authorization", "Cookie", "Referer"} {
			if r.Header.Get(key) != "" {
				t.Errorf("leaked %s", key)
			}
		}
		if r.Header.Get("User-Agent") != "PairRoom-update-check" {
			t.Error("missing fixed user agent")
		}
		if calls.Add(1) == 1 {
			if r.Header.Get("If-None-Match") != "" {
				t.Error("unexpected validator")
			}
			res := response(200, `{"tag_name":"v5.10.0","html_url":"https://evil.example","body":"<script>bad</script>"}`)
			res.Header.Set("ETag", `"release-1"`)
			return res, nil
		}
		if r.Header.Get("If-None-Match") != `"release-1"` {
			t.Error("missing conditional validator")
		}
		return response(304, ""), nil
	})
	first := check(t, c, false)
	if first.Status != "available" || first.LatestVersion != "5.10.0" || first.ReleaseURL != releasesURL+"v5.10.0" {
		t.Fatalf("unexpected result: %+v", first)
	}
	if got := check(t, c, false); got != first {
		t.Fatalf("cache changed: %+v", got)
	}
	check(t, c, true)
	if calls.Load() != 1 {
		t.Fatal("manual check bypassed its floor")
	}
	advance(time.Minute)
	if got := check(t, c, true); got.Status != "available" || got.CheckedAt == first.CheckedAt {
		t.Fatalf("bad 304 result: %+v", got)
	}
	if calls.Load() != 2 {
		t.Fatal("manual refresh did not revalidate")
	}
	advance(checkInterval)
	check(t, c, false)
	if calls.Load() != 3 {
		t.Fatal("automatic cache did not expire")
	}
}

func TestEqualAndAheadVersionsDoNotSuggestDowngrade(t *testing.T) {
	for _, current := range []string{"5.10.0", "6.0.0", "5.10.0+local.42"} {
		c, _ := fixture(current, func(*http.Request) (*http.Response, error) { return response(200, `{"tag_name":"v5.10.0"}`), nil })
		if got := check(t, c, false); got.Status != "current" {
			t.Fatalf("%s: %+v", current, got)
		}
	}
}

func TestFailuresAreNotReportedAsCurrent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"no releases", 404, "not found"}, {"server error", 500, "secret remote error"},
		{"redirect", 302, ""}, {"unexpected 304", 304, ""},
		{"invalid JSON", 200, "<html>offline</html>"}, {"empty", 200, `{}`},
		{"trailing JSON", 200, `{"tag_name":"v6.0.0"}{}`},
		{"draft", 200, `{"tag_name":"v6.0.0","draft":true}`},
		{"prerelease flag", 200, `{"tag_name":"v6.0.0","prerelease":true}`},
		{"prerelease tag", 200, `{"tag_name":"v6.0.0-beta.1"}`},
		{"bad tag", 200, `{"tag_name":"../6.0.0"}`},
		{"oversize", 200, strings.Repeat(" ", maxResponse+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c, advance := fixture("5.9.0", func(*http.Request) (*http.Response, error) { calls.Add(1); return response(tc.status, tc.body), nil })
			got := check(t, c, false)
			if got.Status != "unavailable" || got.ReleaseURL != "" || got.LatestVersion != "" {
				t.Fatalf("bad failure result: %+v", got)
			}
			advance(failureInterval - time.Second)
			check(t, c, false)
			if calls.Load() != 1 {
				t.Fatal("failure was not cached")
			}
			advance(time.Second)
			check(t, c, false)
			if calls.Load() != 2 {
				t.Fatal("failure cache never expired")
			}
		})
	}
}

func TestUnsupportedBuildDoesNotContactGitHub(t *testing.T) {
	c, _ := fixture("dev", func(*http.Request) (*http.Response, error) {
		t.Error("unsupported version contacted network")
		return nil, errors.New("unexpected")
	})
	if got := check(t, c, true); got.Status != "unsupported" {
		t.Fatal(got)
	}
}

func TestWaiterCancellationDoesNotCancelSharedCheck(t *testing.T) {
	started, releaseRequest := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	c, _ := fixture("5.9.0", func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-releaseRequest:
			return response(200, `{"tag_name":"v5.10.0"}`), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { _, err := c.Check(ctx, false); firstDone <- err }()
	<-started
	cancel()
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := check(t, c, false); got.Status != "available" {
				t.Errorf("waiter: %+v", got)
			}
		}()
	}
	close(releaseRequest)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("%d requests, want one", calls.Load())
	}
}

func TestCancelledCallerDoesNotStartWork(t *testing.T) {
	c, _ := fixture("5.9.0", func(*http.Request) (*http.Response, error) {
		t.Error("cancelled caller contacted network")
		return nil, errors.New("unexpected")
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Check(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestTransportFailureAndRecovery(t *testing.T) {
	var calls atomic.Int32
	c, advance := fixture("5.9.0", func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, context.DeadlineExceeded
		}
		return response(200, `{"tag_name":"v5.10.0"}`), nil
	})
	if got := check(t, c, false); got.Status != "unavailable" {
		t.Fatal(got)
	}
	advance(time.Minute)
	if got := check(t, c, true); got.Status != "available" {
		t.Fatal(got)
	}
	if err := c.client.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatal("redirects are not rejected")
	}
	if c.client.Timeout != 5*time.Second {
		t.Fatal("missing request timeout")
	}
}

func TestRateLimitsAlsoConstrainManualChecks(t *testing.T) {
	var calls atomic.Int32
	c, advance := fixture("5.9.0", func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		res := response(429, "do not expose this")
		res.Header.Set("Retry-After", "3600")
		return res, nil
	})
	check(t, c, true)
	advance(59 * time.Minute)
	check(t, c, true)
	if calls.Load() != 1 {
		t.Fatal("manual check ignored rate limit")
	}
	advance(time.Minute)
	check(t, c, true)
	if calls.Load() != 2 {
		t.Fatal("rate limit never expired")
	}
	for _, tc := range []struct {
		header http.Header
		want   time.Duration
	}{
		{http.Header{"Retry-After": {"9999999999"}}, 24 * time.Hour},
		{http.Header{"Retry-After": {"-1"}}, 15 * time.Minute},
		{http.Header{"Retry-After": {c.now().Add(2 * time.Hour).UTC().Format(http.TimeFormat)}}, 2 * time.Hour},
		{http.Header{"X-Ratelimit-Reset": {fmt.Sprint(c.now().Add(3 * time.Hour).Unix())}}, 3 * time.Hour},
	} {
		if got := rateLimitUntil(tc.header, c.now()).Sub(c.now()); got != tc.want {
			t.Fatalf("retry delay %s, want %s", got, tc.want)
		}
	}
}

func TestHTTPBoundaryDoesNotForwardCredentials(t *testing.T) {
	c, _ := fixture("5.9.0", func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != releasesAPI || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("forwarded caller input")
		}
		return response(200, `{"tag_name":"v5.10.0"}`), nil
	})
	req := httptest.NewRequest("GET", Endpoint+"?refresh=1&url=https://evil.example", nil)
	req.Header.Set("Authorization", "Bearer PRIVATE")
	req.Header.Set("Cookie", "private=session")
	w := httptest.NewRecorder()
	c.Handler().ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"status":"available"`) {
		t.Fatalf("response: %d %s", w.Code, w.Body.String())
	}
	for _, method := range []string{"POST", "HEAD", "DELETE"} {
		w := httptest.NewRecorder()
		c.Handler().ServeHTTP(w, httptest.NewRequest(method, Endpoint, nil))
		if w.Code != 405 {
			t.Fatalf("%s: %d", method, w.Code)
		}
	}
}
