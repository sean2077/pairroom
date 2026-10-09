package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestClaudeWakeNewBurstDoesNotRequireAnotherTurn(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		replay        bool
	}{
		{name: "submitted", outcome: "submitted"},
		{name: "reserved without outcome"},
		{name: "replayed Claude suppression", outcome: "submitted", replay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, auth, dir := testEngine(t)
			target := auth[model.ActorSlot1]
			now := e.bindings[target.Slot].LastActivity.Add(time.Second)
			e.cfg.Now = func() time.Time { return now }
			first, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "claude-1", Text: "first"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ReserveWake(first.ID, target.Slot); err != nil {
				t.Fatal(err)
			}
			if tc.outcome != "" {
				if err := e.RecordWakeAttempt(first.ID, tc.outcome, "", target.Slot); err != nil {
					t.Fatal(err)
				}
			}
			// A receipt-matched handoff supersedes the wake need without waiting
			// for Turn end. It does not prove the native nudge itself arrived.
			now = now.Add(time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			claim, err := e.Claim(ctx, target, false)
			if err != nil || claim == nil || claim.ID != first.ID {
				t.Fatalf("first claim = %#v, %v", claim, err)
			}
			if err := e.Ack(target, claim.ID, claim.Receipt); err != nil {
				t.Fatal(err)
			}
			next, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "claude-2", Text: "next"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.replay {
				// Keep reading the audit vocabulary emitted by the prior binary;
				// that observation must not reserve the new queue head. The actual
				// handoff is durable and must supersede the wake after replay too.
				if err := e.RecordWake("suppressed", "nudge_pending", target.Slot); err != nil {
					t.Fatal(err)
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e = reopenEngine(t, dir)
				e.cfg.Now = func() time.Time { return now }
				if c, ok := e.WakeCandidate(next.ID); !ok || c.NudgePending || c.Reserved {
					t.Fatalf("replayed Claude candidate = %#v, %v; want acknowledged progress", c, ok)
				}
			}
			if c, ok := e.WakeCandidate(next.ID); !ok || !c.QueueStart || c.Reserved || c.NudgePending {
				t.Fatalf("Claude candidate = %#v, %v; want an unattempted new burst", c, ok)
			}
			if err := e.ReserveWake(first.ID, target.Slot); !errors.Is(err, ErrWakeReserved) {
				t.Fatalf("original wake reservation = %v; want ErrWakeReserved", err)
			}
			if err := e.ReserveWake(next.ID, target.Slot); err != nil {
				t.Fatalf("new burst reservation = %v", err)
			}
		})
	}
}

func TestClaudeWakePendingRequiresAcknowledgedCollection(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		replay        bool
	}{
		{name: "submitted", outcome: "submitted"},
		{name: "unknown outcome"},
		{name: "replayed submitted", outcome: "submitted", replay: true},
		{name: "replayed unknown outcome", replay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, auth, dir := testEngine(t)
			target := auth[model.ActorSlot1]
			now := e.bindings[target.Slot].LastActivity.Add(time.Second)
			e.cfg.Now = func() time.Time { return now }
			first, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "first", Text: "first"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ReserveWake(first.ID, target.Slot); err != nil {
				t.Fatal(err)
			}
			if tc.outcome != "" {
				if err := e.RecordWakeAttempt(first.ID, tc.outcome, "", target.Slot); err != nil {
					t.Fatal(err)
				}
			}
			if err := e.Cancel(first.ID); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			next, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "next", Text: "next"})
			if err != nil {
				t.Fatal(err)
			}
			// All these calls authenticate, but none retrieves an envelope. A
			// held native nudge can remain outstanding throughout them. Include
			// a real binding append so replay also sees a later LastActivity.
			if _, err := e.AuthSummary(target); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Peer(target); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Send(target, SendRequest{ID: "own-send", Text: "update", To: model.ActorUser}); err != nil {
				t.Fatal(err)
			}
			if _, err := e.ConfirmSession(target, target.SessionID, "/updated/transcript"); err != nil {
				t.Fatal(err)
			}
			e.InspectTransport()
			bad := target
			bad.Secret = "wrong"
			if _, err := e.Inspect(bad); !errors.Is(err, ErrAuth) {
				t.Fatalf("invalid authentication = %v", err)
			}
			if tc.replay {
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e = reopenEngine(t, dir)
				e.cfg.Now = func() time.Time { return now }
			}
			if c, ok := e.WakeCandidate(next.ID); !ok || !c.QueueStart || !c.NudgePending || c.Reserved {
				t.Fatalf("non-collection activity released Claude nudge = %#v, %v", c, ok)
			}
			if err := e.ReserveWake(next.ID, target.Slot); !errors.Is(err, ErrWakeIneligible) {
				t.Fatalf("pending admission = %v; want ErrWakeIneligible", err)
			}
			now = now.Add(time.Second)
			claim, err := e.Claim(context.Background(), target, false)
			if err != nil || claim == nil || claim.ID != next.ID {
				t.Fatalf("claim = %#v, %v", claim, err)
			}
			last, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "last", Text: "last"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Ack(target, claim.ID, "wrong-receipt"); !errors.Is(err, ErrAuth) {
				t.Fatalf("wrong acknowledgement = %v", err)
			}
			if c, ok := e.WakeCandidate(last.ID); !ok || !c.NudgePending || !c.Delivering {
				t.Fatalf("claim or invalid Ack consumed nudge = %#v, %v", c, ok)
			}
			if err := e.Ack(target, claim.ID, claim.Receipt); err != nil {
				t.Fatal(err)
			}
			if c, ok := e.WakeCandidate(last.ID); !ok || c.NudgePending || c.Delivering {
				t.Fatalf("acknowledged collection did not supersede wake = %#v, %v", c, ok)
			}
			if err := e.ReserveWake(last.ID, target.Slot); err != nil {
				t.Fatalf("new burst admission = %v", err)
			}
			now = now.Add(time.Second)
			if err := e.Ack(target, claim.ID, claim.Receipt); err != nil {
				t.Fatal(err)
			}
			if c, ok := e.WakeCandidate(last.ID); !ok || !c.NudgePending {
				t.Fatalf("duplicate old Ack released a newer nudge = %#v, %v", c, ok)
			}
			if err := e.ReserveWake(first.ID, target.Slot); !errors.Is(err, ErrWakeReserved) {
				t.Fatalf("attempted message admission = %v; want ErrWakeReserved", err)
			}
		})
	}
}

func TestClaudeWakeOldOrSimultaneousClaimCannotSupersedeNudge(t *testing.T) {
	for _, equal := range []bool{false, true} {
		t.Run(map[bool]string{false: "late Ack of earlier claim", true: "claim at reservation time"}[equal], func(t *testing.T) {
			e, auth, _ := testEngine(t)
			target := auth[model.ActorSlot1]
			now := e.bindings[target.Slot].LastActivity.Add(time.Second)
			e.cfg.Now = func() time.Time { return now }
			first, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "first", Text: "first"})
			if err != nil {
				t.Fatal(err)
			}
			if equal {
				if err := e.ReserveWake(first.ID, target.Slot); err != nil {
					t.Fatal(err)
				}
			}
			claim, err := e.Claim(context.Background(), target, false)
			if err != nil || claim == nil {
				t.Fatalf("claim = %#v, %v", claim, err)
			}
			now = now.Add(DeliveryLease + time.Second)
			if err := e.Reap(); err != nil {
				t.Fatal(err)
			}
			next, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "next", Text: "next"})
			if err != nil {
				t.Fatal(err)
			}
			if !equal {
				if err := e.ReserveWake(next.ID, target.Slot); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(time.Second)
			if err := e.Ack(target, claim.ID, claim.Receipt); err != nil {
				t.Fatal(err)
			}
			if c, ok := e.WakeCandidate(next.ID); !ok || !c.NudgePending {
				t.Fatalf("old claim released newer nudge = %#v, %v", c, ok)
			}
		})
	}
}
