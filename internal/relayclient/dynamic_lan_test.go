package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func bindDynamicLANOwner(t *testing.T, f *lanCLIWire, id string, kind model.RuntimeKind, replace bool) relay.Auth {
	t.Helper()
	b, err := f.engine.Bind(model.ActorSlot1, relay.BindRequest{BindID: id, Runtime: kind, SessionID: "host-session-" + id, CredentialHash: relay.Digest("host-secret-" + id), Replace: replace})
	if err != nil {
		t.Fatal(err)
	}
	return relay.Auth{Slot: b.Slot, BindID: b.BindID, Generation: b.Generation, Runtime: b.Runtime, SessionID: b.SessionID, Secret: "host-secret-" + id}
}

func TestLANDynamicLateStopKeepsOriginalGenerationAcrossClaimAndLostReceipt(t *testing.T) {
	f := newLANCLIWire(t, true)
	bindDynamicLANOwner(t, f, "old-owner", model.RuntimeClaude, false)
	_, original := f.bind(t, "guest-session")
	if !original.DynamicBindings || original.AutomaticPeerGeneration != 1 {
		t.Fatalf("LAN join lost its original bootstrap context: %+v", original)
	}
	owner := bindDynamicLANOwner(t, f, "new-owner", model.RuntimeGrok, true)
	if _, err := f.engine.Send(owner, relay.SendRequest{ID: "new-host-context", Text: "new host request", ExpectedPeerGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	output, err := f.run(t, "guest-session", true, "wait", "--timeout", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"from: @agent1", "grok, generation 2", "peer: @agent1, generation 2", "Automatic publication is paused"} {
		if !strings.Contains(string(output), fragment) {
			t.Fatalf("LAN used stale join metadata instead of claim provenance %q: %s", fragment, output)
		}
	}
	text := "old host response @agent1"
	input, _ := json.Marshal(HookInput{Event: "Stop", SessionID: "guest-session", CWD: f.root, LastAssistantMessage: &text})
	f.dropReport.Store(true) // the host accepted peer_changed; this reply was lost
	var out, diagnostic bytes.Buffer
	if err := Run(context.Background(), []string{"hook", "--runtime", "codex"}, bytes.NewReader(input), &out, &diagnostic); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.root, ".pairroom", "rooms", original.Room, "slots", "slot2")
	c, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	if c.State.Pending == nil || !c.State.Pending.Unknown || c.State.Pending.ExpectedPeerGeneration != 1 || c.State.Pending.Text != text {
		t.Fatalf("lost LAN receipt destroyed its original WAL: %+v", c.State.Pending)
	}
	if err := c.Reconcile(context.Background(), false); !errors.Is(err, relay.ErrPeerChanged) {
		t.Fatalf("original remote receipt did not settle locally: %v", err)
	}
	if c.State.Pending.Unknown || c.State.Pending.Outcome != relay.PeerChangedCode || c.State.Pending.Text != text || c.State.AutomaticPeerGeneration != 1 || c.State.PeerGeneration != 2 {
		t.Fatal("LAN mapping retargeted or discarded the original response")
	}
	if len(f.engine.Snapshot().Messages) != 1 {
		t.Fatal("old guest response reached the replacement host")
	}
	if _, err := f.run(t, "guest-session", true, "send", "--id", "wrong", "--peer-generation", "1", "--text", text); err == nil || !strings.Contains(err.Error(), "nothing was published") {
		t.Fatalf("LAN stale target was not definite peer_changed: %v", err)
	}
	if _, err := f.run(t, "guest-session", true, "send", "--id", "intentional", "--peer-generation", "2", "--text", text); err != nil {
		t.Fatal(err)
	}
	if len(f.engine.Snapshot().Messages) != 2 {
		t.Fatal("explicit guest recovery was not published")
	}
}

func TestLANDynamicDetachRejoinSameSessionCannotResetAutomaticFence(t *testing.T) {
	f := newLANCLIWire(t, true)
	bindDynamicLANOwner(t, f, "old-owner", model.RuntimeClaude, false)
	_, original := f.bind(t, "same-guest-session")
	if _, err := f.run(t, "same-guest-session", true, "unbind", "--local-only"); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	bindDynamicLANOwner(t, f, "new-owner", model.RuntimeGrok, true)
	invite := f.invite(t)
	output, err := f.run(t, "same-guest-session", true, "join", lanshare.EncodeInvite(invite), "--replace")
	if err != nil {
		t.Fatal(err)
	}
	f.approve(t, output)
	if _, err := f.run(t, "same-guest-session", true, "join", lanshare.EncodeInvite(invite), "--replace"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, ".pairroom", "rooms", original.Room, "slots", "slot2", "state.json")
	var state State
	if err := readPrivate(path, &state); err != nil {
		t.Fatal(err)
	}
	if state.BindID == original.BindID || state.Generation != 2 || !state.AutomaticPaused || state.AutomaticPeerGeneration != 1 || state.PeerGeneration != 2 {
		t.Fatalf("same-session LAN rejoin laundered response context: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(f.root, ".pairroom", "retired", original.BindID, "state.json")); err != nil {
		t.Fatal("original private evidence was not archived", err)
	}
}
