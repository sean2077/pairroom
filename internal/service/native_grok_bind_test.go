package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// The CLI, HTTP service and journal are real; harness identities are synthetic.
// No Stop hook is fired before both participants exchange their first messages.
func TestGrokBindEnablesExchangeBeforeFirstStop(t *testing.T) {
	for _, peerKind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		t.Run(string(peerKind), func(t *testing.T) {
			f := grokNativeHTTP(t, peerKind)
			a, b := f.bind(t, model.ActorClaude), f.bind(t, model.ActorCodex)
			for _, audit := range f.native.engine.Snapshot().Audit {
				if strings.Contains(audit.Kind, "publication") {
					t.Fatal("binding required a Stop publication")
				}
			}
			opening := "Review this exact revision"
			if _, err := f.runAs(t, peerKind, b.SessionID, []string{"send", "--id", "opening", "--text", opening}, nil); err != nil {
				t.Fatal(err)
			}
			out, err := f.runAs(t, model.RuntimeGrok, a.SessionID, []string{"exchange", "--id", "findings", "--text", "Check the crash window", "--timeout", "1"}, nil)
			if err != nil || !strings.HasSuffix(string(out), opening+"\n") {
				t.Fatalf("exchange before first Stop: %s %v", out, err)
			}
			out, err = f.runAs(t, peerKind, b.SessionID, []string{"wait", "--timeout", "1"}, nil)
			if err != nil || !strings.HasSuffix(string(out), "Check the crash window\n") {
				t.Fatalf("peer cannot collect before first Stop: %s %v", out, err)
			}
			for _, message := range f.native.engine.Snapshot().Messages {
				if message.State != "handed_off" {
					t.Fatalf("foreground receipt lost: %s", message.State)
				}
			}
			out, err = f.runAs(t, model.RuntimeGrok, a.SessionID, []string{"bind"}, nil)
			var resumed struct {
				Binding relay.Binding `json:"binding"`
			}
			if err != nil || json.Unmarshal(out, &resumed) != nil || resumed.Binding.BindID != a.BindID || resumed.Binding.Generation != a.Generation || bytes.Contains(out, []byte("bind_nonce")) {
				t.Fatalf("same-session resume rotated identity: %s %v", out, err)
			}
		})
	}
}

func TestGrokHookConfirmsBoundIdentityWithoutImplicitAssociation(t *testing.T) {
	f := grokNativeHTTP(t, model.RuntimeCodex)
	a := bindGrok(t, f, model.ActorClaude)
	before := f.native.engine.Snapshot()
	input := map[string]any{"hookEventName": "stop", "hook_event_name": "Stop", "sessionId": "different-session", "cwd": f.project.Root, "workspaceRoot": f.project.Root, "reason": "end_turn", "lastAssistantMessage": "@codex must not publish"}
	out, err := f.runAs(t, model.RuntimeGrok, a.SessionID, []string{"hook", "--runtime", "grok"}, input)
	if err == nil || !strings.Contains(err.Error(), "different session identity") || len(out) != 0 || !reflect.DeepEqual(before, f.native.engine.Snapshot()) {
		t.Fatalf("Grok identity mismatch did not fail without effects: %s %v", out, err)
	}
	// A different, genuinely unbound session is ignored, not associated by Stop.
	out, err = f.runAs(t, model.RuntimeGrok, "different-session", []string{"hook", "--runtime", "grok"}, input)
	if err != nil || strings.TrimSpace(string(out)) != "{}" || !reflect.DeepEqual(before, f.native.engine.Snapshot()) {
		t.Fatalf("unbound Grok hook acquired identity: %s %v", out, err)
	}
}

func TestGrokCreateWithoutSessionHasNoLocalOrDurableEffects(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GROK_HOME", "")
	f := nativeHTTP(t)
	before := f.registry.Snapshot(true)
	if _, err := f.run(t, []string{"install", "--runtime", "grok"}, nil); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, []string{"bind", "--create", "--runtime", "grok", "--peer-runtime", "codex", "--service-file", f.endpoint}, nil)
	if err == nil || !strings.Contains(err.Error(), "GROK_SESSION_ID is missing") || len(out) != 0 || !reflect.DeepEqual(before.Rooms, f.registry.Snapshot(true).Rooms) {
		t.Fatalf("identity-free Grok create had effects: %s %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(f.project.Root, ".pairroom")); !os.IsNotExist(err) {
		t.Fatal("missing identity created local binding state")
	}
}

func TestGrokRejectedReplacePreservesCommittedBinding(t *testing.T) {
	f := grokNativeHTTP(t, model.RuntimeCodex)
	a := bindGrok(t, f, model.ActorClaude)
	other, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: f.project.ID, Name: "other Grok", HostMode: model.HostNative, Agents: f.room.Agents}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"bind", "--room", other.ID, "--slot", "1", "--service-file", f.endpoint}
	if _, err := f.runAs(t, model.RuntimeGrok, "other-session", args, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.project.Root, ".pairroom", "rooms", other.ID, "slots", "claude")
	before := map[string][]byte{}
	for _, name := range []string{"credentials", "state.json"} {
		before[name], err = os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
	}
	if out, err := f.runAs(t, model.RuntimeGrok, a.SessionID, append(append([]string{}, args...), "--replace"), nil); err == nil || len(out) != 0 {
		t.Fatal("globally conflicting Grok replacement succeeded")
	}
	for name, want := range before {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("failed replacement overwrote %s", name)
		}
	}
	if _, err := f.runAs(t, model.RuntimeGrok, "other-session", args, nil); err != nil {
		t.Fatalf("old binding became unusable: %v", err)
	}
}
