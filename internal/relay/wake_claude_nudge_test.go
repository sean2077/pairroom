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
			// Claude may read its nudge between tool calls and collect this burst
			// before the Turn ends. Model acceptance remains outside this test.
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
				// that observation must not reserve the new queue head. The earlier
				// Claim/Ack activity was process-local, so replay cannot assume the
				// nudge was consumed until another authenticated relay call.
				if err := e.RecordWake("suppressed", "nudge_pending", target.Slot); err != nil {
					t.Fatal(err)
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e = reopenEngine(t, dir)
				if c, ok := e.WakeCandidate(next.ID); !ok || !c.NudgePending || c.Reserved {
					t.Fatalf("replayed Claude candidate = %#v, %v; want conservative pending", c, ok)
				}
				if _, err := e.Inspect(target); err != nil {
					t.Fatal(err)
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

func TestClaudeWakePendingRequiresLaterAuthenticatedActivity(t *testing.T) {
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
			// Equal timestamps cannot establish that this call followed the nudge.
			if _, err := e.Inspect(target); err != nil {
				t.Fatal(err)
			}
			if err := e.Cancel(first.ID); err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Minute)
			next, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "next", Text: "next"})
			if err != nil {
				t.Fatal(err)
			}
			// Read-only Management inspection and invalid relay credentials do not
			// supply an activity observation for the current target binding.
			e.InspectTransport()
			bad := target
			bad.Secret = "wrong"
			if _, err := e.Inspect(bad); !errors.Is(err, ErrAuth) {
				t.Fatalf("invalid authentication = %v", err)
			}
			if err := e.RecordWake("suppressed", "nudge_pending", target.Slot); err != nil {
				t.Fatal(err)
			}
			if tc.replay {
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e = reopenEngine(t, dir)
				e.cfg.Now = func() time.Time { return now }
			}
			if c, ok := e.WakeCandidate(next.ID); !ok || !c.QueueStart || !c.NudgePending || c.Reserved {
				t.Fatalf("outstanding Claude nudge = %#v, %v", c, ok)
			}
			if err := e.ReserveWake(next.ID, target.Slot); !errors.Is(err, ErrWakeIneligible) {
				t.Fatalf("pending admission = %v; want ErrWakeIneligible", err)
			}
			if _, err := e.Inspect(target); err != nil {
				t.Fatal(err)
			}
			if c, ok := e.WakeCandidate(next.ID); !ok || c.NudgePending || c.Reserved {
				t.Fatalf("Claude activity did not release next burst = %#v, %v", c, ok)
			}
			if err := e.ReserveWake(next.ID, target.Slot); err != nil {
				t.Fatalf("new burst admission = %v", err)
			}
			if err := e.ReserveWake(first.ID, target.Slot); !errors.Is(err, ErrWakeReserved) {
				t.Fatalf("attempted message admission = %v; want ErrWakeReserved", err)
			}
		})
	}
}
