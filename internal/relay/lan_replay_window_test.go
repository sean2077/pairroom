package relay

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func TestLANReplayAfterInvitationWindowPreservesMembershipHistory(t *testing.T) {
	for _, phase := range []string{"pending", "accepted", "revoked", "successor"} {
		t.Run(phase, func(t *testing.T) {
			now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			e, _, dir := lanEngine(t, func() time.Time { return now })
			invite, err := e.CreateLANInvite()
			if err != nil {
				t.Fatal(err)
			}
			key := Digest("original-peer")
			if _, _, err := e.RequestLANJoin(LANJoinRequest{InviteID: invite.ID, RequestID: "original-request", Key: key, Runtime: model.RuntimeCodex}); err != nil {
				t.Fatal(err)
			}
			if phase != "pending" {
				if _, err := e.AcceptLANJoin("original-request", key); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "revoked" || phase == "successor" {
				if err := e.RevokeLANMember(); err != nil {
					t.Fatal(err)
				}
			}
			nextKey := Digest("successor-peer")
			if phase == "successor" {
				next, err := e.CreateLANInvite()
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err := e.RequestLANJoin(LANJoinRequest{InviteID: next.ID, RequestID: "successor-request", Key: nextKey, Runtime: model.RuntimeGemini}); err != nil {
					t.Fatal(err)
				}
				if _, err := e.AcceptLANJoin("successor-request", nextKey); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "events.jsonl")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// The public invitation lasts ten minutes and retains an expired
			// pending answer for one more window. Replay runs well beyond both.
			now = now.Add(24 * time.Hour)
			log, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := Open(Config{RoomID: "room", Store: log, SharedSlot: model.ActorSlot2, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeAwaitingPeer}, Now: func() time.Time { return now }})
			if err != nil {
				_ = log.Close()
				t.Fatalf("replay %s after invitation expiry: %v", phase, err)
			}
			defer restarted.Close()
			_, status, err := restarted.LANJoinStatus("original-request", key)
			wantRuntime := model.RuntimeAwaitingPeer
			switch phase {
			case "pending":
				if !errors.Is(err, ErrAuth) {
					t.Fatalf("expired unadmitted request survived: %q, %v", status, err)
				}
			case "accepted":
				if err != nil || status != "accepted" {
					t.Fatalf("admission was lost: %q, %v", status, err)
				}
				wantRuntime = model.RuntimeCodex
			default:
				if err != nil || status != "revoked" {
					t.Fatalf("retired admission was lost: %q, %v", status, err)
				}
			}
			if phase == "successor" {
				if _, status, err := restarted.LANJoinStatus("successor-request", nextKey); err != nil || status != "accepted" {
					t.Fatalf("successor was lost: %q, %v", status, err)
				}
				wantRuntime = model.RuntimeGemini
			}
			if got := restarted.Runtimes()[model.ActorSlot2]; got != wantRuntime {
				t.Fatalf("replayed peer runtime = %s, want %s", got, wantRuntime)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("projection pruning rewrote the Event Log: %v", err)
			}
			// The same reopened Engine is also used by offline archive.
			if err := restarted.RevokeLANMember(); err != nil {
				t.Fatalf("reopened Room cannot retire its member: %v", err)
			}
		})
	}
}

func TestLANReplayPreservesReusedUnadmittedRequestID(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	e, _, dir := lanEngine(t, func() time.Time { return now })
	invite, err := e.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	key := Digest("retrying-peer")
	for _, id := range []string{"attempt-a", "attempt-b", "attempt-a"} {
		now = now.Add(time.Second)
		if _, status, err := e.RequestLANJoin(LANJoinRequest{InviteID: invite.ID, RequestID: id, Key: key, Runtime: model.RuntimeCodex}); err != nil || status != "pending" {
			t.Fatalf("retry %s: %q, %v", id, status, err)
		}
	}
	if _, err := e.AcceptLANJoin("attempt-a", key); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(Config{RoomID: "room", Store: log, SharedSlot: model.ActorSlot2, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeAwaitingPeer}, Now: func() time.Time { return now }})
	if err != nil {
		_ = log.Close()
		t.Fatalf("valid request ID reuse could not replay: %v", err)
	}
	defer restarted.Close()
	if _, status, err := restarted.LANJoinStatus("attempt-a", key); err != nil || status != "accepted" {
		t.Fatalf("reused request lost its admission: %q, %v", status, err)
	}
	if _, _, err := restarted.LANJoinStatus("attempt-b", key); !errors.Is(err, ErrAuth) {
		t.Fatalf("superseded unadmitted attempt survived replay: %v", err)
	}
}

func TestLANReplayRejectsReuseOfAnAdmittedRequestID(t *testing.T) {
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	e, _, dir := lanEngine(t, func() time.Time { return now })
	key := Digest("admitted-peer")
	admitLAN(t, e, key)
	requestID := "request-" + key[:8]
	if err := e.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	invite, err := e.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	event, err := model.NewEvent("room", EventLANJoin, model.ActorSystem, LANJoinRequest{InviteID: invite.ID, RequestID: requestID, Key: Digest("other-peer"), Runtime: model.RuntimeGemini, CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	event.CreatedAt = now
	if err := e.cfg.Store.Append(&event); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	restarted, err := Open(Config{RoomID: "room", Store: log, SharedSlot: model.ActorSlot2, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeAwaitingPeer}, Now: func() time.Time { return now }})
	if err == nil {
		_ = restarted.Close()
		t.Fatal("an admitted request ID was reassigned during replay")
	}
}
