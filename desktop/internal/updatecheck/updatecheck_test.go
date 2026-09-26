package updatecheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseStableAcceptsOnlyXYZ(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Version
		ok   bool
	}{
		{"5.6.0", Version{5, 6, 0}, true},
		{"v10.0.12", Version{10, 0, 12}, true},
		{"v5.7.0-rc.1", Version{}, false},
		{"5.7.0+build", Version{}, false},
		{"5.7", Version{}, false},
		{"5.7.0.1", Version{}, false},
		{"05.7.0", Version{}, false},
		{"5.-1.0", Version{}, false},
		{"5..0", Version{}, false},
		{"vv5.7.0", Version{}, false},
		{" 5.7.0", Version{}, false},
		{"5.7.0\n", Version{}, false},
		{"5.٣.0", Version{}, false},
		{"1234567890.0.0", Version{}, false},
		{"", Version{}, false},
	} {
		got, ok := ParseStable(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("ParseStable(%q) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestCompareOrdersNumerically(t *testing.T) {
	for _, tc := range []struct {
		a, b Version
		want int
	}{
		{Version{5, 6, 0}, Version{5, 6, 0}, 0},
		{Version{5, 6, 0}, Version{5, 10, 0}, -1},
		{Version{6, 0, 0}, Version{5, 99, 99}, 1},
		{Version{5, 6, 1}, Version{5, 6, 0}, 1},
	} {
		if got := tc.a.Compare(tc.b); got != tc.want {
			t.Errorf("%v.Compare(%v) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDueThrottlesToOncePerInterval(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		last time.Time
		want bool
	}{
		{"never checked", time.Time{}, true},
		{"just checked", now.Add(-time.Minute), false},
		{"23h59m ago", now.Add(-24*time.Hour + time.Minute), false},
		{"exactly 24h ago", now.Add(-24 * time.Hour), true},
		{"clock moved back", now.Add(time.Hour), true},
	} {
		if got := Due(tc.last, now); got != tc.want {
			t.Errorf("%s: Due = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestParseReleaseIgnoresPrereleasesAndMalformedTags(t *testing.T) {
	got, err := ParseRelease(strings.NewReader(`{"tag_name":"v5.7.0","draft":false,"prerelease":false,"html_url":"https://evil.example/"}`))
	if err != nil || got != (Version{5, 7, 0}) {
		t.Fatalf("stable release = %v, %v", got, err)
	}
	for _, body := range []string{
		`{"tag_name":"v5.7.0","prerelease":true}`,
		`{"tag_name":"v5.7.0","draft":true}`,
		`{"tag_name":"v5.7.0-beta.1"}`,
		`{"tag_name":"nightly"}`,
		`{}`,
		`not json`,
		`{"tag_name":7}`,
	} {
		if v, err := ParseRelease(strings.NewReader(body)); err == nil {
			t.Errorf("ParseRelease(%s) = %v, want error", body, v)
		}
	}
}

func TestReleasePageIsDerivedFromVersionNotResponse(t *testing.T) {
	if got := ReleasePage(Version{5, 7, 0}); got != "https://github.com/sean2077/pairroom/releases/tag/v5.7.0" {
		t.Fatalf("release page = %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeGitHub answers in-process; no test opens a socket or reaches the network.
type fakeGitHub struct {
	mu       sync.Mutex
	requests []*http.Request
	status   int
	body     string
	err      error
}

func (f *fakeGitHub) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r)
		if f.err != nil {
			return nil, f.err
		}
		return &http.Response{StatusCode: f.status, Body: io.NopCloser(strings.NewReader(f.body)), Header: http.Header{}, Request: r}, nil
	})}
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func newChecker(t *testing.T, gh *fakeGitHub, now *time.Time) *Checker {
	t.Helper()
	return &Checker{Dir: filepath.Join(t.TempDir(), "PairRoom Desktop"), Current: "5.6.0", Client: gh.client(), Now: func() time.Time { return *now }}
}

func TestDisabledByDefaultAndNeverRequests(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{status: 200, body: `{"tag_name":"v9.0.0"}`}
	c := newChecker(t, gh, &now)
	status, err := c.Status()
	if err != nil || status.Enabled || status.Latest != "" {
		t.Fatalf("default status = %+v, %v", status, err)
	}
	for _, explicit := range []bool{false, true} {
		if _, err := c.Check(context.Background(), explicit); !errors.Is(err, ErrDisabled) {
			t.Fatalf("Check(explicit=%v) while disabled: %v", explicit, err)
		}
	}
	if gh.count() != 0 {
		t.Fatalf("disabled checker sent %d requests", gh.count())
	}
	if _, err := os.Stat(c.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading status created the preference directory: %v", err)
	}
}

func TestEnabledCheckFindsNewerReleaseAndThrottles(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{status: 200, body: `{"tag_name":"v5.7.0","prerelease":false}`}
	c := newChecker(t, gh, &now)
	var changes []Status
	c.OnChange = func(s Status) { changes = append(changes, s) }
	if _, err := c.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if gh.count() != 0 {
		t.Fatal("enabling must not itself send a request")
	}
	status, err := c.Check(context.Background(), false)
	if err != nil || status.Latest != "5.7.0" || status.URL != "https://github.com/sean2077/pairroom/releases/tag/v5.7.0" || !status.LastCheck.Equal(now) {
		t.Fatalf("status = %+v, %v", status, err)
	}
	request := gh.requests[0]
	if request.Method != http.MethodGet || request.URL.String() != Endpoint {
		t.Fatalf("request = %s %s", request.Method, request.URL)
	}
	if ua := request.Header.Get("User-Agent"); ua != "PairRoom-Desktop/5.6.0" {
		t.Fatalf("User-Agent = %q", ua)
	}
	for name := range request.Header {
		if name != "User-Agent" && name != "Accept" {
			t.Fatalf("unexpected request header %q", name)
		}
	}
	if request.URL.RawQuery != "" || request.Body != nil && request.Body != http.NoBody {
		t.Fatal("request carries a query or body")
	}

	now = now.Add(23 * time.Hour)
	if _, err := c.Check(context.Background(), false); err != nil || gh.count() != 1 {
		t.Fatalf("automatic check within the interval sent a request: %d, %v", gh.count(), err)
	}
	if _, err := c.Check(context.Background(), true); err != nil || gh.count() != 2 {
		t.Fatalf("explicit check must bypass the throttle: %d, %v", gh.count(), err)
	}
	now = now.Add(25 * time.Hour)
	if _, err := c.Check(context.Background(), false); err != nil || gh.count() != 3 {
		t.Fatalf("automatic check after the interval: %d, %v", gh.count(), err)
	}
	if len(changes) == 0 || changes[len(changes)-1].Latest != "5.7.0" {
		t.Fatalf("OnChange = %+v", changes)
	}

	// The notice reflects the running version: after upgrading it disappears.
	upgraded := &Checker{Dir: c.Dir, Current: "5.7.0"}
	if s, _ := upgraded.Status(); s.Latest != "" {
		t.Fatalf("notice survived the upgrade: %+v", s)
	}
	// Turning the check off hides the stored notice without deleting history.
	if s, _ := c.SetEnabled(false); s.Latest != "" || s.Enabled {
		t.Fatalf("disabled status = %+v", s)
	}
}

func TestSameOrOlderReleaseIsNotANotice(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, tag := range []string{"v5.6.0", "v5.5.9", "v4.99.99"} {
		gh := &fakeGitHub{status: 200, body: `{"tag_name":"` + tag + `"}`}
		c := newChecker(t, gh, &now)
		c.SetEnabled(true)
		if status, err := c.Check(context.Background(), true); err != nil || status.Latest != "" || status.URL != "" {
			t.Fatalf("%s: status = %+v, %v", tag, status, err)
		}
	}
}

func TestFailedCheckIsRecordedButKeepsKnownRelease(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{status: 200, body: `{"tag_name":"v5.7.0"}`}
	c := newChecker(t, gh, &now)
	c.SetEnabled(true)
	c.Check(context.Background(), true)
	for _, failure := range []func(){
		func() { gh.err = errors.New("dial tcp: no route") },
		func() { gh.err, gh.status = nil, 403 },
		func() { gh.status, gh.body = 200, `{"tag_name":"v6.0.0-rc.1","prerelease":true}` },
		func() { gh.body = `<html>` },
	} {
		failure()
		now = now.Add(25 * time.Hour)
		status, err := c.Check(context.Background(), false)
		if err == nil {
			t.Fatal("failure was reported as success")
		}
		if status.Latest != "5.7.0" || !status.LastCheck.Equal(now) {
			t.Fatalf("status after failure = %+v", status)
		}
	}
	// A failed attempt still counts toward the throttle.
	before := gh.count()
	now = now.Add(time.Hour)
	c.Check(context.Background(), false)
	if gh.count() != before {
		t.Fatal("automatic retry within the interval after a failure")
	}
}

func TestMalformedPreferencesNeverImplyConsent(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{status: 200, body: `{"tag_name":"v5.7.0"}`}
	c := newChecker(t, gh, &now)
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Dir, prefsFile), []byte(`{"enabled":tru`), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, err := c.Status(); err == nil || status.Enabled {
		t.Fatalf("malformed preference = %+v, %v", status, err)
	}
	if _, err := c.Check(context.Background(), true); err == nil || gh.count() != 0 {
		t.Fatalf("malformed preference sent a request: %d, %v", gh.count(), err)
	}
	// An explicit choice replaces the damaged file.
	if status, err := c.SetEnabled(true); err != nil || !status.Enabled {
		t.Fatalf("SetEnabled = %+v, %v", status, err)
	}
	if reloaded, err := LoadPrefs(c.Dir); err != nil || !reloaded.Enabled {
		t.Fatalf("reloaded = %+v, %v", reloaded, err)
	}
}

func TestRunChecksOnStartAndAfterEnable(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{status: 200, body: `{"tag_name":"v5.7.0"}`}
	var mu sync.Mutex
	c := newChecker(t, gh, &now)
	c.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	checked := make(chan Status, 4)
	c.OnChange = func(s Status) {
		if !s.LastCheck.IsZero() {
			checked <- s
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx, time.Hour); close(done) }()
	defer func() { cancel(); <-done }()

	// Disabled at start: Run's initial pass must not request anything. Enabling
	// wakes it for the first check.
	if _, err := c.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-checked:
		if s.Latest != "5.7.0" {
			t.Fatalf("status = %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("enabling did not trigger a check")
	}
	if gh.count() != 1 {
		t.Fatalf("requests = %d", gh.count())
	}
}

func TestHandleValidatesBridgeMessages(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	gh := &fakeGitHub{status: 200, body: `{"tag_name":"v5.7.0"}`}
	c := newChecker(t, gh, &now)
	ctx := context.Background()
	for _, message := range []string{
		`{`,
		`{"kind":"pairroom.desktop.startup","id":"x","action":"get"}`,
		`{"kind":"` + MessageKind + `","id":"","action":"get"}`,
		`{"kind":"` + MessageKind + `","id":"x","action":"set"}`,
		`{"kind":"` + MessageKind + `","id":"x","action":"get","enabled":true}`,
		`{"kind":"` + MessageKind + `","id":"x","action":"check","enabled":true}`,
		`{"kind":"` + MessageKind + `","id":"x","action":"set","enabled":"true"}`,
		`{"kind":"` + MessageKind + `","id":"x","action":"download"}`,
		`{"kind":"` + MessageKind + `","id":"x","action":"get","url":"https://evil.example"}`,
		`{"kind":"` + MessageKind + `","id":"x","action":"get"}{}`,
		`{"kind":"` + MessageKind + `","id":"` + strings.Repeat("x", 101) + `","action":"get"}`,
		`{"kind":"` + MessageKind + `","id":"x","action":"get","pad":"` + strings.Repeat("x", 1100) + `"}`,
	} {
		if response, ok := c.Handle(ctx, message); ok {
			t.Errorf("accepted %q: %+v", message, response)
		}
	}
	if gh.count() != 0 {
		t.Fatal("rejected messages sent a request")
	}

	response, ok := c.Handle(ctx, `{"kind":"`+MessageKind+`","id":"a","action":"check"}`)
	if !ok || response.ID != "a" || response.Enabled == nil || *response.Enabled || response.Error != ErrDisabled.Error() || gh.count() != 0 {
		t.Fatalf("check while disabled = %+v", response)
	}
	response, _ = c.Handle(ctx, `{"kind":"`+MessageKind+`","id":"b","action":"set","enabled":true}`)
	if response.Enabled == nil || !*response.Enabled || response.Current != "5.6.0" || response.Error != "" || gh.count() != 0 {
		t.Fatalf("set = %+v", response)
	}
	response, _ = c.Handle(ctx, `{"kind":"`+MessageKind+`","id":"c","action":"check"}`)
	if response.Latest != "5.7.0" || response.URL != ReleasePage(Version{5, 7, 0}) || response.CheckedAt != "2026-09-27T12:00:00Z" || response.Error != "" {
		t.Fatalf("check = %+v", response)
	}
	gh.err = errors.New("offline")
	response, _ = c.Handle(ctx, `{"kind":"`+MessageKind+`","id":"d","action":"check"}`)
	if response.Error == "" || response.Latest != "5.7.0" {
		t.Fatalf("failed check = %+v", response)
	}
	response, _ = c.Handle(ctx, `{"kind":"`+MessageKind+`","id":"e","action":"get"}`)
	if response.Error != "" || response.Latest != "5.7.0" || gh.count() != 2 {
		t.Fatalf("get = %+v (requests %d)", response, gh.count())
	}
}
