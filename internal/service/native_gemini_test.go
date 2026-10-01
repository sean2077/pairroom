package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Real CLI/HTTP/Registry/journal, synthetic vendor sessions. This does not
// certify an installed, authenticated Gemini process or its consent UI.
func geminiNativeHTTP(t *testing.T, peer model.RuntimeKind) *nativeFixture {
	t.Helper()
	f := nativeHTTP(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GEMINI_CLI_HOME", home)
	noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		t.Error("Gemini Native spawned an adapter")
		return Binding{}, nil, errors.New("must not spawn")
	})
	created, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: f.project.ID, Name: "Gemini pair", HostMode: model.HostNative,
		Agents: map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: model.RuntimeGemini}, model.ActorSlot2: {Runtime: peer}},
	}, noSpawn)
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := f.manager.Activate(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.room, f.native = created, rt.(*nativeHostRuntime)
	return f
}

func geminiTestRun(t *testing.T, f *nativeFixture, kind model.RuntimeKind, session string, args []string, input any) ([]byte, error) {
	t.Helper()
	clearNativeSessionEnv(t)
	t.Setenv("GEMINI_CLI", "")
	t.Setenv("GEMINI_SESSION_ID", "")
	t.Setenv("PAIRROOM_GEMINI_SESSION_ID", "")
	if kind == model.RuntimeGemini {
		t.Setenv("GEMINI_CLI", "1")
		// This is the output of the approved BeforeTool identity bridge, not
		// an assumption that Gemini exports session_id to arbitrary children.
		t.Setenv("PAIRROOM_GEMINI_SESSION_ID", session)
	} else if kind != "" {
		t.Setenv(sessionEnvVar(kind), session)
	}
	return f.exec(t, args, input)
}

func geminiTestBind(t *testing.T, f *nativeFixture, slot model.ActorID) relay.Auth {
	t.Helper()
	kind := f.room.Agents[slot].Runtime
	session := "official-gemini-test-" + string(slot)
	if out, err := geminiTestRun(t, f, "", "", []string{"install", "--runtime", string(kind)}, nil); err != nil {
		t.Fatalf("install: %s %v", out, err)
	}
	out, err := geminiTestRun(t, f, kind, session, []string{"bind", "--room", f.room.ID, "--slot", string(slot), "--service-file", f.endpoint}, nil)
	var result struct {
		Binding relay.Binding `json:"binding"`
	}
	if err != nil || json.Unmarshal(out, &result) != nil || result.Binding.SessionID != session {
		t.Fatalf("bind: %s %v", out, err)
	}
	data, err := os.ReadFile(filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", string(slot), "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	var cred struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(data, &cred); err != nil || cred.Secret == "" {
		t.Fatal("missing credential")
	}
	if strings.Contains(string(out), cred.Secret) {
		t.Fatal("bind leaked credential")
	}
	if err := f.native.engine.Park(slot, false); err != nil {
		t.Fatal(err)
	}
	return relay.Auth{Slot: slot, BindID: result.Binding.BindID, Generation: result.Binding.Generation, Secret: cred.Secret, SessionID: session}
}

func TestGeminiNativeRoundTripAndAfterAgentContinuation(t *testing.T) {
	for _, peerKind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok, model.RuntimeGemini} {
		t.Run(string(peerKind), func(t *testing.T) {
			f := geminiNativeHTTP(t, peerKind)
			a, b := geminiTestBind(t, f, model.ActorSlot1), geminiTestBind(t, f, model.ActorSlot2)
			opening := "Review the exact revision: 完整消息🌟\nsecond line"
			if _, err := geminiTestRun(t, f, peerKind, b.SessionID, []string{"send", "--id", "opening", "--text", opening}, nil); err != nil {
				t.Fatal(err)
			}
			out, err := geminiTestRun(t, f, model.RuntimeGemini, a.SessionID, []string{"exchange", "--id", "findings", "--text", "First finding", "--timeout", "1"}, nil)
			if err != nil || !strings.HasSuffix(string(out), opening+"\n") {
				t.Fatalf("exchange before AfterAgent: %s %v", out, err)
			}
			out, err = geminiTestRun(t, f, peerKind, b.SessionID, []string{"wait", "--timeout", "1"}, nil)
			if err != nil || !strings.HasSuffix(string(out), "First finding\n") {
				t.Fatalf("peer delivery: %s %v", out, err)
			}
			if _, err := f.native.engine.Send(b, relay.SendRequest{ID: "continue", Text: opening}); err != nil {
				t.Fatal(err)
			}
			if err := f.native.engine.Park(a.Slot, true); err != nil {
				t.Fatal(err)
			}
			ids := model.ParticipantIdentities(map[model.ActorID]model.RuntimeKind{a.Slot: model.RuntimeGemini, b.Slot: peerKind})
			full := strings.Repeat("完整回复🌟 ", 1000) + ids[b.Slot].MentionHandle
			input := map[string]any{"hook_event_name": "AfterAgent", "session_id": a.SessionID, "cwd": f.project.Root, "prompt_response": full, "stop_hook_active": false}
			out, err = geminiTestRun(t, f, model.RuntimeGemini, a.SessionID, []string{"hook", "--runtime", "gemini"}, input)
			var decision struct{ Decision, Reason string }
			if err != nil || json.Unmarshal(out, &decision) != nil || decision.Decision != "deny" || !strings.HasSuffix(decision.Reason, opening) {
				t.Fatalf("AfterAgent continuation: %s %v", out, err)
			}
			published := false
			for _, message := range f.native.engine.Snapshot().Messages {
				if message.From == a.Slot && message.Text == full && message.To == b.Slot {
					published = true
				}
				if message.Text == opening && message.State != "handed_off" {
					t.Fatal("Gemini stdout not acknowledged")
				}
			}
			if !published {
				t.Fatal("AfterAgent lost or truncated the complete response")
			}
			out, err = geminiTestRun(t, f, model.RuntimeGemini, a.SessionID, []string{"bind"}, nil)
			var resumed struct {
				Binding relay.Binding `json:"binding"`
			}
			if err != nil || json.Unmarshal(out, &resumed) != nil || resumed.Binding.BindID != a.BindID || resumed.Binding.Generation != a.Generation {
				t.Fatalf("resume changed identity: %s %v", out, err)
			}
		})
	}
}

func TestGeminiPassiveAndUnboundHooksHaveNoDurableEffects(t *testing.T) {
	f := geminiNativeHTTP(t, model.RuntimeCodex)
	a := geminiTestBind(t, f, model.ActorSlot1)
	before := f.native.engine.Snapshot()
	for _, event := range []string{"BeforeTool", "SessionStart", "SessionEnd", "Stop", "StopFailure", "AfterModel", "AfterAgent"} {
		session := a.SessionID
		if event == "AfterAgent" {
			session = "unbound-session"
		}
		out, err := geminiTestRun(t, f, model.RuntimeGemini, session, []string{"hook", "--runtime", "gemini"}, map[string]any{
			"hook_event_name": event, "session_id": session, "cwd": f.project.Root, "prompt_response": "@codex must not relay", "tool_name": "run_shell_command", "tool_input": map[string]any{"command": "git status"},
		})
		if err != nil || strings.TrimSpace(string(out)) != "{}" || !reflect.DeepEqual(before, f.native.engine.Snapshot()) {
			t.Fatalf("passive event %s changed state: %s %v", event, out, err)
		}
	}
	out, err := geminiTestRun(t, f, model.RuntimeGemini, a.SessionID, []string{"hook", "--runtime", "gemini"}, map[string]any{"hook_event_name": "AfterAgent", "session_id": "wrong-session", "cwd": f.project.Root, "prompt_response": "@codex must not publish"})
	if err == nil || len(out) != 0 || !reflect.DeepEqual(before, f.native.engine.Snapshot()) {
		t.Fatalf("identity mismatch had effects: %s %v", out, err)
	}
}

func TestGeminiCreateRequiresBridgeIdentityBeforeProvisioning(t *testing.T) {
	f := geminiNativeHTTP(t, model.RuntimeCodex)
	before := f.registry.Snapshot(true)
	out, err := geminiTestRun(t, f, model.RuntimeGemini, "", []string{"bind", "--create", "--peer-runtime", "codex", "--service-file", f.endpoint}, nil)
	if err == nil || !strings.Contains(err.Error(), "PAIRROOM_GEMINI_SESSION_ID is missing") || len(out) != 0 || !reflect.DeepEqual(before.Rooms, f.registry.Snapshot(true).Rooms) {
		t.Fatalf("identity-free creation: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(f.project.Root, ".pairroom")); !os.IsNotExist(err) {
		t.Fatal("missing identity wrote binding state")
	}
}
