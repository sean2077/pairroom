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
				// that observation must not reserve or strand the new queue head.
				if err := e.RecordWake("suppressed", "nudge_pending", target.Slot); err != nil {
					t.Fatal(err)
				}
				if err := e.Close(); err != nil {
					t.Fatal(err)
				}
				e = reopenEngine(t, dir)
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
