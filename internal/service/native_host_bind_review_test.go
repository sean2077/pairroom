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
	"github.com/sean2077/pairroom/internal/relayclient"
)

// Synthetic vendor inputs through the actual CLI/HTTP/Registry/Engine path.
// These regressions are deliberately not described as real vendor E2E.
func TestNativeRejectedCLIBindKeepsOriginalRelayUsable(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorSlot1)
	b := associateCLI(t, f, model.ActorSlot2)
	pair := defaultAgentSelections()
	pair[model.ActorSlot2] = pair[model.ActorSlot1]
	other, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: f.project.ID, Name: "duplicate target", HostMode: model.HostNative, Agents: pair}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.manager.Activate(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runAs(t, model.RuntimeClaude, a.SessionID, []string{"bind", "--room", other.ID, "--slot", "2", "--runtime", "claude", "--service-file", f.endpoint}, nil); err == nil {
		t.Fatal("duplicate session accepted")
	}
	if _, err := os.Stat(filepath.Join(f.project.Root, ".pairroom", "rooms", other.ID, "slots", "slot2", "state.json")); !os.IsNotExist(err) {
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

// Stop replies saved while the endpoint file is absent reach the real Engine
// in order under their original sequences, with no gap and no duplicate.
func TestNativeStopBacklogPublishesInOrderAfterServiceReturns(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorSlot1)
	associateCLI(t, f, model.ActorSlot2)
	endpoint, err := os.ReadFile(f.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.endpoint); err != nil {
		t.Fatal(err)
	}
	stopped := []string{"@codex first while stopped", "private unaddressed reply", "@codex third while stopped"}
	for _, text := range stopped {
		if _, err := f.hook(t, a, text, false); err == nil || !strings.Contains(err.Error(), "is PairRoom running") {
			t.Fatalf("stopped Service not reported: %v", err)
		}
	}
	if err := os.WriteFile(f.endpoint, endpoint, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, []string{"reconcile", "--room", f.room.ID, "--slot", "1"}, nil); err != nil {
		t.Fatal(err)
	}
	var peer []string
	for seq := uint64(1); seq <= 3; seq++ {
		p, ok, err := f.native.engine.Publication(a, seq)
		if err != nil || !ok || p.GapFrom != 0 {
			t.Fatalf("seq %d not published exactly: %+v ok=%v err=%v", seq, p, ok, err)
		}
		if p.Message != nil {
			peer = append(peer, p.Message.Text)
		}
	}
	// The unaddressed reply records only its receipt; routed bodies keep order.
	if got := strings.Join(peer, "|"); got != stopped[0]+"|"+stopped[2] {
		t.Fatalf("peer messages = %q", got)
	}
	var local relayclient.State
	data, err := os.ReadFile(filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", "slot1", "state.json"))
	if err != nil || json.Unmarshal(data, &local) != nil || local.Pending != nil || local.Held != nil || local.LastConfirmedSeq != 3 {
		t.Fatalf("local backlog not settled: %+v %v", local, err)
	}
}

// bind --replace starts a fresh local State; it must refuse while Stop replies
// are saved but unpublished, before revoking the generation that can still
// publish them, and succeed once the backlog is explicitly settled.
func TestNativeReplaceRefusesUnpublishedStopBacklog(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorSlot1)
	associateCLI(t, f, model.ActorSlot2)
	statePath := filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", "slot1", "state.json")
	endpoint, err := os.ReadFile(f.endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.endpoint); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"@codex saved head", "@codex saved held"} {
		if _, err := f.hook(t, a, text, false); err == nil {
			t.Fatal("stopped Service not reported")
		}
	}
	if err := os.WriteFile(f.endpoint, endpoint, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	replace := []string{"bind", "--room", f.room.ID, "--slot", "1", "--service-file", f.endpoint, "--replace"}
	out, err := f.runAs(t, model.RuntimeClaude, a.SessionID, replace, nil)
	if err == nil || !strings.Contains(err.Error(), "2 unpublished Stop replies") || !strings.Contains(err.Error(), "relay reconcile") || len(out) != 0 {
		t.Fatalf("replace discarded or hid the backlog: %s %v", out, err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("refused replace rewrote local state: %v", err)
	}
	if got := f.native.engine.Snapshot().Bindings[a.Slot]; got.Generation != a.Generation {
		t.Fatalf("refused replace rotated the binding: %+v", got)
	}
	if _, ok, _ := f.native.engine.Publication(a, 1); ok {
		t.Fatal("refused replace published as a side effect")
	}
	if _, err := f.run(t, []string{"reconcile", "--room", f.room.ID, "--slot", "1"}, nil); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 2; seq++ {
		if _, ok, err := f.native.engine.Publication(a, seq); err != nil || !ok {
			t.Fatalf("seq %d not published before replacement: %v", seq, err)
		}
	}
	out, err = f.runAs(t, model.RuntimeClaude, a.SessionID, replace, nil)
	if err != nil {
		t.Fatalf("replace after settling the backlog: %s %v", out, err)
	}
	if got := f.native.engine.Snapshot().Bindings[a.Slot]; got.Generation != a.Generation+1 {
		t.Fatalf("replacement did not rotate generation: %+v", got)
	}
}

func TestNativeRejectedReplaceKeepsOldLocalBinding(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorSlot1)
	other, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: f.project.ID, Name: "replace target", HostMode: model.HostNative}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.manager.Activate(context.Background(), other.ID); err != nil {
		t.Fatal(err)
	}
	args := []string{"bind", "--room", other.ID, "--slot", "1", "--runtime", "claude", "--service-file", f.endpoint}
	beforeResult, err := f.runAs(t, model.RuntimeClaude, "other-official-session", args, nil)
	if err != nil {
		t.Fatal(err)
	}
	var beforeBinding struct {
		Binding relay.Binding `json:"binding"`
	}
	if err := json.Unmarshal(beforeResult, &beforeBinding); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.project.Root, ".pairroom", "rooms", other.ID, "slots", "slot1")
	before := map[string]string{}
	for _, name := range []string{"state.json", "credentials"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = string(data)
	}
	if _, err := f.runAs(t, model.RuntimeClaude, a.SessionID, append(append([]string{}, args...), "--replace"), nil); err == nil {
		t.Fatal("globally conflicting replace accepted")
	}
	for name, expected := range before {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != expected {
			t.Fatalf("rejected replace changed %s", name)
		}
	}
	afterResult, err := f.runAs(t, model.RuntimeClaude, "other-official-session", args, nil)
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
	a := associateCLI(t, f, model.ActorSlot1)
	before := f.native.engine.Snapshot()
	out, err := f.runAs(t, model.RuntimeClaude, a.SessionID, []string{"hook", "--runtime", "claude"}, map[string]any{"hook_event_name": "Stop", "session_id": "different-hook-session", "cwd": f.project.Root, "last_assistant_message": "@codex must never publish", "stop_hook_active": false})
	if err == nil || !strings.Contains(err.Error(), "different session identity") || len(out) != 0 {
		t.Fatalf("identity mismatch was silently ignored: %s %v", out, err)
	}
	if !reflect.DeepEqual(before, f.native.engine.Snapshot()) {
		t.Fatal("mismatched hook changed relay state")
	}
}
