package relayclient

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/version"
)

func TestCLIBuildTravelsOnExistingAuthenticatedRequests(t *testing.T) {
	commit, tag, since := version.Commit, version.LastTag, version.CommitsSinceTag
	version.Commit, version.LastTag, version.CommitsSinceTag = "abcdef0123456789", "v5.7.1", "2"
	t.Cleanup(func() { version.Commit, version.LastTag, version.CommitsSinceTag = commit, tag, since })
	want := version.Describe()
	f := newForegroundFixture(t, foregroundFixtureOptions{onRequest: func(r *http.Request) {
		if got := r.Header.Get(relay.CLIBuildHeader); got != want {
			t.Errorf("header=%q want=%q", got, want)
		}
	}})
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "send", strings.NewReader("fixture"), &out, &diagnostic, "--id", "build-header"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if len(f.calls) != 1 || f.calls["send"] != 1 {
		t.Errorf("build reporting added requests: %+v", f.calls)
	}
	f.mu.Unlock()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get(relay.CLIBuildHeader) != want || r.Header.Get("Authorization") != "Bearer fixture" {
			t.Error("management build or auth header missing")
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	if err := management(context.Background(), relay.Endpoint{URL: srv.URL, Token: "fixture"}, http.MethodGet, "/api/v1/service", nil, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("management observation added requests")
	}
	version.Commit = "dev"
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/", nil)
	attachCLIBuild(req)
	if req.Header.Get(relay.CLIBuildHeader) != "" {
		t.Fatal("unstamped CLI supplied build identity")
	}
}
