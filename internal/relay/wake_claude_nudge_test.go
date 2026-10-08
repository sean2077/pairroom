package relay

import (
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

// TestClaudeNudgePendingAssumption verifies that Claude targets now receive
// nudge_pending protection, preventing redundant wake attempts during active Turns.
// This was enabled after verification confirmed that Claude Code's cross-session
// inbox behaves like codex queue, holding messages until Turn end.
func TestClaudeNudgePendingAssumption(t *testing.T) {
	e, a, _ := testEngine(t)

	// Bind both slots with their respective runtimes
	first, err := e.Send(a[model.ActorSlot2], SendRequest{ID: "claude-1", Text: "first"})
	if err != nil {
		t.Fatal(err)
	}

	// Reserve and submit a wake to Claude (slot1)
	if err := e.ReserveWake(first.ID, model.ActorSlot1); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWakeAttempt(first.ID, "submitted", "", model.ActorSlot1); err != nil {
		t.Fatal(err)
	}

	// Simulate mid-Turn activity (Claude made a relay call but Turn hasn't ended)
	if _, err := e.Inspect(a[model.ActorSlot1]); err != nil {
		t.Fatal(err)
	}

	// Send another message while Claude is "mid-Turn"
	next, err := e.Send(a[model.ActorSlot2], SendRequest{ID: "claude-2", Text: "next"})
	if err != nil {
		t.Fatal(err)
	}

	// CURRENT BEHAVIOR (after verification): Claude IS protected by nudge_pending
	c, ok := e.WakeCandidate(next.ID)
	if !ok {
		t.Fatal("expected valid wake candidate")
	}

	if !c.NudgePending {
		t.Error("Claude candidate should have NudgePending=true after verification")
	}

	// Reservation should fail due to outstanding nudge
	if err := e.ReserveWake(next.ID, model.ActorSlot1); err != ErrWakeIneligible {
		t.Errorf("Claude reservation = %v; want ErrWakeIneligible due to nudge_pending", err)
	}
}

// TestCodexNudgePendingWorksAsExpected verifies that Codex targets DO get
// nudge_pending protection (this is the current working behavior).
func TestCodexNudgePendingWorksAsExpected(t *testing.T) {
	e, a, _ := testEngine(t)

	// Send to Codex (slot2)
	first, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "codex-1", Text: "first"})
	if err != nil {
		t.Fatal(err)
	}

	// Reserve and accept wake
	if err := e.ReserveWake(first.ID, model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWakeAttempt(first.ID, "accepted", "", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}

	// Simulate mid-Turn activity
	if _, err := e.Inspect(a[model.ActorSlot2]); err != nil {
		t.Fatal(err)
	}

	// Send another message
	next, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "codex-2", Text: "next"})
	if err != nil {
		t.Fatal(err)
	}

	// Codex SHOULD have nudge_pending protection
	c, ok := e.WakeCandidate(next.ID)
	if !ok {
		t.Fatal("expected valid wake candidate")
	}

	if !c.NudgePending {
		t.Error("Codex candidate should have NudgePending=true")
	}

	// Reservation should fail due to outstanding nudge
	if err := e.ReserveWake(next.ID, model.ActorSlot2); err != ErrWakeIneligible {
		t.Errorf("Codex reservation = %v; want ErrWakeIneligible due to nudge_pending", err)
	}
}

// TestClaudeAndCodexSymmetry documents the asymmetry we want to investigate.
// This test captures the current state and can guide the verification.
func TestClaudeAndCodexWakeSymmetry(t *testing.T) {
	e, a, _ := testEngine(t)

	type testCase struct {
		name            string
		targetSlot      model.ActorID
		senderSlot      model.ActorID
		expectProtected bool // whether nudge_pending should protect
	}

	cases := []testCase{
		{
			name:            "Codex target gets nudge_pending",
			targetSlot:      model.ActorSlot2, // Codex
			senderSlot:      model.ActorSlot1,
			expectProtected: true, // CURRENT: works
		},
		{
			name:            "Claude target has nudge_pending",
			targetSlot:      model.ActorSlot1, // Claude
			senderSlot:      model.ActorSlot2,
			expectProtected: true, // After verification: Claude inbox behaves like codex queue
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Send first message
			first, err := e.Send(a[tc.senderSlot], SendRequest{ID: tc.name + "-1", Text: "first"})
			if err != nil {
				t.Fatal(err)
			}

			// Wake it
			if err := e.ReserveWake(first.ID, tc.targetSlot); err != nil {
				t.Fatal(err)
			}
			outcome := "accepted"
			if e.cfg.Runtimes[tc.targetSlot] == model.RuntimeClaude {
				outcome = "submitted"
			}
			if err := e.RecordWakeAttempt(first.ID, outcome, "", tc.targetSlot); err != nil {
				t.Fatal(err)
			}

			// Mid-Turn activity
			if _, err := e.Inspect(a[tc.targetSlot]); err != nil {
				t.Fatal(err)
			}

			// Send second message
			second, err := e.Send(a[tc.senderSlot], SendRequest{ID: tc.name + "-2", Text: "second"})
			if err != nil {
				t.Fatal(err)
			}

			c, ok := e.WakeCandidate(second.ID)
			if !ok {
				t.Fatal("no candidate")
			}

			if c.NudgePending != tc.expectProtected {
				t.Errorf("NudgePending = %v; want %v (current design)", c.NudgePending, tc.expectProtected)
			}

			reserveErr := e.ReserveWake(second.ID, tc.targetSlot)
			if tc.expectProtected {
				if reserveErr != ErrWakeIneligible {
					t.Errorf("protected target reservation = %v; want ErrWakeIneligible", reserveErr)
				}
			} else {
				if reserveErr != nil {
					t.Errorf("unprotected target reservation failed: %v", reserveErr)
				}
			}
		})
	}
}
