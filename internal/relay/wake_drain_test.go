package relay

import (
	"context"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestReservedWakeCompletionDuringDrain(t *testing.T) {
	for _, tc := range []struct{ outcome, reason string }{{"accepted", ""}, {"submitted", ""}, {"failed", "command_cancelled"}, {"failed", "socket_cancelled"}} {
		t.Run(tc.outcome+tc.reason, func(t *testing.T) {
			e, a, dir := testEngine(t)
			m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "reserved", Text: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			unreserved, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "unreserved", Text: "fixture"})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.ReserveWake(m.ID, model.ActorSlot2); err != nil {
				t.Fatal(err)
			}
			e.SetDraining(true)
			before := e.Sequence()
			for _, attempt := range []struct {
				id, outcome, reason string
				target              model.ActorID
			}{
				{unreserved.ID, tc.outcome, tc.reason, model.ActorSlot2},
				{m.ID, tc.outcome, tc.reason, model.ActorSlot1},
				{m.ID, "suppressed", "minimum_interval", model.ActorSlot2},
			} {
				if err := e.RecordWakeAttempt(attempt.id, attempt.outcome, attempt.reason, attempt.target); err == nil {
					t.Fatal("invalid completion accepted")
				}
			}
			if err := e.RecordWake("suppressed", "minimum_interval", model.ActorSlot2); err == nil {
				t.Fatal("new suppression during drain")
			}
			if err := e.ReserveWake(unreserved.ID, model.ActorSlot2); err == nil {
				t.Fatal("new reservation during drain")
			}
			if e.Sequence() != before {
				t.Fatal("rejected operations appended facts")
			}
			if err := e.RecordWakeAttempt(m.ID, tc.outcome, tc.reason, model.ActorSlot2); err != nil {
				t.Fatal(err)
			}
			if err := e.Close(); err != nil {
				t.Fatal(err)
			}
			if err := e.RecordWakeAttempt(m.ID, tc.outcome, tc.reason, model.ActorSlot2); err == nil {
				t.Fatal("completion after closure")
			}
			restored := reopenEngine(t, dir)
			got := restored.Summary().LastWake[model.ActorSlot2]
			if got.Outcome != tc.outcome || got.Reason != tc.reason {
				t.Fatalf("lost completed audit on replay: %+v", got)
			}
		})
	}
}

func TestIdleDrainClosesClaimAndReservationAdmission(t *testing.T) {
	e, a, _ := testEngine(t)
	m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "idle", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if !e.TryBeginIdleClose() {
		t.Fatal("idle close refused")
	}
	if _, err := e.Claim(context.Background(), a[model.ActorSlot2], false); err == nil {
		t.Fatal("claim crossed drain")
	}
	if err := e.ReserveWake(m.ID, model.ActorSlot2); err == nil {
		t.Fatal("reservation crossed drain")
	}
	if len(e.WakeReservations()) != 0 {
		t.Fatal("rejected reservation consumed its ID")
	}
}
