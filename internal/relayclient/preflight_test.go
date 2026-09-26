package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/version"
)

type preflightFixture struct {
	root     string
	endpoint string
	requests *int
}

// newPreflightFixture isolates the caller, points the bare pairroom command at
// the running binary, and serves a Service that registers root as a Project.
func newPreflightFixture(t *testing.T, serviceVersion string, rooms []map[string]any) preflightFixture {
	t.Helper()
	stubLineage(t, 4242, "claude", true)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "preflight-session")
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "")
	binary := filepath.Join(t.TempDir(), "pairroom.exe")
	if err := os.WriteFile(binary, []byte("cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	stubPreflightCLI(t, binary, nil)
	root := sessionGitRoot(t)
	requests := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer preflight-secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"version": serviceVersion, "projects": []any{map[string]string{"id": "project", "root": root}}, "rooms": rooms})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("preflight made an unexpected request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	endpoint := filepath.Join(t.TempDir(), "relay-endpoint.json")
	if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: server.URL, Token: "preflight-secret-token"}); err != nil {
		t.Fatal(err)
	}
	return preflightFixture{root: root, endpoint: endpoint, requests: &requests}
}

func stubPreflightCLI(t *testing.T, onPath string, lookErr error) {
	t.Helper()
	executable, look := preflightExecutable, preflightLookPath
	preflightExecutable = func() (string, error) { return onPath, nil }
	preflightLookPath = func(string) (string, error) { return onPath, lookErr }
	t.Cleanup(func() { preflightExecutable, preflightLookPath = executable, look })
}

func runPreflightJSON(t *testing.T, o options) (preflightReport, string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runPreflight(context.Background(), o, &out)
	var report preflightReport
	if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil {
		t.Fatalf("preflight output is not one JSON report: %v: %q", decodeErr, out.String())
	}
	return report, out.String(), err
}

func activeNativeRoom() []map[string]any {
	return []map[string]any{
		{"id": "room1", "project_id": "project", "host_mode": "native", "lifecycle": "active"},
		{"id": "room2", "project_id": "project", "host_mode": "native", "lifecycle": "archived"},
		{"id": "room3", "project_id": "project", "host_mode": "embedded", "lifecycle": "active"},
	}
}

func TestPreflightReadyWhenCLIServiceAndHookAreInPlace(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current+"+8.a5cb253", activeNativeRoom())
	if err := editHooks(f.root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	report, raw, err := runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint})
	if err != nil || !report.Ready {
		t.Fatalf("expected ready: err=%v report=%s", err, raw)
	}
	if report.CLI.Status != checkPass || !report.CLI.SameBinary {
		t.Fatalf("CLI check: %+v", report.CLI)
	}
	if report.Caller.Runtime != "claude" || !report.Caller.InSession || report.Caller.Bound {
		t.Fatalf("caller check: %+v", report.Caller)
	}
	// Build metadata after the release does not count as a version mismatch.
	if !report.Service.VersionMatch || !report.Service.ProjectRegistered || report.Service.ActiveNativeRooms != 1 {
		t.Fatalf("service check: %+v", report.Service)
	}
	hook := report.Hooks["claude"]
	if hook.Status != "installed" || hook.Approval != "unknown" || len(report.Hooks) != 1 {
		t.Fatalf("hooks: %+v", report.Hooks)
	}
	if len(report.NextSteps) != 1 || !strings.Contains(report.NextSteps[0], "pairroom relay bind") || strings.Contains(report.NextSteps[0], "--create --name") {
		t.Fatalf("an active Room should suggest joining it: %q", report.NextSteps)
	}
	if strings.Contains(raw, "preflight-secret-token") {
		t.Fatal("preflight printed the Service endpoint token")
	}
	if *f.requests != 1 {
		t.Fatalf("preflight made %d Service requests, want one read", *f.requests)
	}
}

func TestPreflightFailsWhenBareCommandIsNotOnPath(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current, nil)
	if err := editHooks(f.root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	stubPreflightCLI(t, "", errors.New("executable file not found in %PATH%"))
	report, _, err := runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint})
	if !errors.Is(err, errPreflightNotReady) || report.Ready || report.CLI.Status != checkFail {
		t.Fatalf("missing PATH entry must fail: err=%v cli=%+v", err, report.CLI)
	}
	if len(report.NextSteps) == 0 || !strings.Contains(report.NextSteps[0], "PATH") {
		t.Fatalf("first next step must fix PATH: %q", report.NextSteps)
	}
}

func TestPreflightReportsMissingHookAndNoRoomWithActionableSteps(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current, nil)
	report, _, err := runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint})
	if !errors.Is(err, errPreflightNotReady) || report.Ready || report.Hooks["claude"].Status != "missing" {
		t.Fatalf("missing hook must block: err=%v hooks=%+v", err, report.Hooks)
	}
	steps := strings.Join(report.NextSteps, "\n")
	if !strings.Contains(steps, "pairroom relay install --runtime claude") || !strings.Contains(steps, "project hook consent") {
		t.Fatalf("next steps must install and approve the hook: %q", report.NextSteps)
	}
	if strings.Contains(steps, "bind") {
		t.Fatalf("bind advice before the hook exists: %q", report.NextSteps)
	}
}

func TestPreflightSuggestsCreateWithoutAnActiveNativeRoom(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current, nil)
	if err := editHooks(f.root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	report, _, err := runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint})
	if err != nil || !report.Ready || report.Service.ActiveNativeRooms != 0 {
		t.Fatalf("expected ready without a Room: err=%v service=%+v", err, report.Service)
	}
	if last := report.NextSteps[len(report.NextSteps)-1]; !strings.Contains(last, "bind --create") {
		t.Fatalf("expected a create step, got %q", report.NextSteps)
	}
}

func TestPreflightWithoutServiceNamesTheStartOptions(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current, nil)
	if err := editHooks(f.root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "relay-endpoint.json")
	report, _, err := runPreflightJSON(t, options{repo: f.root, endpoint: missing})
	if !errors.Is(err, errPreflightNotReady) || report.Service.Status != checkFail {
		t.Fatalf("missing Service must fail: err=%v service=%+v", err, report.Service)
	}
	if !strings.Contains(report.Service.Hint, "pairroom daemon status") || !strings.Contains(report.Service.Hint, "--service-file") {
		t.Fatalf("hint must name how to find or start the Service: %q", report.Service.Hint)
	}
}

func TestPreflightWarnsOnVersionMismatchWithoutBlocking(t *testing.T) {
	f := newPreflightFixture(t, "v0.0.1", nil)
	if err := editHooks(f.root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	report, _, err := runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint})
	if err != nil || !report.Ready || report.Service.Status != checkWarn || report.Service.VersionMatch {
		t.Fatalf("version mismatch should warn only: err=%v service=%+v", err, report.Service)
	}
}

func TestPreflightGrokReusesClaudeProjectHook(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current, nil)
	if err := editHooks(f.root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	report, _, err := runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint, kind: string(model.RuntimeGrok)})
	hook := report.Hooks["grok"]
	if err != nil || hook.Status != "installed" || !strings.HasSuffix(hook.File, filepath.Join(".claude", "settings.json")) || hook.Detail == "" {
		t.Fatalf("grok should reuse the Claude hook: err=%v hook=%+v", err, hook)
	}
	t.Setenv("GROK_CLAUDE_HOOKS_ENABLED", "false")
	report, _, err = runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint, kind: string(model.RuntimeGrok)})
	if !errors.Is(err, errPreflightNotReady) || report.Hooks["grok"].Status != "missing" {
		t.Fatalf("disabled compatibility needs the Grok hook: err=%v hooks=%+v", err, report.Hooks)
	}
}

func TestPreflightPlainTerminalChecksEveryRuntime(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current, nil)
	stubLineage(t, 0, "", false)
	if err := editHooks(f.root, model.RuntimeCodex, false); err != nil {
		t.Fatal(err)
	}
	report, _, err := runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint})
	if err != nil || !report.Ready || len(report.Hooks) != 3 || report.Caller.Status != checkWarn || report.Caller.InSession {
		t.Fatalf("plain terminal: err=%v caller=%+v hooks=%+v", err, report.Caller, report.Hooks)
	}
	if !strings.Contains(strings.Join(report.NextSteps, "\n"), "tool call") {
		t.Fatalf("plain terminal must explain where bind runs: %q", report.NextSteps)
	}
}

func TestPreflightDetectsAnExistingBinding(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current, activeNativeRoom())
	if err := editHooks(f.root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	callerState(t, f.root, "room1", "preflight-session", model.ActorSlot1, model.RuntimeClaude)
	report, _, _ := runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint})
	steps := strings.Join(report.NextSteps, "\n")
	if !report.Caller.Bound || !strings.Contains(steps, "relay doctor") || strings.Contains(steps, "pairroom relay bind (") {
		t.Fatalf("bound session must be sent to relay doctor: caller=%+v steps=%q", report.Caller, report.NextSteps)
	}
}

func TestPreflightIsReadOnly(t *testing.T) {
	f := newPreflightFixture(t, "v"+version.Current, nil)
	before := preflightTree(t, f.root)
	for _, kind := range []string{"", "claude", "codex", "grok"} {
		_, _, _ = runPreflightJSON(t, options{repo: f.root, endpoint: f.endpoint, kind: kind})
	}
	if after := preflightTree(t, f.root); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("preflight wrote into the workspace:\nbefore=%q\nafter=%q", before, after)
	}
	if _, err := os.Stat(filepath.Join(f.root, ".pairroom")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created relay state: %v", err)
	}
}

// preflightTree lists workspace entries and file contents, excluding Git's own
// metadata; directory timestamps are deliberately not compared.
func preflightTree(t *testing.T, root string) []string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == ".git" {
			return filepath.SkipDir
		}
		entry := filepath.ToSlash(rel)
		if !d.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			entry += "=" + string(data)
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	return entries
}
