package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

// nudgeFixture drives a real relay Engine and waker on one controlled clock.
// slot2 is a Codex target; its relay calls, Stop parks and collections are the
// Turn-boundary signals the outstanding-nudge rule infers from.
type nudgeFixture struct {
	t      *testing.T
	dir    string
	now    time.Time
	engine *relay.Engine
	waker  *nativeWaker
	auth   map[model.ActorID]relay.Auth
	runs   int
	runErr error
}

func newNudgeFixture(t *testing.T) *nudgeFixture {
	t.Helper()
	f := &nudgeFixture{t: t, dir: t.TempDir(), now: time.Date(2026, time.September, 30, 5, 0, 0, 0, time.UTC), auth: map[model.ActorID]relay.Auth{}}
	f.open()
	for _, slot := range model.SlotActors() {
		secret, session := "secret-"+string(slot), "session-"+string(slot)
		b, err := f.engine.Bind(slot, relay.BindRequest{BindID: "bind-" + string(slot), CredentialHash: relay.Digest(secret), SessionID: session})
		if err != nil {
			t.Fatal(err)
		}
		f.auth[slot] = relay.Auth{Slot: slot, BindID: b.BindID, Generation: b.Generation, SessionID: session, Secret: secret}
	}
	return f
}

func (f *nudgeFixture) open() {
	f.t.Helper()
	log, err := store.Open(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	clock := func() time.Time { return f.now }
	engine, err := relay.Open(relay.Config{RoomID: "room", Store: log, Now: clock, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = engine.Close() })
	f.engine = engine
	f.waker = newNativeWaker(nativeWakerConfig{
		Relay: engine,
		Now:   clock,
		Wait:  func(context.Context, time.Duration) error { return nil },
		Run: func(context.Context, string, ...string) error {
			f.runs++
			return f.runErr
		},
	})
}

// restart replaces the Engine and waker from the same Event Log, as a Service
// restart or runtime reactivation does.
func (f *nudgeFixture) restart() {
	f.t.Helper()
	if err := f.engine.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.open()
}

func (f *nudgeFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// send queues peer input for slot2 and runs its post-enqueue wake decision.
func (f *nudgeFixture) send(id string) string {
	f.t.Helper()
	m, err := f.engine.Send(f.auth[model.ActorSlot1], relay.SendRequest{ID: id, Text: "task " + id})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.waker.Wake(context.Background(), m.ID); err != nil {
		f.t.Fatal(err)
	}
	return m.ID
}

// activity is an authenticated relay call from the target session.
func (f *nudgeFixture) activity() {
	f.t.Helper()
	if _, err := f.engine.Inspect(f.auth[model.ActorSlot2]); err != nil {
		f.t.Fatal(err)
	}
}

// collect is a foreground relay wait that claims and acknowledges one message.
func (f *nudgeFixture) collect() {
	f.t.Helper()
	f.claim(false)
}

func (f *nudgeFixture) claim(park bool) *relay.Claim {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	claim, err := f.engine.Claim(ctx, f.auth[model.ActorSlot2], park)
	if err != nil {
		f.t.Fatal(err)
	}
	if claim != nil {
		if err := f.engine.Ack(f.auth[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
			f.t.Fatal(err)
		}
	}
	return claim
}

// stop is the Stop hook park at a Turn end with nothing to deliver.
func (f *nudgeFixture) stop() {
	f.t.Helper()
	if claim := f.claim(true); claim != nil {
		f.t.Fatalf("Turn-end park unexpectedly continued with %s", claim.ID)
	}
}

func (f *nudgeFixture) reservations() int { return len(f.engine.WakeReservations()) }

func (f *nudgeFixture) audits(detail string) int {
	count := 0
	for _, entry := range f.engine.Snapshot().Audit {
		if entry.Kind == relay.EventWakeAttempted && entry.Detail == detail {
			count++
		}
	}
	return count
}

func (f *nudgeFixture) expect(runs, reservations int) {
	f.t.Helper()
	if f.runs != runs || f.reservations() != reservations {
		f.t.Fatalf("vendor runs=%d reservations=%d, want %d/%d; audit=%#v", f.runs, f.reservations(), runs, reservations, f.engine.Snapshot().Audit)
	}
}

// startBusyTurn makes slot2 observably mid-Turn: a previous Turn ended and a
// relay call has happened since.
func (f *nudgeFixture) startBusyTurn() {
	f.stop()
	f.advance(time.Second)
	f.activity()
	f.advance(time.Second)
}

func TestNativeWakeMidTurnCollectionDoesNotConsumeQueuedNudge(t *testing.T) {
	f := newNudgeFixture(t)
	f.startBusyTurn()
	f.send("m1")
	f.expect(1, 1)
	// The busy target collects mid-Turn. Its native queue still holds the nudge.
	f.advance(time.Second)
	f.collect()
	f.advance(time.Minute + time.Second)
	f.send("m2")
	f.expect(1, 1)
	if f.audits("wake suppressed (nudge_pending)") != 1 {
		t.Fatalf("audit = %#v", f.engine.Snapshot().Audit)
	}
	if !f.waker.InUse() {
		t.Fatal("nudge_pending head may be cut short by an idle suspend")
	}
	// It collects that one in the same Turn too; the Turn then ends.
	f.collect()
	f.advance(time.Second)
	f.stop()
	// A Turn end alone does not prove the queued nudge ran.
	f.advance(time.Second)
	head := f.send("m3")
	f.expect(1, 1)
	// Identical suppressions coalesce; maintenance adds none while pending.
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	if f.audits("wake suppressed (nudge_pending)") != 1 {
		t.Fatalf("repeated suppression was not coalesced: %#v", f.engine.Snapshot().Audit)
	}
	candidate, ok := f.engine.WakeCandidate(head)
	if !ok || candidate.Reserved {
		t.Fatalf("suppressed head = %#v, %v; want unattempted", candidate, ok)
	}
	// The nudge starts the next Turn, whose first relay call consumes it.
	f.advance(time.Second)
	f.activity()
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	f.expect(2, 2)
	if got := f.engine.WakeReservations()[1].MessageID; got != head {
		t.Fatalf("renewed reservation = %s, want %s", got, head)
	}
	if f.waker.InUse() {
		t.Fatal("settled wake kept the runtime lease")
	}
}

func TestNativeWakeIdleTargetConsumesNudgeOnNextRelayCall(t *testing.T) {
	f := newNudgeFixture(t)
	f.stop()
	f.advance(time.Second)
	first := f.send("m1")
	f.expect(1, 1)
	// Without any target relay call, a new burst is still covered by the nudge.
	if err := f.engine.Cancel(first); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Minute + time.Second)
	f.send("m2")
	f.expect(1, 1)
	f.advance(time.Second)
	f.activity()
	f.collect()
	f.advance(time.Minute + time.Second)
	f.send("m3")
	f.expect(2, 2)
}

func TestNativeWakeContinuationParkIsNotTurnEnd(t *testing.T) {
	f := newNudgeFixture(t)
	f.startBusyTurn()
	f.send("m1")
	f.collect()
	f.advance(time.Minute + time.Second)
	f.send("m2")
	f.expect(1, 1)
	// The Stop park delivers m2, so the Turn continues and the nudge stays queued.
	f.advance(time.Second)
	if f.claim(true) == nil {
		t.Fatal("Stop park did not continue with queued input")
	}
	f.advance(time.Second)
	f.activity()
	f.advance(time.Second)
	f.send("m3")
	f.expect(1, 1)
}

func TestNativeWakeOutstandingNudgeExpiresAfterRenewal(t *testing.T) {
	f := newNudgeFixture(t)
	f.startBusyTurn()
	f.send("m1")
	f.collect()
	f.advance(relay.WakeRenewAfter - time.Second)
	f.send("m2")
	f.expect(1, 1)
	f.advance(time.Second)
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	f.expect(2, 2)
}

func TestNativeWakeFailedAttemptDoesNotBlockNextBurst(t *testing.T) {
	f := newNudgeFixture(t)
	f.startBusyTurn()
	f.runErr = errors.New("codex exited 1")
	f.send("m1")
	f.expect(1, 1)
	if f.audits("wake failed (command_failed)") != 1 {
		t.Fatalf("audit = %#v", f.engine.Snapshot().Audit)
	}
	f.runErr = nil
	f.collect()
	f.advance(time.Minute + time.Second)
	f.send("m2")
	f.expect(2, 2)
}

func TestNativeWakeBindingReplacementClearsOutstandingNudge(t *testing.T) {
	f := newNudgeFixture(t)
	f.startBusyTurn()
	f.send("m1")
	f.collect()
	secret := "secret-replacement"
	b, err := f.engine.Bind(model.ActorSlot2, relay.BindRequest{BindID: "bind-slot2-new", CredentialHash: relay.Digest(secret), SessionID: "session-slot2-new", Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	f.auth[model.ActorSlot2] = relay.Auth{Slot: model.ActorSlot2, BindID: b.BindID, Generation: b.Generation, SessionID: "session-slot2-new", Secret: secret}
	f.advance(time.Minute + time.Second)
	f.send("m2")
	f.expect(2, 2)
}

func TestNativeWakeRestartKeepsReplayedNudgeConservative(t *testing.T) {
	f := newNudgeFixture(t)
	// Idle at the attempt; the record of that is process-local.
	f.stop()
	f.advance(time.Second)
	f.send("m1")
	f.expect(1, 1)
	f.advance(time.Second)
	f.send("m1-followup")
	f.restart()
	// Replay treats the nudge as mid-Turn: this collection cannot consume it.
	f.collect()
	f.collect()
	f.advance(time.Minute + time.Second)
	f.send("m2")
	f.expect(1, 1)
	if f.audits("wake suppressed (nudge_pending)") != 1 {
		t.Fatalf("audit = %#v", f.engine.Snapshot().Audit)
	}
	// Replay accepts the new reason, and the fallback bound still releases input.
	f.restart()
	if f.audits("wake suppressed (nudge_pending)") != 1 {
		t.Fatalf("replayed audit = %#v", f.engine.Snapshot().Audit)
	}
	f.advance(relay.WakeRenewAfter)
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	f.expect(2, 2)
}
