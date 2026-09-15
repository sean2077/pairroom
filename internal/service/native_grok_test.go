package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

// Real Service/CLI and durable state with vendor-shaped hook fixtures. This is
// not authenticated Grok execution or a claim about its tool-output limits.
func grokNativeHTTP(t *testing.T, peer model.RuntimeKind) *nativeFixture {
	t.Helper()
	f := nativeHTTP(t)
	t.Setenv("GROK_HOME", "")
	noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		t.Error("Grok Native spawned an adapter")
		return Binding{}, nil, errors.New("must not spawn")
	})
	room, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: f.project.ID, Name: "Grok pair", HostMode: model.HostNative,
		Agents: map[model.ActorID]model.AgentSelection{model.ActorClaude: {Runtime: model.RuntimeGrok}, model.ActorCodex: {Runtime: peer}},
	}, noSpawn)
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := f.manager.Activate(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.room, f.native = room, rt.(*nativeHostRuntime)
	return f
}

func grokHook(t *testing.T, f *nativeFixture, a relay.Auth, text string, extra map[string]any) ([]byte, error) {
	t.Helper()
	fields := map[string]any{"hookEventName": "stop", "hook_event_name": "Stop", "sessionId": a.SessionID, "cwd": f.project.Root, "workspaceRoot": f.project.Root, "reason": "end_turn", "lastAssistantMessage": text}
	for key, value := range extra {
		fields[key] = value
	}
	return f.run(t, []string{"hook", "--runtime", "grok"}, fields)
}

func associateGrok(t *testing.T, f *nativeFixture, slot model.ActorID) relay.Auth {
	t.Helper()
	a, nonce := f.bind(t, slot)
	if err := f.native.engine.Park(slot, false); err != nil {
		t.Fatal(err)
	}
	if _, err := grokHook(t, f, a, nonce, nil); err != nil {
		t.Fatal(err)
	}
	if b, err := f.native.engine.Inspect(a); err != nil || b.SessionID != a.SessionID {
		t.Fatalf("Grok official nonce association failed: %+v %v", b, err)
	}
	return a
}

func TestGrokNativeRoundTripDefersFullInboxToForeground(t *testing.T) {
	f := grokNativeHTTP(t, model.RuntimeCodex)
	grok := associateGrok(t, f, model.ActorClaude)
	peer := associateCLI(t, f, model.ActorCodex)
	text := strings.Repeat("完整消息🌟", 4000) // longer than Grok's 10k hook feedback
	incoming, err := f.native.engine.Send(peer, relay.SendRequest{ID: "opening", Text: text})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.native.engine.Park(grok.Slot, true); err != nil {
		t.Fatal(err)
	}
	output, err := grokHook(t, f, grok, "ready @codex", nil)
	if err != nil || !strings.Contains(string(output), `"decision":"block"`) || !strings.Contains(string(output), "pairroom relay wait") || strings.Contains(string(output), "完整消息") || len(output) > 8000 {
		t.Fatalf("unsafe readiness hint: %q %v", output, err)
	}
	for _, message := range f.native.engine.Snapshot().Messages {
		if message.ID == incoming.ID && message.State != "queued" {
			t.Fatal("hook claimed a message it did not deliver")
		}
	}
	t.Setenv("GROK_SESSION_ID", grok.SessionID)
	output, err = f.run(t, []string{"wait", "--timeout", "1"}, nil)
	if err != nil || !strings.HasSuffix(string(output), text+"\n") {
		t.Fatalf("foreground truncated the original: %v", err)
	}
	for _, message := range f.native.engine.Snapshot().Messages {
		if message.ID == incoming.ID && message.State != "handed_off" {
			t.Fatal("foreground not acknowledged")
		}
	}
	// Reuse the exact binding with exchange: full reply goes through send, while
	// user steering remains the next FIFO input rather than a matched RPC reply.
	_, err = f.native.engine.SendUser(relay.SendRequest{ID: "user-steer", To: grok.Slot, Text: "User changed the review scope"})
	if err != nil {
		t.Fatal(err)
	}
	output, err = f.run(t, []string{"exchange", "--id", "long-findings", "--text", text, "--timeout", "1"}, nil)
	if err != nil || !strings.Contains(string(output), "from: @user") {
		t.Fatalf("Grok exchange lost user steering: %s %v", output, err)
	}
	snapshot := f.native.engine.Snapshot()
	found := false
	for _, message := range snapshot.Messages {
		if message.From == grok.Slot && message.Text == text && message.To == peer.Slot {
			found = true
		}
	}
	if !found {
		t.Fatal("Grok explicit publication lost full reply")
	}
	// Ordinary bind restores the associated session and endpoint, no new nonce.
	output, err = f.run(t, []string{"bind"}, nil)
	if err != nil || strings.Contains(string(output), `"bind_nonce"`) {
		t.Fatalf("Grok resume failed: %s %v", output, err)
	}
}

func TestGrokClippedStopIsNotPublishedAndPassiveEventsStayPassive(t *testing.T) {
	f := grokNativeHTTP(t, model.RuntimeClaude)
	a := associateGrok(t, f, model.ActorClaude)
	seq := f.native.engine.Snapshot().Sequence
	for _, extra := range []map[string]any{
		{"reason": "shutdown"}, {"reason": "channel_closed"}, {"subagentType": "explore"},
		{"hookEventName": "stop_cancelled", "hook_event_name": "StopCancelled", "reason": "user_interrupt"},
	} {
		out, err := grokHook(t, f, a, "@claude private partial", extra)
		if err != nil || strings.TrimSpace(string(out)) != "{}" || f.native.engine.Snapshot().Sequence != seq {
			t.Fatalf("passive Grok event had effects: %s %v", out, err)
		}
	}
	out, err := grokHook(t, f, a, "@claude prefix… [+123 chars]", nil)
	if err != nil || !strings.Contains(string(out), "COMPLETE original text") || f.native.engine.Snapshot().Sequence != seq {
		t.Fatalf("clipped prefix was published: %q %v", out, err)
	}
	if len(f.native.engine.Snapshot().Messages) != 0 {
		t.Fatal("truncated hook wrote a message")
	}
	// Imported Claude compatibility hook must not duplicate or misassociate it.
	out, err = f.run(t, []string{"hook", "--runtime", "claude"}, map[string]any{
		"hookEventName": "stop", "hook_event_name": "Stop", "sessionId": a.SessionID, "cwd": f.project.Root,
		"reason": "end_turn", "lastAssistantMessage": "@claude do not double publish",
	})
	if err != nil || strings.TrimSpace(string(out)) != "{}" || f.native.engine.Snapshot().Sequence != seq {
		t.Fatal("compatibility hook duplicated relay")
	}
	// Failure records a safe category only, never error text or partial replies.
	out, err = grokHook(t, f, a, "PRIVATE error with @claude", map[string]any{"hookEventName": "stop_failure", "hook_event_name": "StopFailure", "error": "authentication_failed"})
	if err != nil || strings.TrimSpace(string(out)) != "{}" {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(f.native.engine.Snapshot())
	if strings.Contains(string(snapshot), "PRIVATE") || len(f.native.engine.Snapshot().Messages) != 0 || !strings.Contains(string(snapshot), "authentication_failed") {
		t.Fatal("failure leaked content or lost its allowlisted category")
	}
}

func TestTwoNativeGrokSessionsUseDistinctSlotsAndHandles(t *testing.T) {
	f := grokNativeHTTP(t, model.RuntimeGrok)
	a := associateGrok(t, f, model.ActorClaude)
	b := associateGrok(t, f, model.ActorCodex)
	for i, sender := range []relay.Auth{a, b} {
		to := model.OtherParticipant(sender.Slot)
		text := fmt.Sprintf("@grok%d review this", 1-i)
		if _, err := grokHook(t, f, sender, text, nil); err != nil {
			t.Fatal(err)
		}
		messages := f.native.engine.Snapshot().Messages
		last := messages[len(messages)-1]
		if last.From != sender.Slot || last.To != to || last.Text != text {
			t.Fatal("Grok runtime became a third slot or routed to self")
		}
		t.Setenv("GROK_SESSION_ID", sender.SessionID)
		output, err := f.run(t, []string{"bind"}, nil)
		if err != nil || strings.Contains(string(output), `"bind_nonce"`) {
			t.Fatal("same-runtime session resume failed")
		}
		t.Setenv("GROK_SESSION_ID", "")
	}
	// Old-generation credentials cannot consume the inbox after replacement.
	_, err := f.native.engine.Bind(a.Slot, relay.BindRequest{BindID: "replacement", CredentialHash: relay.Digest("secret"), NonceHash: relay.Digest("nonce"), Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := f.native.engine.WaitForPending(context.Background(), a); ready || !errors.Is(err, relay.ErrAuth) {
		t.Fatal("old Grok generation retained readiness access")
	}
}

func TestGrokNativeCreateInsideHarnessWithoutIdentityFlags(t *testing.T) {
	for _, useProfile := range []bool{false, true} {
		t.Run(fmt.Sprintf("default-profile=%v", useProfile), func(t *testing.T) {
			f := nativeHTTP(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("APPDATA", filepath.Join(home, "AppData"))
			t.Setenv("GROK_HOME", filepath.Join(home, "custom-grok"))
			t.Setenv("GROK_SESSION_ID", "grok-creator")
			t.Chdir(f.project.Root)
			config, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			config = filepath.Join(config, "pairroom")
			if err := os.MkdirAll(config, 0700); err != nil {
				t.Fatal(err)
			}
			if err := relay.WriteEndpoint(config, relay.Endpoint{URL: f.server.URL, Token: "management-secret"}); err != nil {
				t.Fatal(err)
			}
			args := []string{"bind", "--create", "--name", "Harness-created Grok pair"}
			wantSlot := model.ActorClaude
			if useProfile {
				// A reversed Service pair must not be silently rewritten into
				// vendor-named slots just because the caller is Grok.
				_, err = f.registry.SaveAgentPairProfile(context.Background(), "", AgentPairProfileInput{Name: "Grok default", IsDefault: true,
					Agents: map[model.ActorID]model.AgentSelection{model.ActorClaude: {Runtime: model.RuntimeCodex}, model.ActorCodex: {Runtime: model.RuntimeGrok}},
				})
				if err != nil {
					t.Fatal(err)
				}
				wantSlot = model.ActorCodex
			} else {
				args = append(args, "--peer-runtime", "codex")
			}
			var out bytes.Buffer
			run := func(args []string) error {
				out.Reset()
				return relayclient.Run(context.Background(), args, strings.NewReader(""), &out, io.Discard)
			}
			if err := run([]string{"install"}); err != nil {
				t.Fatal(err)
			}
			if err := run(args); err != nil {
				t.Fatal(err)
			}
			var created struct {
				Binding relay.Binding `json:"binding"`
				Nonce   string        `json:"bind_nonce"`
			}
			if json.Unmarshal(out.Bytes(), &created) != nil || created.Nonce == "" || created.Binding.Slot != wantSlot || created.Binding.SessionID != "" {
				t.Fatalf("create guessed identity or bypassed nonce: %s", out.String())
			}
			for _, room := range f.registry.Snapshot(true).Rooms {
				if room.Name == "Harness-created Grok pair" {
					f.room = room
				}
			}
			if f.room.Agents[wantSlot].Runtime != model.RuntimeGrok {
				t.Fatal("caller/runtime selection lost")
			}
			rt, _, err := f.manager.Activate(context.Background(), f.room.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.native = rt.(*nativeHostRuntime)
			if err := f.native.engine.Park(wantSlot, false); err != nil {
				t.Fatal(err)
			}
			if _, err := grokHook(t, f, relay.Auth{Slot: wantSlot, SessionID: "grok-creator"}, created.Nonce, nil); err != nil {
				t.Fatal(err)
			}
			if err := run([]string{"bind"}); err != nil || strings.Contains(out.String(), `"bind_nonce"`) {
				t.Fatalf("zero-flag resume failed: %s %v", out.String(), err)
			}
			if err := f.manager.Suspend(context.Background(), f.room.ID); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: f.registry.Root()})
			if err != nil {
				t.Fatal(err)
			}
			room, ok := reopened.Room(f.room.ID)
			if !ok || room.HostMode != model.HostNative || room.Agents[wantSlot].Runtime != model.RuntimeGrok || room.Bindings[wantSlot].SessionID != "grok-creator" {
				t.Fatal("Grok selection/association did not survive replay")
			}
		})
	}
}
