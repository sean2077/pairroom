package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
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
