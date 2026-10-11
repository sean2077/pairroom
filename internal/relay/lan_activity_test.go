package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func admitLANRuntime(t *testing.T, e *Engine, kind model.RuntimeKind) Auth {
	t.Helper()
	invite, err := e.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	key := Digest("native-peer")
	if _, _, err := e.RequestLANJoin(LANJoinRequest{InviteID: invite.ID, RequestID: "native-request", Key: key, Runtime: kind}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AcceptLANJoin("native-request", key); err != nil {
		t.Fatal(err)
	}
	auth, err := e.LANAuth(key)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

func TestLANNativeActivityDoesNotComeFromOwnerOrObserverReads(t *testing.T) {
	now := time.Now().UTC()
	e, owner, _ := lanEngine(t, func() time.Time { return now })
	auth := admitLANRuntime(t, e, model.RuntimeCodex)
	activity := func() time.Time { return e.Snapshot().Bindings[auth.Slot].LastActivity }
	initial := activity()
	now = now.Add(time.Second)
	message, err := e.Send(owner, SendRequest{ID: "incoming", Text: "native work"})
	if err != nil {
		t.Fatal(err)
	}
	before := e.Sequence()
	if _, err := e.LANAuth(auth.MemberKey); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Inspect(auth); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AuthSummary(auth); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AuthSnapshot(auth); err != nil {
		t.Fatal(err)
	}
	if _, err := e.AuthHistory(auth, HistoryQuery{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Peer(auth); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.Publication(auth, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.LANJoinStatus("native-request", auth.MemberKey); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LANWakeCandidate(auth, message.ID); err != nil {
		t.Fatal(err)
	}
	if !activity().Equal(initial) || e.Sequence() != before {
		t.Fatal("read-only observer manufactured native activity or a durable heartbeat")
	}
	if _, err := e.SendLANUser(auth, SendRequest{ID: "human-input", Text: "human comment", To: owner.Slot}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LANUserReceipt(auth, "human-input"); err != nil {
		t.Fatal(err)
	}
	if !activity().Equal(initial) {
		t.Fatal("remote human input was treated as native agent activity")
	}
	before = e.Sequence()
	confirmed, err := e.ConfirmLAN(auth)
	if err != nil || !confirmed.LastActivity.Equal(now) || confirmed.SessionID != "" || confirmed.TranscriptPath != "" {
		t.Fatalf("native confirmation did not advance only public activity: %+v, %v", confirmed, err)
	}
	if e.Sequence() != before {
		t.Fatal("native heartbeat appended an unnecessary durable binding event")
	}
	for _, action := range []struct {
		name string
		call func() error
	}{
		{"send", func() error {
			_, err := e.Send(auth, SendRequest{ID: "native-update", Text: "working", To: model.ActorUser})
			return err
		}},
		{"report", func() error { _, err := e.Report(auth, 1, "private unaddressed reply"); return err }},
		{"failure", func() error { return e.Failure(auth, "rate_limit") }},
	} {
		now = now.Add(time.Second)
		if err := action.call(); err != nil || !activity().Equal(now) {
			t.Fatalf("%s did not advance authenticated native activity: %v", action.name, err)
		}
	}
	now = now.Add(time.Second)
	head, err := e.PrepareHead(context.Background(), auth, false)
	if err != nil || head == nil || !activity().Equal(now) || e.Snapshot().Messages[0].State != "queued" {
		t.Fatalf("foreground preparation did not record activity without consuming: %+v, %v", head, err)
	}
	now = now.Add(time.Second)
	claim, err := e.ClaimPrepared(context.Background(), auth, head.Message.ID, head.Digest, false)
	if err != nil || claim == nil || !activity().Equal(now) {
		t.Fatalf("prepared claim did not advance native activity: %+v, %v", claim, err)
	}
	claimedAt := activity()
	now = now.Add(time.Second)
	if err := e.Ack(auth, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if err := e.Ack(auth, claim.ID, claim.Receipt); err != nil || !activity().Equal(claimedAt) {
		t.Fatal("background original-ACK recovery manufactured fresh native progress")
	}
	bad := auth
	bad.MemberKey = Digest("another-certificate")
	before = e.Sequence()
	if _, err := e.ConfirmLAN(bad); !errors.Is(err, ErrAuth) || !activity().Equal(claimedAt) || e.Sequence() != before {
		t.Fatal("invalid certificate advanced native activity")
	}
}

func TestLANCodexWakeUsesNativeProgressAndRemainsConservativeAfterRestart(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "busy native turn", true: "observed idle native turn"}[idle], func(t *testing.T) {
			now := time.Now().UTC()
			e, owner, dir := lanEngine(t, func() time.Time { return now })
			auth := admitLANRuntime(t, e, model.RuntimeCodex)
			if idle {
				now = now.Add(time.Second)
				if head, err := e.PrepareHead(context.Background(), auth, true); err != nil || head != nil {
					t.Fatalf("empty Stop park did not end the native turn: %+v, %v", head, err)
				}
			}
			now = now.Add(time.Second)
			first, err := e.Send(owner, SendRequest{ID: "first-nudge", Text: "first"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ReserveLANWake(auth, first.ID); err != nil {
				t.Fatal(err)
			}
			if err := e.RecordLANWake(auth, first.ID, "accepted", ""); err != nil {
				t.Fatal(err)
			}
			if err := e.Cancel(first.ID); err != nil {
				t.Fatal(err)
			}
			next, err := e.Send(owner, SendRequest{ID: "next-nudge", Text: "next"})
			if err != nil {
				t.Fatal(err)
			}
			assertPending := func(want bool) {
				t.Helper()
				candidate, err := e.LANWakeCandidate(auth, next.ID)
				if err != nil || candidate == nil || candidate.NudgePending != want || candidate.Reserved || !candidate.QueueStart {
					t.Fatalf("wake pending=%v: %+v, %v", want, candidate, err)
				}
			}
			now = now.Add(time.Second)
			if _, err := e.AuthSummary(auth); err != nil {
				t.Fatal(err)
			}
			assertPending(true)
			if _, err := e.ConfirmLAN(auth); err != nil {
				t.Fatal(err)
			}
			assertPending(!idle)
			if !idle {
				now = now.Add(time.Second)
				if err := e.Failure(auth, "rate_limit"); err != nil {
					t.Fatal(err)
				}
				assertPending(true) // Turn end alone is not later native progress.
				now = now.Add(time.Second)
				if _, err := e.ConfirmLAN(auth); err != nil {
					t.Fatal(err)
				}
				assertPending(false)
			}
			if err := e.ReserveLANWake(auth, first.ID); !errors.Is(err, ErrWakeReserved) {
				t.Fatalf("original spent reservation became eligible again: %v", err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			log, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			e, err = Open(Config{RoomID: "room", Store: log, SharedSlot: auth.Slot, Runtimes: map[model.ActorID]model.RuntimeKind{owner.Slot: model.RuntimeClaude, auth.Slot: model.RuntimeAwaitingPeer}, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.Close() })
			assertPending(true) // Restart cannot reconstruct the native turn boundary.
			if reservations := e.WakeReservations(); len(reservations) != 1 || reservations[0].MessageID != first.ID {
				t.Fatal("restart lost the original spent reservation")
			}
			now = now.Add(time.Second)
			if err := e.Failure(auth, "rate_limit"); err != nil {
				t.Fatal(err)
			}
			assertPending(true)
			now = now.Add(time.Second)
			if _, err := e.ConfirmLAN(auth); err != nil {
				t.Fatal(err)
			}
			assertPending(false)
			if err := e.ReserveLANWake(auth, next.ID); err != nil {
				t.Fatalf("new burst stayed blocked after real native progress: %v", err)
			}
			if err := e.ReserveLANWake(auth, first.ID); !errors.Is(err, ErrWakeReserved) {
				t.Fatal("restart or fresh progress replayed the old reservation")
			}
		})
	}
}
