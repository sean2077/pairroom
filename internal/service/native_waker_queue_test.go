package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Existing waker tests simulate the effect transport. Keep their queue probe
// equally isolated; real subprocess/protocol coverage lives in codexwake.
func emptyNativeWakeQueue(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func TestNativeCodexQueuePreventsStackingAcrossRenewalAndRestart(t *testing.T) {
	f := newNudgeFixture(t)
	f.startBusyTurn()
	pending := false
	probes := 0
	install := func() {
		f.waker.queuePending = func(_ context.Context, command, session, nudge string) (bool, error) {
			probes++
			if command != "codex" || session != f.auth[model.ActorSlot2].SessionID || nudge != nativeWakeNudge {
				t.Fatal("probe did not use the configured transport, bound session and fixed nudge")
			}
			return pending, nil
		}
	}
	install()
	f.send("first")
	pending = true // The busy Desktop still displays the accepted prompt.
	f.advance(time.Second)
	f.collect()
	var second string
	for i, id := range []string{"second", "third"} {
		f.advance(relay.WakeRenewAfter + time.Second)
		sent := f.send(id)
		if i == 0 {
			second = sent
		}
		f.expect(1, 1)
		if i == 0 {
			f.restart()
			install()
		}
		f.waker.Reconcile(context.Background())
		f.waker.workers.Wait()
		f.expect(1, 1)
	}
	if !f.waker.InUse() {
		t.Fatal("pending native queue lost the lease needed to recheck unattempted input")
	}
	before := probes
	for i := 0; i < 3; i++ {
		f.waker.Reconcile(context.Background())
		f.waker.workers.Wait()
	}
	if probes != before {
		t.Fatal("pending native queue was polled before its recheck deadline")
	}
	// Consuming/deleting the native prompt permits the next unattempted input,
	// without new peer traffic and without replaying the first reservation.
	pending = false
	f.advance(nativeWakeQueueRecheck)
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	f.expect(2, 2)
	if got := f.engine.WakeReservations()[1].MessageID; got != second {
		t.Fatalf("renewal reserved %q; want oldest unattempted input", got)
	}
}

func TestNativeCodexQueueGuardsInferredConsumption(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "duplicate Stop", true: "idle relay activity"}[idle], func(t *testing.T) {
			f := newNudgeFixture(t)
			if idle {
				f.stop()
				f.advance(time.Second)
			} else {
				f.startBusyTurn()
			}
			f.send("first")
			f.waker.queuePending = func(context.Context, string, string, string) (bool, error) { return true, nil }
			f.advance(time.Second)
			f.collect()
			if !idle {
				f.advance(time.Second)
				f.stop()
				f.advance(time.Second)
				f.activity() // Metadata/auth activity from a repeated hook.
				f.stop()
			}
			f.advance(time.Minute + time.Second)
			f.send("next")
			f.expect(1, 1)
			if f.audits("wake suppressed (nudge_pending)") != 1 {
				t.Fatal("native queued prompt was not audited as pending")
			}
		})
	}
}

func TestNativeCodexQueueProbeFailureDoesNotReserveOrSpendRateBudget(t *testing.T) {
	f := newNudgeFixture(t)
	f.waker.codexCommand = "configured-codex"
	f.waker.queuePending = func(_ context.Context, command, _, _ string) (bool, error) {
		if command != "configured-codex" {
			t.Fatal("probe ignored configured executable")
		}
		return false, errors.New("private vendor output must not enter the audit")
	}
	f.send("first")
	f.expect(0, 0)
	if f.waker.InUse() || len(f.waker.attempts) != 0 || f.audits("wake suppressed (capability_unavailable)") != 1 {
		t.Fatal("unavailable queue probe spent a reservation, rate budget or effect lease")
	}
	f.waker.queuePending = emptyNativeWakeQueue
	f.advance(nativeWakeQueueRecheck)
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	f.expect(1, 1)
}

func TestNativeCodexQueueProbeDoesNotCrossChangedCandidate(t *testing.T) {
	f := newNudgeFixture(t)
	f.waker.queuePending = func(context.Context, string, string, string) (bool, error) {
		heads := f.engine.WakeHeads()
		if len(heads) != 1 {
			t.Fatal("expected one queued candidate")
		}
		if err := f.engine.Cancel(heads[0].MessageID); err != nil {
			t.Fatal(err)
		}
		return false, nil
	}
	f.send("first")
	f.expect(0, 0)
}

func TestNativeCodexQueueProbeAndIdleCloseAdmission(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending queue first", true: "idle close first"}[closeFirst], func(t *testing.T) {
			f := newNudgeFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			f.waker.queuePending = func(context.Context, string, string, string) (bool, error) {
				close(entered)
				<-release
				return !closeFirst, nil
			}
			m, err := f.engine.Send(f.auth[model.ActorSlot1], relay.SendRequest{ID: "first", Text: "task"})
			if err != nil {
				t.Fatal(err)
			}
			f.waker.Schedule(context.Background(), m.ID)
			<-entered
			if closeFirst && !f.waker.tryBeginIdleClose(func() bool { return true }) {
				close(release)
				f.waker.workers.Wait()
				t.Fatal("read-only probe unnecessarily blocked idle close")
			}
			close(release)
			f.waker.workers.Wait()
			f.expect(0, 0)
			if !closeFirst && f.waker.tryBeginIdleClose(func() bool { return true }) {
				t.Fatal("idle close crossed pending native queue lease")
			}
		})
	}
}
