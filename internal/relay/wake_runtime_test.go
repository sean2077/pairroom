package relay

import (
	"errors"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestWakeAdmissionRequiresKnownRuntime(t *testing.T) {
	for _, tc := range []struct {
		name               string
		configured, bound  model.RuntimeKind
		wantRuntime        model.RuntimeKind
		wantWakeCapability bool
	}{
		{"Claude", model.RuntimeClaude, "", model.RuntimeClaude, true},
		{"Codex", model.RuntimeCodex, "", model.RuntimeCodex, true},
		{"canonical alias", " CLAUDE-CODE ", "", model.RuntimeClaude, true},
		{"binding fallback", "", model.RuntimeClaude, model.RuntimeClaude, true},
		{"Room identity wins", model.RuntimeCodex, model.RuntimeClaude, model.RuntimeCodex, true},
		{"unknown Room identity cannot fall back", "future-runtime", model.RuntimeClaude, "future-runtime", false},
		{"empty identity", "", "", "", false},
		{"unknown binding", "", "future-runtime", "future-runtime", false},
		{"Grok", model.RuntimeGrok, "", model.RuntimeGrok, false},
		{"Gemini", model.RuntimeGemini, "", model.RuntimeGemini, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, auth, _ := testEngine(t)
			target := model.ActorSlot2
			e.cfg.Runtimes[target] = tc.configured
			b := e.bindings[target]
			b.Runtime = tc.bound
			if err := e.commitBinding(b); err != nil {
				t.Fatal(err)
			}
			m, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "wake", Text: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if c, ok := e.WakeCandidate(m.ID); !ok || c.Runtime != tc.wantRuntime {
				t.Fatalf("candidate = %#v, %v; want runtime %q", c, ok, tc.wantRuntime)
			}
			_, bindings, _ := e.InspectTransport()
			if got := bindings[target].Runtime; got != tc.wantRuntime {
				t.Fatalf("inspection runtime = %q; want %q", got, tc.wantRuntime)
			}
			events, err := e.cfg.Store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if got, err := HasWakeWork("room", events, e.cfg.Runtimes); err != nil || got != tc.wantWakeCapability {
				t.Fatalf("startup wake probe = %v, %v; want %v", got, err, tc.wantWakeCapability)
			}
			before := e.Sequence()
			err = e.ReserveWake(m.ID, target)
			if tc.wantWakeCapability {
				if err != nil || len(e.WakeReservations()) != 1 {
					t.Fatalf("supported wake reservation = %v", err)
				}
			} else if !errors.Is(err, ErrWakeIneligible) || len(e.WakeReservations()) != 0 || e.Sequence() != before {
				t.Fatalf("unsupported wake reservation = %v; reservations=%#v sequence=%d->%d", err, e.WakeReservations(), before, e.Sequence())
			}
		})
	}
}
