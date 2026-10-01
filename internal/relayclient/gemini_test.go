package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func geminiCallerFixture(t *testing.T) int {
	t.Helper()
	IsolateNativeCaller(t)
	oldTable, oldBirth, oldNow := processTable, geminiProcessBirth, geminiCallerNow
	t.Cleanup(func() { processTable, geminiProcessBirth, geminiCallerNow = oldTable, oldBirth, oldNow })
	const host = 987654
	processTable = func() (map[int]procInfo, error) {
		return map[int]procInfo{
			os.Getpid(): {ppid: host - 1, name: "pairroom"}, host - 1: {ppid: host, name: "sh"},
			host: {ppid: host + 1, name: "node.exe"}, host + 1: {ppid: 1, name: "claude"},
		}, nil
	}
	geminiProcessBirth = func(pid int) (string, error) {
		if pid != host {
			return "", errors.New("not the host")
		}
		return "boot:12345", nil
	}
	geminiCallerNow = func() time.Time { return time.Date(2026, 10, 1, 1, 2, 3, 0, time.UTC) }
	harnessAncestor = findHarnessAncestor
	return host
}

func observeGemini(t *testing.T, session, command string) string {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"hook_event_name": "BeforeTool", "session_id": session, "tool_name": "run_shell_command", "tool_input": map[string]string{"command": command}})
	var out, diag bytes.Buffer
	handled, err := geminiBeforeTool(data, &out, &diag)
	if !handled || err != nil || strings.TrimSpace(out.String()) != "{}" {
		t.Fatalf("BeforeTool changed tool input: %t %q %v", handled, out.String(), err)
	}
	return diag.String()
}

func TestGeminiBeforeToolSuppliesExactBindIdentityWithoutEnvironmentFallback(t *testing.T) {
	root, endpoint, created := createBindFixture(t, model.RuntimeGemini)
	host := geminiCallerFixture(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-claude")
	t.Setenv("GEMINI_SESSION_ID", "forged-hook-only-env")
	if _, err := requireSessionID(model.RuntimeGemini); err == nil {
		t.Fatal("accepted hook-only env outside an observed tool call")
	}
	if diag := observeGemini(t, "official-gemini", "pairroom relay bind --create"); diag != "" {
		t.Fatal(diag)
	}
	caller, err := currentNativeCaller()
	if err != nil || caller.runtime != model.RuntimeGemini || caller.session != "official-gemini" {
		t.Fatalf("outer caller won: %+v %v", caller, err)
	}
	if err := editHooks(root, model.RuntimeGemini, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := bind(context.Background(), root, options{create: true, endpoint: endpoint}, &out); err != nil {
		t.Fatal(err)
	}
	if *created != 1 || !strings.Contains(out.String(), "official-gemini") {
		t.Fatalf("create/bind=%d %s", *created, out.String())
	}
	path, err := geminiCallerPath(host, false)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, forbidden := range []string{"pairroom relay", root, "outer-claude", "forged-hook-only-env", "test-token"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("caller cache persisted %q", forbidden)
		}
	}
	c, err := loadLocal(filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "slot1"))
	if err != nil || c.State.Runtime != model.RuntimeGemini || c.State.SessionID != "official-gemini" {
		t.Fatalf("binding=%+v %v", c, err)
	}
}

func TestGeminiCallerRejectsStaleReusedMissingAndUnsafeMetadata(t *testing.T) {
	host := geminiCallerFixture(t)
	if diag := observeGemini(t, "session-one", "pairroom relay wait"); diag != "" {
		t.Fatal(diag)
	}
	path, _ := geminiCallerPath(host, false)
	original, _ := os.ReadFile(path)
	var record geminiCallerRecord
	_ = json.Unmarshal(original, &record)
	for _, test := range []struct {
		name   string
		change func(*geminiCallerRecord)
	}{
		{"expired", func(r *geminiCallerRecord) { r.Seen = r.Seen.Add(-geminiCallerTTL - time.Second) }},
		{"future", func(r *geminiCallerRecord) { r.Seen = r.Seen.Add(time.Second) }},
		{"pid-reused", func(r *geminiCallerRecord) { r.Birth = "other-process" }},
		{"invalid-session", func(r *geminiCallerRecord) { r.Session = "../outside" }},
		{"missing-birth", func(r *geminiCallerRecord) { r.Birth = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := record
			test.change(&r)
			raw, _ := json.Marshal(r)
			if err := atomicText(path, string(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if got := geminiCallerForProcess(host, "node"); got != "" {
				t.Fatal(got)
			}
		})
	}
	if err := atomicText(path, string(original), 0600); err != nil {
		t.Fatal(err)
	}
	if got := geminiCallerForProcess(host, "unrelated"); got != "" {
		t.Fatal("generic process accepted")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if geminiCallerForProcess(host, "node") != "" {
			t.Fatal("public metadata accepted")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if geminiCallerForProcess(host, "node") != "" {
		t.Fatal("missing metadata accepted")
	}
}

func TestGeminiBeforeToolUnrelatedAndUnboundAfterAgentAreInert(t *testing.T) {
	host := geminiCallerFixture(t)
	if diag := observeGemini(t, "session", "echo hello"); diag != "" {
		t.Fatal(diag)
	}
	if path, err := geminiCallerPath(host, false); err == nil {
		if _, err := os.Stat(path); err == nil {
			t.Fatal("unrelated tool wrote metadata")
		}
	}
	root := sessionGitRoot(t)
	payload, _ := json.Marshal(map[string]any{"hook_event_name": "AfterAgent", "session_id": "not-bound", "cwd": root, "prompt_response": "ordinary answer"})
	var out, diag bytes.Buffer
	if err := Run(context.Background(), []string{"hook", "--runtime", "gemini"}, bytes.NewReader(payload), &out, &diag); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "{}" || diag.Len() != 0 {
		t.Fatalf("unbound hook was not inert: %q %q", out.String(), diag.String())
	}
}

func TestGeminiAfterAgentUsesOfficialFullReplyOnly(t *testing.T) {
	text := "@codex 完整\n  response 🌟"
	raw, _ := json.Marshal(map[string]any{"hook_event_name": "AfterAgent", "session_id": "session", "cwd": "/repo", "prompt_response": text, "stop_hook_active": true})
	hook, err := decodeGeminiHook(raw)
	if err != nil || hook.Event != "Stop" || hook.LastAssistantMessage == nil || *hook.LastAssistantMessage != text || !hook.StopHookActive {
		t.Fatalf("decoded=%+v %v", hook, err)
	}
	for _, raw := range []string{`{"hook_event_name":"AfterAgent","session_id":"s","last_assistant_message":"forged","prompt_response":"real"}`, `{"hook_event_name":"AfterAgent","session_id":""}`, `{"hook_event_name":"AfterAgent","session_id":"../s"}`, `{"hook_event_name":"AfterAgent","session_id":"s","prompt_response":5}`, "\xff"} {
		if _, err := decodeGeminiHook([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid input %q", raw)
		}
	}
	hook, err = decodeGeminiHook([]byte(`{"hook_event_name":"Stop","session_id":"s","last_assistant_message":"wrong vendor"}`))
	if err != nil || hook.Event != "" {
		t.Fatal("treated another vendor Stop as AfterAgent")
	}
}

func TestGeminiInstallPreservesSettingsAndOwnsBothHooks(t *testing.T) {
	IsolateNativeCaller(t)
	root, home := t.TempDir(), t.TempDir()
	t.Setenv("GEMINI_CLI_HOME", home)
	dir, err := secureDir(root, ".gemini")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	initial := `{"security":{"auth":{"selectedType":"oauth-personal"}},"hooks":{"AfterAgent":[{"hooks":[{"type":"command","command":"user-hook"}]}]}}`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runInstall(root, []model.RuntimeKind{model.RuntimeGemini}, strings.NewReader(""), &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := editHooks(root, model.RuntimeGemini, false); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) || installed(root, model.RuntimeGemini) != nil {
		t.Fatal("install is not idempotent/discoverable")
	}
	var config map[string]any
	_ = json.Unmarshal(second, &config)
	hooks := config["hooks"].(map[string]any)
	if hooks["Stop"] != nil || !hasGeminiBeforeTool(hooks) || !strings.Contains(string(second), "45000") || !strings.Contains(string(second), "user-hook") || !strings.Contains(string(second), "oauth-personal") {
		t.Fatal(string(second))
	}
	if skillStatus(model.RuntimeGemini) != "current" {
		t.Fatal("Gemini native skill was not installed")
	}
	if err := editHooks(root, model.RuntimeGemini, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "user-hook") || !strings.Contains(string(raw), "oauth-personal") || strings.Contains(string(raw), hookCommand(model.RuntimeGemini)) {
		t.Fatal(string(raw))
	}
	if installed(root, model.RuntimeGemini) == nil {
		t.Fatal("removed hooks still pass")
	}
	// Missing BeforeTool and explicit hooks.enabled=false both fail preflight.
	if err := editHooks(root, model.RuntimeGemini, false); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(path)
	_ = json.Unmarshal(raw, &config)
	hooks = config["hooks"].(map[string]any)
	hooks["BeforeTool"] = []any{}
	raw, _ = json.Marshal(config)
	_ = os.WriteFile(path, raw, 0600)
	if installed(root, model.RuntimeGemini) == nil {
		t.Fatal("AfterAgent alone passed bind preflight")
	}
	hooks["enabled"] = false
	raw, _ = json.Marshal(config)
	_ = os.WriteFile(path, raw, 0600)
	_, disabled, err := ownRelayStopHook(root, model.RuntimeGemini)
	if err != nil || !disabled {
		t.Fatalf("disabled=%v %v", disabled, err)
	}
}

func TestGeminiProcessBirthIsStableForLiveProcess(t *testing.T) {
	first, err := platformProcessBirth(os.Getpid())
	if err != nil || first == "" {
		t.Fatalf("birth: %q %v", first, err)
	}
	second, err := platformProcessBirth(os.Getpid())
	if err != nil || first != second {
		t.Fatalf("unstable birth: %q %q %v", first, second, err)
	}
}

func TestGeminiStaleIdentityCannotFallBackToAnOuterHarness(t *testing.T) {
	host := geminiCallerFixture(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "outer-session")
	if diag := observeGemini(t, "official-gemini", "pairroom relay wait"); diag != "" {
		t.Fatal(diag)
	}
	now := geminiCallerNow()
	geminiCallerNow = func() time.Time { return now.Add(geminiCallerTTL + time.Second) }
	pid, name, ok := findHarnessAncestor()
	if !ok || pid != host || name != "gemini" {
		t.Fatal("stale Gemini observation selected an outer harness")
	}
	if caller, err := currentNativeCaller(); err == nil || caller.runtime != model.RuntimeGemini || caller.session != "" {
		t.Fatalf("unsafe fallback: %+v %v", caller, err)
	}
}

func TestGeminiCumulativeResponseBoundaries(t *testing.T) {
	var cursor *GeminiResponseCursor
	for _, test := range []struct {
		full, want string
		active     bool
	}{
		{"@codex first 🌟", "@codex first 🌟", false},
		{"@codex first 🌟\nprivate follow-up", "private follow-up", true},
		{"@codex first 🌟\nprivate follow-up\nprivate follow-up", "private follow-up", true},
		{"@codex first 🌟\nprivate follow-up\nprivate follow-up", "", true},
		// A new user turn is a new publication even if its entire text equals
		// the prior turn. Never implement body-based deduplication here.
		{"@codex first 🌟", "@codex first 🌟", false},
		{"@codex first 🌟", "@codex first 🌟", false},
		{"", "", false},
		{"@codex after empty", "@codex after empty", true},
	} {
		hook := HookInput{LastAssistantMessage: &test.full, StopHookActive: test.active}
		text, next, err := geminiResponse(hook, cursor)
		if err != nil || text != test.want {
			t.Fatalf("full=%q active=%t: text=%q want=%q err=%v", test.full, test.active, text, test.want, err)
		}
		cursor = next
	}
	full := "@codex old"
	_, cursor, err := geminiResponse(HookInput{LastAssistantMessage: &full}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, full := range []string{"short", "@codex new", "@codex oldno separator"} {
		if _, _, err := geminiResponse(HookInput{LastAssistantMessage: &full, StopHookActive: true}, cursor); err == nil {
			t.Fatalf("accepted unverifiable continuation %q", full)
		}
	}
	if _, _, err := geminiResponse(HookInput{LastAssistantMessage: &full, StopHookActive: true}, nil); err == nil {
		t.Fatal("accepted continuation with no retained prefix")
	}
}

func TestGeminiCursorSharesPublicationWAL(t *testing.T) {
	full := "@codex first"
	text, first, err := geminiResponse(HookInput{LastAssistantMessage: &full}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var saved State
	writes := 0
	c := &Client{State: State{Schema: 2, Runtime: model.RuntimeGemini}, Save: func(next State) error {
		writes++
		saved = next
		if next.GeminiResponse == nil || next.Pending == nil || next.Pending.Text != text {
			t.Fatalf("cursor and response were not written together: %+v", next)
		}
		return nil
	}}
	if err := c.reservePublication(text, first); err != nil || writes != 1 {
		t.Fatalf("reservation: writes=%d err=%v", writes, err)
	}
	raw, _ := json.Marshal(saved)
	var restored State
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	c.State = restored
	full += "\nunaddressed"
	text, next, err := geminiResponse(HookInput{LastAssistantMessage: &full, StopHookActive: true}, restored.GeminiResponse)
	if err != nil || text != "unaddressed" {
		t.Fatalf("restored cursor: %q %v", text, err)
	}
	c.Save = func(State) error { return errors.New("injected failed write") }
	if err := c.reservePublication(text, next); err == nil {
		t.Fatal("failed cursor/WAL write succeeded")
	}
	if c.State.LastSeq != 1 || *c.State.GeminiResponse != *first || len(c.State.Held) != 0 {
		t.Fatalf("failed write advanced cursor or publication: %+v", c.State)
	}
	c.Save = func(State) error { t.Fatal("full backlog wrote a cursor"); return nil }
	for i := 0; i < maxPublicationBacklog-1; i++ {
		c.State.Held = append(c.State.Held, Pending{Seq: uint64(i + 2)})
	}
	if err := c.reservePublication(text, next); !errors.Is(err, errPublicationBacklogFull) || *c.State.GeminiResponse != *first {
		t.Fatalf("full backlog advanced cursor: %+v %v", c.State, err)
	}
	if err := c.reservePublication(strings.Repeat("x", relay.MaxBodyBytes+1), next); !errors.Is(err, errReplyTooLarge) || *c.State.GeminiResponse != *first {
		t.Fatalf("oversized reply advanced cursor: %+v %v", c.State, err)
	}
}

func TestGeminiCursorSurvivesUnknownPublicationAndRestart(t *testing.T) {
	c, server := publicationClient(t)
	c.State.Runtime = model.RuntimeGemini
	full := "@codex first"
	text, first, err := geminiResponse(HookInput{LastAssistantMessage: &full}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.reservePublication(text, first); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.dropAfter = true
	server.mu.Unlock()
	if err := c.Reconcile(context.Background(), false); !errors.Is(err, relay.ErrUnknown) {
		t.Fatalf("lost acknowledgement: %v", err)
	}
	c, err = load(c.Dir)
	if err != nil {
		t.Fatal(err)
	}
	full += "\nprivate follow-up"
	text, next, err := geminiResponse(HookInput{LastAssistantMessage: &full, StopHookActive: true}, c.State.GeminiResponse)
	if err != nil || text != "private follow-up" {
		t.Fatalf("restart reused old response: %q %v", text, err)
	}
	if err := c.reservePublication(text, next); err != nil {
		t.Fatal(err)
	}
	if c.State.Pending.Text != "@codex first" || len(c.State.Held) != 1 || c.State.Held[0].Text != "private follow-up" {
		t.Fatalf("original unknown publication was overwritten: %+v", c.State)
	}
	server.mu.Lock()
	server.dropAfter = false
	server.mu.Unlock()
	if err := c.Reconcile(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.reports != 2 || server.queries != 1 || c.State.Pending != nil || *c.State.GeminiResponse != *next {
		t.Fatalf("uncertain publication replayed or cursor lost: reports=%d queries=%d state=%+v", server.reports, server.queries, c.State)
	}
}

func TestGeminiBoundedCumulativeHookInput(t *testing.T) {
	IsolateNativeCaller(t)
	root := sessionGitRoot(t)
	// '<' expands to six bytes in JSON. Nine maximal replies represent the
	// initial reply plus every supported hook continuation, not one oversize
	// new response. The old generic 2 MiB raw-input cap rejected this payload.
	reply := strings.Repeat("<", relay.MaxBodyBytes)
	previous := strings.TrimSuffix(strings.Repeat(reply+"\n", relay.MaxBlocks), "\n")
	full := previous + "\n" + reply
	payload, err := json.Marshal(map[string]any{
		"hook_event_name": "AfterAgent", "session_id": "unbound-session", "cwd": root,
		"prompt_response": full, "stop_hook_active": true,
	})
	if err != nil || len(payload) <= 2<<20 || len(payload) >= maxGeminiHookBytes {
		t.Fatalf("invalid cumulative fixture: bytes=%d err=%v", len(payload), err)
	}
	var out, diagnostic bytes.Buffer
	if err := Run(context.Background(), []string{"hook", "--runtime", "gemini"}, bytes.NewReader(payload), &out, &diagnostic); err != nil || strings.TrimSpace(out.String()) != "{}" {
		t.Fatalf("bounded cumulative payload rejected: %v %s", err, diagnostic.String())
	}
	hook, err := decodeGeminiHook(payload)
	if err != nil {
		t.Fatal(err)
	}
	_, cursor, err := geminiResponse(HookInput{LastAssistantMessage: &previous}, nil)
	if err != nil {
		t.Fatal(err)
	}
	text, _, err := geminiResponse(hook, cursor)
	if err != nil || text != reply {
		t.Fatalf("large cumulative payload did not isolate the complete new reply: bytes=%d err=%v", len(text), err)
	}
	for _, test := range []struct {
		kind  string
		limit int
	}{{"gemini", maxGeminiHookBytes}, {"claude", 2 << 20}} {
		input := strings.NewReader(strings.Repeat(" ", test.limit+4096))
		out.Reset()
		err := Run(context.Background(), []string{"hook", "--runtime", test.kind}, input, &out, &diagnostic)
		if err == nil || !strings.Contains(err.Error(), "payload exceeds limit") || input.Len() != 4095 || out.Len() != 0 {
			t.Fatalf("%s exceeded its bounded read: unread=%d err=%v", test.kind, input.Len(), err)
		}
	}
}

func TestGeminiKnownStateWriteFailureStopsNativeContinuation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory write-permission failure")
	}
	for _, oversized := range []bool{false, true} {
		t.Run(map[bool]string{false: "reservation", true: "cursor-invalidation"}[oversized], func(t *testing.T) {
			f := newForegroundFixture(t, foregroundFixtureOptions{})
			dir := filepath.Dir(f.statePath)
			c, err := load(dir)
			if err != nil {
				t.Fatal(err)
			}
			previous := "retained prior response"
			_, cursor, err := geminiResponse(HookInput{LastAssistantMessage: &previous}, nil)
			if err != nil {
				t.Fatal(err)
			}
			next := c.State
			next.Runtime, next.GeminiResponse = model.RuntimeGemini, cursor
			next.LastSeq, next.LastConfirmedSeq = 1, 1
			if err := c.persist(next); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			if probe, err := os.CreateTemp(dir, "permission-probe"); err == nil {
				_ = probe.Close()
				_ = os.Remove(probe.Name())
				t.Skip("current user bypasses directory write permissions")
			}
			newText := "@codex response that cannot be saved"
			if oversized {
				newText += strings.Repeat("x", relay.MaxBodyBytes)
			}
			payload, _ := json.Marshal(map[string]any{
				"hook_event_name": "AfterAgent", "session_id": "session", "cwd": f.args[1],
				"prompt_response": previous + "\n" + newText, "stop_hook_active": true,
			})
			var out, diagnostic bytes.Buffer
			err = Run(context.Background(), []string{"hook", "--runtime", "gemini"}, bytes.NewReader(payload), &out, &diagnostic)
			var decision struct {
				Continue   *bool  `json:"continue"`
				StopReason string `json:"stopReason"`
			}
			if err != nil || json.Unmarshal(out.Bytes(), &decision) != nil || decision.Continue == nil || *decision.Continue || decision.StopReason == "" {
				t.Fatalf("failed write became Gemini's nonblocking exit warning: %s %v", out.String(), err)
			}
			if f.count("wait") != 0 || f.count("report") != 0 || diagnostic.Len() == 0 || strings.Contains(out.String(), newText) {
				t.Fatalf("failed write collected, published or exposed text: %s %s", out.String(), diagnostic.String())
			}
			var saved State
			if err := readPrivate(f.statePath, &saved); err != nil || saved.LastSeq != 1 || saved.GeminiResponse == nil || *saved.GeminiResponse != *cursor {
				t.Fatalf("failed write changed prior retained identity: %+v %v", saved, err)
			}
		})
	}
}

func TestGeminiSessionLookupFailureStopsNativeContinuation(t *testing.T) {
	f := newForegroundFixture(t, foregroundFixtureOptions{})
	c, err := load(filepath.Dir(f.statePath))
	if err != nil {
		t.Fatal(err)
	}
	next := c.State
	next.Runtime = model.RuntimeGemini
	if err := c.persist(next); err != nil {
		t.Fatal(err)
	}
	if err := rememberSession(c.State); err != nil {
		t.Fatal(err)
	}
	dir, err := locatorDirectory(nativeCaller{runtime: model.RuntimeGemini, session: c.State.SessionID}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, locatorFilename(c.State)), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{
		"hook_event_name": "AfterAgent", "session_id": "session", "cwd": f.args[1],
		"prompt_response": "@codex response behind a corrupt own-session locator",
	})
	var out, diagnostic bytes.Buffer
	err = Run(context.Background(), []string{"hook", "--runtime", "gemini"}, bytes.NewReader(payload), &out, &diagnostic)
	if err != nil || !strings.Contains(out.String(), `"continue":false`) || !strings.Contains(diagnostic.String(), "session locator") || f.count("wait") != 0 || f.count("report") != 0 {
		t.Fatalf("lookup failure did not stop before publication/collection: %s %s %v", out.String(), diagnostic.String(), err)
	}
}

// Use the actual installed Node process and /proc ancestry, not an invented
// process-table row. Node 24+ may expose comm=MainThread while title=node.
// This exercises official-shaped hook metadata, not authenticated vendor E2E.
func TestGeminiLiveNodeCaller(t *testing.T) {
	if os.Getenv("PAIRROOM_GEMINI_CALLER_HELPER") == "1" {
		payload := `{"hook_event_name":"BeforeTool","session_id":"live-node-session","tool_name":"run_shell_command","tool_input":{"command":"pairroom relay preflight"}}`
		var out, diagnostic bytes.Buffer
		if err := Run(context.Background(), []string{"hook", "--runtime", "gemini"}, strings.NewReader(payload), &out, &diagnostic); err != nil || diagnostic.Len() != 0 {
			t.Fatalf("BeforeTool: %v %s", err, diagnostic.String())
		}
		caller, err := currentNativeCaller()
		if err != nil || caller.runtime != model.RuntimeGemini || caller.session != "live-node-session" {
			t.Fatalf("real Node caller: %+v %v", caller, err)
		}
		return
	}
	if runtime.GOOS != "linux" {
		t.Skip("Linux /proc regression")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is not installed")
	}
	IsolateNativeCaller(t)
	t.Setenv("PAIRROOM_GEMINI_CALLER_HELPER", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := `const cp = require('node:child_process'); const fs = require('node:fs'); console.log(JSON.stringify({title:process.title,comm:fs.readFileSync('/proc/self/comm','utf8')})); const r=cp.spawnSync(process.argv[1],['-test.run=^TestGeminiLiveNodeCaller$','-test.v'],{encoding:'utf8'}); process.stdout.write(r.stdout||''); process.stderr.write(r.stderr||''); process.exit(r.status === null ? 1 : r.status);`
	output, err := exec.Command(node, "-e", script, exe).CombinedOutput()
	if err != nil {
		t.Fatalf("live Node ancestry: %v\n%s", err, output)
	}
	t.Log(string(output))
}
