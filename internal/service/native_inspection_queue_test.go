package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestNativeDiagnosticsReportsQueueDeferralAfterNudgeExpiry(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "unavailable"}[unavailable], func(t *testing.T) {
			f := newNudgeFixture(t)
			f.startBusyTurn()
			f.send("first")
			f.collect()
			f.advance(10*time.Minute + time.Second)
			probes := 0
			f.waker.queuePending = func(context.Context, string, string, string) (bool, error) {
				probes++
				if unavailable {
					return false, errors.New("private vendor queue failure")
				}
				return true, nil
			}
			// An executable exists, but that does not establish queue capability.
			f.waker.codexCommand, _ = os.Executable()
			head := f.send("next")
			if c, ok := f.engine.WakeCandidate(head); !ok || c.NudgePending {
				t.Fatal("fixture did not expire the Engine's outstanding nudge")
			}
			n := &nativeHostRuntime{engine: f.engine, waker: f.waker}
			before, due := f.engine.Sequence(), f.now.Add(30*time.Second)
			wantReason := "nudge_pending"
			if unavailable {
				wantReason = "capability_unavailable"
			}
			for _, advance := range []time.Duration{0, 31 * time.Second} {
				f.advance(advance)
				report := n.nativeDiagnostics()
				d := report["participants"].(map[model.ActorID]nativeReachability)[model.ActorSlot2]
				if d.Reason != wantReason || d.NextAction != "collect_in_native_session" || d.HeadID != head || d.WakeReserved || d.NextEligibleAt == nil || !d.NextEligibleAt.Equal(due) {
					t.Fatalf("queue deferral diagnostic = %#v", d)
				}
				body, err := json.Marshal(report)
				if err != nil {
					t.Fatal(err)
				}
				for _, private := range []string{f.auth[model.ActorSlot2].SessionID, "private vendor queue failure", "task next"} {
					if strings.Contains(string(body), private) {
						t.Fatalf("queue diagnostic exposed %q", private)
					}
				}
			}
			if probes != 1 || f.engine.Sequence() != before {
				t.Fatal("diagnostics invoked a probe or wrote Room facts")
			}
			f.expect(1, 1)
		})
	}
}

func TestNativeDiagnosticsQueueDeferralDoesNotFollowRetiredHead(t *testing.T) {
	for _, transition := range []string{"cancel", "collect", "replace"} {
		t.Run(transition, func(t *testing.T) {
			f := newNudgeFixture(t)
			f.waker.codexCommand, _ = os.Executable()
			f.waker.queuePending = func(context.Context, string, string, string) (bool, error) { return true, nil }
			old := f.send("old")
			switch transition {
			case "cancel":
				if err := f.engine.Cancel(old); err != nil {
					t.Fatal(err)
				}
			case "collect":
				f.collect()
			case "replace":
				if _, err := f.engine.Bind(model.ActorSlot2, relay.BindRequest{BindID: "replacement", CredentialHash: relay.Digest("secret-new"), SessionID: "session-new", Replace: true}); err != nil {
					t.Fatal(err)
				}
			}
			n := &nativeHostRuntime{engine: f.engine, waker: f.waker}
			d := n.nativeDiagnostics()["participants"].(map[model.ActorID]nativeReachability)[model.ActorSlot2]
			if d.Reason != "no_pending_input" || d.NextEligibleAt != nil {
				t.Fatalf("retired head still affected diagnostic: %#v", d)
			}
			m, err := f.engine.Send(f.auth[model.ActorSlot1], relay.SendRequest{ID: "new", Text: "new input"})
			if err != nil {
				t.Fatal(err)
			}
			// No maintenance has pruned the old deferral, and no probe has run
			// against this new head or binding generation.
			d = n.nativeDiagnostics()["participants"].(map[model.ActorID]nativeReachability)[model.ActorSlot2]
			if d.Reason != "wake_eligible" || d.HeadID != m.ID || d.NextEligibleAt != nil {
				t.Fatalf("new head inherited a stale queue observation: %#v", d)
			}
		})
	}
}

func TestNativeDiagnosticsQueueDeferralPreservesDisabledAndDeliveryPriority(t *testing.T) {
	for _, transition := range []string{"disabled", "delivering", "unknown"} {
		t.Run(transition, func(t *testing.T) {
			f := newNudgeFixture(t)
			f.waker.codexCommand, _ = os.Executable()
			f.waker.queuePending = func(context.Context, string, string, string) (bool, error) { return true, nil }
			f.send("pending")
			want := "wake_disabled"
			if transition == "disabled" {
				if err := f.engine.SetWakeEnabled(false); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.engine.Claim(context.Background(), f.auth[model.ActorSlot2], false); err != nil {
					t.Fatal(err)
				}
				want = "delivery_in_flight"
				if transition == "unknown" {
					oldWaker := f.waker
					f.restart() // Unacknowledged delivery recovers as unknown.
					f.waker = oldWaker
					want = "uncertain_delivery"
				}
			}
			n := &nativeHostRuntime{engine: f.engine, waker: f.waker}
			d := n.nativeDiagnostics()["participants"].(map[model.ActorID]nativeReachability)[model.ActorSlot2]
			if d.Reason != want || d.NextEligibleAt != nil {
				t.Fatalf("queue observation displaced %s priority: %#v", want, d)
			}
		})
	}
}
