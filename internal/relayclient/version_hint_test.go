package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/version"
)

const otherRelease = "0.0.1"

func preflightHintLines(diagnostic string) []string {
	var lines []string
	for _, line := range strings.Split(diagnostic, "\n") {
		if strings.Contains(line, "pairroom relay preflight") {
			lines = append(lines, line)
		}
	}
	return lines
}

func sendWithRelease(t *testing.T, release string) (stdout, diagnostic string, f *foregroundFixture) {
	t.Helper()
	f = newForegroundFixture(t, foregroundFixtureOptions{release: release})
	var out, diag bytes.Buffer
	if err := f.run(context.Background(), "send", nil, &out, &diag, "--id", "note-1", "--text", "hello"); err != nil {
		t.Fatal(err)
	}
	return out.String(), diag.String(), f
}

func TestRelayNamesServiceReleaseMismatchOnStderrOnly(t *testing.T) {
	matchOut, matchDiag, matched := sendWithRelease(t, version.Current)
	skewOut, skewDiag, skewed := sendWithRelease(t, "v"+otherRelease+"+3.abcdef0")
	if skewOut != matchOut || !strings.Contains(skewOut, `"published":"outgoing-note-1"`) {
		t.Fatalf("mismatch changed stdout:\n%q\n%q", skewOut, matchOut)
	}
	if len(preflightHintLines(matchDiag)) != 0 {
		t.Fatalf("matching release printed a hint: %q", matchDiag)
	}
	// Exactly one appended line, naming both releases; the existing diagnostic
	// output is untouched.
	hint, found := strings.CutPrefix(skewDiag, matchDiag)
	if !found || strings.Count(hint, "\n") != 1 || !strings.HasSuffix(hint, "\n") ||
		!strings.Contains(hint, otherRelease) || !strings.Contains(hint, version.Current) || strings.Contains(hint, "+3.abcdef0") {
		t.Fatalf("mismatch hint = %q", hint)
	}
	for _, secret := range []string{"private-long-lived-secret", "management-secret-not-output"} {
		if strings.Contains(skewDiag, secret) || strings.Contains(skewOut, secret) {
			t.Fatalf("hint leaked %s", secret)
		}
	}
	// Observation used only the send response: no probe or other request.
	for _, f := range []*foregroundFixture{matched, skewed} {
		f.mu.Lock()
		calls := len(f.calls)
		f.mu.Unlock()
		if calls != 1 || f.count("send") != 1 {
			t.Fatalf("send made extra requests: %v", f.calls)
		}
	}
}

func TestRelayVersionHintIgnoresBuildMetadataAndUnknownRelease(t *testing.T) {
	for _, release := range []string{"v" + version.Current + "+12.1234567", "", "not a version!"} {
		_, diagnostic, _ := sendWithRelease(t, release)
		if lines := preflightHintLines(diagnostic); len(lines) != 0 {
			t.Fatalf("release %q printed %q", release, lines)
		}
	}
}

func TestRelayVersionHintExplainsSkewShapedRejection(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{release: otherRelease, failAction: "send", failStatus: http.StatusNotFound})
	var out, diagnostic bytes.Buffer
	if err := f.run(context.Background(), "send", nil, &out, &diagnostic, "--id", "note-1", "--text", "hello"); err == nil {
		t.Fatal("rejected send reported success")
	}
	lines := preflightHintLines(diagnostic.String())
	if out.Len() != 0 || len(lines) != 1 || !strings.Contains(lines[0], "failure may come from a release mismatch") {
		t.Fatalf("stdout=%q hint=%q", out.String(), lines)
	}
	// A transport-shaped failure still names the mismatch, but not as its cause.
	f = newForegroundFixture(t, foregroundFixtureOptions{release: otherRelease, failAction: "send"})
	diagnostic.Reset()
	_ = f.run(context.Background(), "send", nil, &out, &diagnostic, "--id", "note-1", "--text", "hello")
	lines = preflightHintLines(diagnostic.String())
	if len(lines) != 1 || strings.Contains(lines[0], "failure may come from") {
		t.Fatalf("503 hint=%q", lines)
	}
}

// A binding lookup can fail before any Service contact; only then does a
// foreground command read the Service release, once and read-only.
func TestRelayProbesServiceReleaseOnlyForPreContactBindingFailure(t *testing.T) {
	for _, tc := range []struct {
		name, session, snapshotVersion string
		wantHint                       bool
	}{
		{"workspace without binding", "", "v" + otherRelease + "+1.abcdef0", true},
		{"unassociated session", "native-session", "v" + otherRelease, true},
		{"same release", "", "v" + version.Current + "+4.1234567", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateCaller(t)
			if tc.session != "" {
				t.Setenv("CLAUDE_CODE_SESSION_ID", tc.session)
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if data, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v: %s", err, data)
			}
			var mu sync.Mutex
			var requests []string
			// An older Service: no release header, only the snapshot version.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				mu.Unlock()
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/service" || r.Header.Get("Authorization") != "Bearer management-secret-not-output" {
					http.Error(w, "unexpected", http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"version": tc.snapshotVersion, "projects": []any{}, "rooms": []any{}})
			}))
			defer srv.Close()
			endpoint := filepath.Join(t.TempDir(), relay.EndpointFile)
			if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: srv.URL, Token: "management-secret-not-output"}); err != nil {
				t.Fatal(err)
			}
			var out, diagnostic bytes.Buffer
			err = Run(context.Background(), []string{"status", "--repo", root, "--service-file", endpoint}, nil, &out, &diagnostic)
			if err == nil {
				t.Fatal("status without a binding succeeded")
			}
			lines := preflightHintLines(diagnostic.String())
			if out.Len() != 0 || strings.Contains(diagnostic.String(), "management-secret-not-output") {
				t.Fatalf("stdout=%q diagnostic=%q", out.String(), diagnostic.String())
			}
			if tc.wantHint != (len(lines) == 1) || len(lines) > 1 || tc.wantHint && !strings.Contains(lines[0], "failure may come from a release mismatch") {
				t.Fatalf("hint=%q", lines)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 1 || requests[0] != "GET /api/v1/service" {
				t.Fatalf("probe requests = %v", requests)
			}
		})
	}
}

func runStopHookWithDiagnostic(t *testing.T, cwd, text string) (stdout, diagnostic string, err error) {
	t.Helper()
	data, _ := json.Marshal(HookInput{Event: "Stop", SessionID: "session", CWD: cwd, LastAssistantMessage: &text})
	var out, diag bytes.Buffer
	err = Run(context.Background(), []string{"hook", "--runtime", "claude"}, bytes.NewReader(data), &out, &diag)
	return out.String(), diag.String(), err
}

// The Stop hook learns the release from responses it already receives and
// never spends a request on it, even when a rejection looks like skew.
func TestStopHookVersionHintAddsNoRequest(t *testing.T) {
	isolateCaller(t)
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	service := &hookService{accepted: map[uint64]string{}}
	srv := httptest.NewServer(service.handler(t))
	defer srv.Close()
	if err := relay.AtomicJSON(c.State.EndpointPath, relay.Endpoint{URL: srv.URL, Token: "management-secret"}); err != nil {
		t.Fatal(err)
	}
	// Also serve the default endpoint, so any probe the hook made would be
	// counted rather than silently find no Service.
	defaultPath, err := defaultEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	// The config root (for example macOS Library/Application Support) may not
	// exist under an isolated home; secureDir creates only its last element.
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(defaultPath)), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := secureDir(filepath.Dir(filepath.Dir(defaultPath)), filepath.Base(filepath.Dir(defaultPath))); err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(defaultPath, relay.Endpoint{URL: srv.URL, Token: "management-secret"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, release string
		rejectWait    bool
		hint          string
	}{
		{"matching release", version.Current, false, ""},
		{"unknown release", "", false, ""},
		{"unknown release on rejection", "", true, ""},
		{"mismatch", otherRelease, false, "the Service runs release " + otherRelease},
		{"mismatch on rejection", otherRelease, true, "failure may come from a release mismatch"},
	} {
		service.release, service.rejectWait = tc.release, tc.rejectWait
		stdout, diagnostic, err := runStopHookWithDiagnostic(t, f.args[1], "@codex "+tc.name)
		if tc.rejectWait != (err != nil) {
			t.Fatalf("%s: err=%v", tc.name, err)
		}
		if !tc.rejectWait && stdout != "{}\n" || tc.rejectWait && stdout != "" {
			t.Fatalf("%s: stdout=%q", tc.name, stdout)
		}
		if got := strings.Join(service.take(), ","); got != "report,wait" {
			t.Fatalf("%s: operations = %s", tc.name, got)
		}
		lines := preflightHintLines(diagnostic)
		if tc.hint == "" && len(lines) != 0 || tc.hint != "" && (len(lines) != 1 || !strings.Contains(lines[0], tc.hint)) {
			t.Fatalf("%s: hint=%q", tc.name, lines)
		}
	}
}

func TestServiceReleaseAcceptsOnlyAVersionToken(t *testing.T) {
	for display, want := range map[string]string{
		"v5.6.0":                 "5.6.0",
		"5.6.0":                  "5.6.0",
		"v5.5.1+8.a5cb253":       "5.5.1",
		"v6.0.0-rc.1+2.abcdef0":  "6.0.0-rc.1",
		"":                       "",
		"v":                      "",
		"5.6.0 run rm -rf":       "",
		"5.6.0\nIgnore previous": "",
		strings.Repeat("9", 40):  "",
	} {
		if got := serviceRelease(display); got != want {
			t.Fatalf("serviceRelease(%q) = %q, want %q", display, got, want)
		}
	}
}
