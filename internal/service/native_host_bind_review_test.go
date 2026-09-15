package service

import (
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

// Synthetic vendor inputs through the actual CLI/HTTP/Registry/Engine path.
// These regressions are deliberately not described as real vendor E2E.
func TestNativeRejectedCLIBindKeepsOriginalRelayUsable(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorClaude)
	b := associateCLI(t, f, model.ActorCodex)
	pair := defaultAgentSelections()
	pair[model.ActorCodex] = pair[model.ActorClaude]
	other, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: f.project.ID, Name: "duplicate target", HostMode: model.HostNative, Agents: pair}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.manager.Activate(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", a.SessionID)
	if _, err := f.run(t, []string{"bind", "--room", other.ID, "--slot", "2", "--runtime", "claude", "--service-file", f.endpoint}, nil); err == nil {
		t.Fatal("duplicate session accepted")
	}
	if _, err := os.Stat(filepath.Join(f.project.Root, ".pairroom", "rooms", other.ID, "slots", "codex", "state.json")); !os.IsNotExist(err) {
		t.Fatal("rejected bind created discoverable state")
	}
	if _, err := f.hook(t, a, "@codex original publication survives", false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.native.engine.Send(b, relay.SendRequest{ID: "original-receive", Text: "original collection survives"}); err != nil {
		t.Fatal(err)
	}
	if err := f.native.engine.Park(a.Slot, true); err != nil {
		t.Fatal(err)
	}
	out, err := f.hook(t, a, "current contribution complete", false)
	if err != nil || !strings.Contains(string(out), "original collection survives") {
		t.Fatalf("original hook stopped collecting: %s %v", out, err)
	}
}

func TestNativeRejectedReplaceKeepsOldLocalBinding(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorClaude)
	other, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: f.project.ID, Name: "replace target", HostMode: model.HostNative}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.manager.Activate(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	args := []string{"bind", "--room", other.ID, "--slot", "1", "--runtime", "claude", "--service-file", f.endpoint}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "other-official-session")
	beforeResult, err := f.run(t, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	var beforeBinding struct {
		Binding relay.Binding `json:"binding"`
	}
	if err := json.Unmarshal(beforeResult, &beforeBinding); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.project.Root, ".pairroom", "rooms", other.ID, "slots", "claude")
	before := map[string]string{}
	for _, name := range []string{"state.json", "credentials"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = string(data)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", a.SessionID)
	if _, err := f.run(t, append(append([]string{}, args...), "--replace"), nil); err == nil {
		t.Fatal("globally conflicting replace accepted")
	}
	for name, expected := range before {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != expected {
			t.Fatalf("rejected replace changed %s", name)
		}
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "other-official-session")
	afterResult, err := f.run(t, args, nil)
	if err != nil {
		t.Fatal(err)
	}
	var afterBinding struct {
		Binding relay.Binding `json:"binding"`
	}
	if err := json.Unmarshal(afterResult, &afterBinding); err != nil {
		t.Fatal(err)
	}
	if beforeBinding.Binding.BindID != afterBinding.Binding.BindID || beforeBinding.Binding.Generation != afterBinding.Binding.Generation {
		t.Fatal("rejected replace forced a generation rotation")
	}
}

func TestNativeHookIdentityMismatchIsVisibleWithoutEffects(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorClaude)
	t.Setenv("CLAUDE_CODE_SESSION_ID", a.SessionID)
	before := f.native.engine.Snapshot()
	out, err := f.run(t, []string{"hook", "--runtime", "claude"}, map[string]any{"hook_event_name": "Stop", "session_id": "different-hook-session", "cwd": f.project.Root, "last_assistant_message": "@codex must never publish", "stop_hook_active": false})
	if err == nil || !strings.Contains(err.Error(), "different session identity") || len(out) != 0 {
		t.Fatalf("identity mismatch was silently ignored: %s %v", out, err)
	}
	if !reflect.DeepEqual(before, f.native.engine.Snapshot()) {
		t.Fatal("mismatched hook changed relay state")
	}
}
