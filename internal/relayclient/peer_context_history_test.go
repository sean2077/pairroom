package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestDynamicBindCorruptResponseHistoryWithholdsBootstrapAndKeepsAttempt(t *testing.T) {
	f := newDynamicCLIWire(t)
	if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
		t.Fatal(err)
	}
	s := f.state(t, model.ActorSlot1).State
	if _, err := f.run(t, model.RuntimeClaude, "source", "unbind"); err != nil {
		t.Fatal(err)
	}
	dir, err := responseHistoryDirectory(s)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "context.json"), []byte(`{"schema":1`), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, model.RuntimeClaude, "source", "bind")
	if err == nil || len(out) != 0 {
		t.Fatalf("corrupt history granted automatic bootstrap: %s %v", out, err)
	}
	statePath := filepath.Join(f.root, ".pairroom", "rooms", f.room, "slots", "slot1", "state.json")
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprovable history became discoverable: %v", err)
	}
	attemptPath := filepath.Join(f.root, ".pairroom", "rooms", f.room, "bind-attempts", autoBindKey(model.RuntimeClaude, "source"), bindAttemptFile)
	if _, err := os.Stat(attemptPath); err != nil {
		t.Fatalf("accepted bind lost its crash recovery identity: %v", err)
	}
}

func TestDynamicBindRejectsRetiredJSONWithIncompleteIdentity(t *testing.T) {
	for _, field := range []string{"runtime", "session_id", "room", "slot", "generation", "runtime_alias", "session_whitespace"} {
		t.Run(field, func(t *testing.T) {
			f := newDynamicCLIWire(t)
			if _, err := f.run(t, model.RuntimeClaude, "source", "bind"); err != nil {
				t.Fatal(err)
			}
			s := f.state(t, model.ActorSlot1).State
			if _, err := f.run(t, model.RuntimeClaude, "source", "unbind"); err != nil {
				t.Fatal(err)
			}
			historyDir, err := responseHistoryDirectory(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(historyDir, "context.json")); err != nil {
				t.Fatal(err)
			}
			archivePath := filepath.Join(f.root, ".pairroom", "retired", s.BindID, "state.json")
			var damaged map[string]any
			if err := readPrivate(archivePath, &damaged); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "runtime_alias":
				damaged["runtime"] = "claude-code"
			case "session_whitespace":
				damaged["session_id"] = " source "
			default:
				delete(damaged, field)
			}
			data, err := json.Marshal(damaged)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(archivePath, data, 0600); err != nil {
				t.Fatal(err)
			}
			next := newNativeCLIWireAt(t, f.root, "another-room", true)
			out, err := next.run(t, model.RuntimeClaude, "source", "bind")
			if err == nil || len(out) != 0 {
				t.Fatalf("incomplete identity granted a new bootstrap: %s %v", out, err)
			}
			for _, path := range []string{filepath.Join(historyDir, "context.json"), filepath.Join(f.root, ".pairroom", "rooms", next.room, "slots", "slot1", "state.json")} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("damaged archive generated new automatic context: %s %v", path, err)
				}
			}
			attemptPath := filepath.Join(f.root, ".pairroom", "rooms", next.room, "bind-attempts", autoBindKey(model.RuntimeClaude, "source"), bindAttemptFile)
			if _, err := os.Stat(attemptPath); err != nil {
				t.Fatalf("accepted bind lost its original recovery key: %v", err)
			}
		})
	}
}

func TestRetiredLANAttemptAloneCannotProveUnconfirmedResponse(t *testing.T) {
	f := newLANCLIWire(t, true)
	bindDynamicLANOwner(t, f, "owner", model.RuntimeClaude, false)
	_, original := f.bind(t, "same-guest")
	var attempt lanJoinAttempt
	if err := readPrivate(filepath.Join(f.root, ".pairroom", "lan-joins", original.Room, "join-attempt.json"), &attempt); err != nil {
		t.Fatal(err)
	}
	if err := archiveLANAttempt(f.root, attempt); err != nil {
		t.Fatal(err)
	}
	// This admitted binding has already exposed a bootstrap. A partial backup
	// containing only its valid attempt cannot prove that no old response exists.
	dir, err := responseHistoryDirectory(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "context.json")); err != nil {
		t.Fatal(err)
	}
	next := State{Schema: 2, Runtime: original.Runtime, SessionID: original.SessionID, Workspace: original.Workspace, Room: "another-room", Slot: model.ActorSlot1, DynamicBindings: true}
	if err := retainResponseContext(context.Background(), &next, 1); err != nil {
		t.Fatal(err)
	}
	if !next.AutomaticScopeChanged || !next.AutomaticPaused {
		t.Fatal("admitted LAN attempt was mistaken for proof of no original response")
	}
	c := &Client{State: next}
	c.Save = func(s State) error { return nil }
	const text = "late accepted LAN session output @agent2"
	if err := c.ReservePublication(text); err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background(), false); !errors.Is(err, errResponseScopeChanged) || c.State.Pending == nil || c.State.Pending.Text != text || c.State.Pending.Outcome != "context_changed" {
		t.Fatalf("partial LAN archive allowed response publication: %+v %v", c.State.Pending, err)
	}
	if len(f.engine.Snapshot().Messages) != 0 {
		t.Fatal("local recovery ambiguity became a remote publication")
	}
}

func TestResponseHistoryRestorationRejectsIncompleteOrTruncatedArchives(t *testing.T) {
	for _, count := range []int{1, 1025} {
		t.Run(fmt.Sprintf("archive-count-%d", count), func(t *testing.T) {
			IsolateNativeCaller(t)
			s := State{Schema: 2, Runtime: model.RuntimeClaude, SessionID: "source", Workspace: t.TempDir(), Room: "room", Slot: model.ActorSlot1, DynamicBindings: true}
			retired, err := secureDir(s.Workspace, ".pairroom", "retired")
			if err != nil {
				t.Fatal(err)
			}
			for i := range count {
				if err := os.Mkdir(filepath.Join(retired, fmt.Sprintf("binding-%d", i)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := retainResponseContext(context.Background(), &s, 2); err == nil || s.AutomaticPeerGeneration != 0 {
				t.Fatalf("incomplete/truncated history authorized a new generation: %+v %v", s, err)
			}
			dir, err := responseHistoryDirectory(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "context.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed scan replaced original history: %v", err)
			}
		})
	}
}
