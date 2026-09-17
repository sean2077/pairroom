package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func reopenEngine(t *testing.T, dir string) *Engine {
	t.Helper()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(Config{RoomID: "room", Store: log, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestWakeConfigDefaultsPersistAndGuardIdleBoundary(t *testing.T) {
	e, a, dir := testEngine(t)
	if !e.WakeEnabled() {
		t.Fatal("wake must default enabled without a durable config fact")
	}
	if err := e.SetWakeEnabled(true); err != nil {
		t.Fatalf("idempotent enable must not append: %v", err)
	}

	// A delivering (unacknowledged) message blocks the idle boundary.
	if _, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "guard-1", Text: "hold"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.Claim(ctx, a[model.ActorSlot2], false); err != nil {
		t.Fatal(err)
	}
	if err := e.SetWakeEnabled(false); !errors.Is(err, ErrWakeRoomBusy) {
		t.Fatalf("delivering guard = %v, want ErrWakeRoomBusy", err)
	}
	// Replaying an unresolved delivering delivery becomes unknown and still guards.
	reopened := reopenEngine(t, dir)
	if err := reopened.SetWakeEnabled(false); !errors.Is(err, ErrWakeRoomBusy) {
		t.Fatalf("unknown guard = %v, want ErrWakeRoomBusy", err)
	}
}

func TestWakeConfigChangeIsDurableAndReplays(t *testing.T) {
	e, _, dir := testEngine(t)
	if err := e.SetWakeEnabled(false); err != nil {
		t.Fatal(err)
	}
	if e.WakeEnabled() {
		t.Fatal("config change must apply immediately")
	}
	var found bool
	for _, entry := range e.Snapshot().Audit {
		if entry.Kind == EventWakeConfig && entry.Actor == model.ActorUser && entry.Detail == "wake disabled" {
			found = true
		}
	}
	if !found {
		t.Fatal("wake config change must be audited")
	}
	reopened := reopenEngine(t, dir)
	if reopened.WakeEnabled() {
		t.Fatal("wake config must survive replay")
	}
	if err := reopened.SetWakeEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !reopenEngine(t, dir).WakeEnabled() {
		t.Fatal("re-enable must survive replay")
	}
}

func TestWakeReservationDeduplicatesAcrossRestart(t *testing.T) {
	e, a, dir := testEngine(t)
	msg, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "wake-1", Text: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ReserveWake(msg.ID, model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if err := e.ReserveWake(msg.ID, model.ActorSlot2); !errors.Is(err, ErrWakeReserved) {
		t.Fatalf("duplicate reservation = %v, want ErrWakeReserved", err)
	}
	if err := e.ReserveWake("", model.ActorSlot2); err == nil {
		t.Fatal("empty message id must be rejected")
	}
	if err := e.ReserveWake("x", model.ActorUser); err == nil {
		t.Fatal("non-participant target must be rejected")
	}
	reservations := e.WakeReservations()
	if len(reservations) != 1 || reservations[0].MessageID != msg.ID || reservations[0].Target != model.ActorSlot2 || reservations[0].At.IsZero() {
		t.Fatalf("reservations = %#v", reservations)
	}

	reopened := reopenEngine(t, dir)
	history := reopened.WakeReservations()
	if len(history) != 1 || history[0].MessageID != msg.ID || history[0].At.IsZero() {
		t.Fatalf("replayed history = %#v", history)
	}
	if err := reopened.ReserveWake(msg.ID, model.ActorSlot2); !errors.Is(err, ErrWakeReserved) {
		t.Fatalf("post-restart duplicate = %v, want ErrWakeReserved", err)
	}
}

func TestRecordWakeEnforcesRedactionVocabulary(t *testing.T) {
	e, _, _ := testEngine(t)
	if err := e.RecordWake("accepted", "", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWake("suppressed", "minimum_interval", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWake("failed", "command_timeout", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []struct{ outcome, reason string }{
		{"accepted", "minimum_interval"}, // accepted carries no reason
		{"woken", ""},
		{"failed", "thread 01a0ae1f-cecf-74f3-926a-1701f4f09249 exited"},
		{"suppressed", "burst because body contains secret"},
		{"", ""},
	} {
		if err := e.RecordWake(bad.outcome, bad.reason, model.ActorSlot2); err == nil {
			t.Fatalf("outcome %q reason %q must be rejected", bad.outcome, bad.reason)
		}
	}
	if err := e.RecordWake("accepted", "", model.ActorUser); err == nil {
		t.Fatal("non-participant target must be rejected")
	}
	var details []string
	for _, entry := range e.Snapshot().Audit {
		if entry.Kind == EventWakeAttempted {
			details = append(details, entry.Detail)
		}
	}
	want := []string{"wake accepted", "wake suppressed (minimum_interval)", "wake failed (command_timeout)"}
	if len(details) != len(want) {
		t.Fatalf("audit details = %#v", details)
	}
	for i := range want {
		if details[i] != want[i] {
			t.Fatalf("audit detail %d = %q, want %q", i, details[i], want[i])
		}
	}
}

func TestWakeCandidateFactsAreAtomic(t *testing.T) {
	e, a, _ := testEngine(t)
	if _, ok := e.WakeCandidate("missing"); ok {
		t.Fatal("unknown message must not produce a candidate")
	}
	first, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "cand-1", Text: "first"})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := e.WakeCandidate(first.ID)
	if !ok {
		t.Fatal("queued message must produce a candidate")
	}
	if !c.Enabled || !c.QueueStart || c.WaiterActive || c.Delivering {
		t.Fatalf("candidate = %#v", c)
	}
	if c.Target != model.ActorSlot2 || c.Runtime.Canonical() != model.RuntimeCodex || c.SessionID != "session-slot2" {
		t.Fatalf("candidate binding facts = %#v", c)
	}

	// A second queued message does not start a fresh burst; the oldest does.
	second, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "cand-2", Text: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := e.WakeCandidate(second.ID); !ok || c.QueueStart {
		t.Fatalf("second candidate = %#v, %v", c, ok)
	}
	if c, ok := e.WakeCandidate(first.ID); !ok || !c.QueueStart {
		t.Fatalf("first candidate = %#v, %v", c, ok)
	}

	// A registered Claim waiter makes the candidate report WaiterActive.
	e.mu.Lock()
	e.waiters[model.ActorSlot2] = 1
	e.mu.Unlock()
	if c, ok := e.WakeCandidate(first.ID); !ok || !c.WaiterActive {
		t.Fatalf("candidate with waiter = %#v, %v", c, ok)
	}
	e.mu.Lock()
	e.waiters[model.ActorSlot2] = 0
	e.mu.Unlock()
	if c, ok := e.WakeCandidate(first.ID); !ok || c.WaiterActive {
		t.Fatalf("candidate after waiter left = %#v, %v", c, ok)
	}

	// Claimed messages are no longer candidates.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.Claim(ctx, a[model.ActorSlot2], false); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.WakeCandidate(first.ID); ok {
		t.Fatal("a claimed message must not produce a candidate")
	}
}

func TestClaimRegistersBlockedWaiter(t *testing.T) {
	e, a, _ := testEngine(t)
	waitCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.Claim(waitCtx, a[model.ActorSlot2], false)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		registered := e.waiters[model.ActorSlot2]
		e.mu.Unlock()
		if registered == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked Claim never registered as waiter (count %d)", registered)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	deadline = time.Now().Add(2 * time.Second)
	for {
		e.mu.Lock()
		registered := e.waiters[model.ActorSlot2]
		e.mu.Unlock()
		if registered == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cancelled Claim leaked waiter registration (count %d)", registered)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestWakeCandidateReflectsDeliveringAndDisabled(t *testing.T) {
	e, a, _ := testEngine(t)
	msg, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "cand-3", Text: "deliver me"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.Claim(ctx, a[model.ActorSlot2], false); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.WakeCandidate(msg.ID); ok {
		t.Fatal("a delivering message is no longer a wake candidate")
	}
	late, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "cand-4", Text: "behind delivering"})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := e.WakeCandidate(late.ID)
	if !ok || !c.Delivering || !c.QueueStart {
		t.Fatalf("candidate behind delivering = %#v, %v", c, ok)
	}
	// Config disables candidacy facts without hiding them: the waker decides.
	unresolved := e.SetWakeEnabled(false)
	if !errors.Is(unresolved, ErrWakeRoomBusy) {
		t.Fatalf("config change with delivering message = %v, want ErrWakeRoomBusy", unresolved)
	}
}
