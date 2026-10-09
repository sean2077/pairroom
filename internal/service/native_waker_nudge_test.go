package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/claudewake"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

// nudgeFixture drives a real relay Engine and waker on one controlled clock.
// slot2 defaults to a Codex target; its relay calls, Stop parks and collections
// are the Turn-boundary signals the outstanding-nudge rule infers from.
type nudgeFixture struct {
	t             *testing.T
	dir           string
	now           time.Time
	targetRuntime model.RuntimeKind
	engine        *relay.Engine
	waker         *nativeWaker
	auth          map[model.ActorID]relay.Auth
	runs          int
	runErr        error
}

func newNudgeFixture(t *testing.T) *nudgeFixture {
	t.Helper()
	return newNudgeFixtureForRuntime(t, model.RuntimeCodex)
}

func newNudgeFixtureForRuntime(t *testing.T, targetRuntime model.RuntimeKind) *nudgeFixture {
	t.Helper()
	f := &nudgeFixture{t: t, dir: t.TempDir(), now: time.Date(2026, time.September, 30, 5, 0, 0, 0, time.UTC), targetRuntime: targetRuntime, auth: map[model.ActorID]relay.Auth{}}
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
	runtimes := map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: f.targetRuntime}
	if f.targetRuntime == model.RuntimeClaude {
		runtimes[model.ActorSlot1] = model.RuntimeCodex
	}
	engine, err := relay.Open(relay.Config{RoomID: "room", Store: log, Now: clock, Runtimes: runtimes})
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
		Claude: func(relay.WakeCandidate) (claudewake.Send, error) {
			return func(context.Context, string) error {
				f.runs++
				return f.runErr
			}, nil
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

// TestNativeWakeUnsupportedRuntimeTransportDoesNotReserve covers the policy gate:
// Grok's NativeWakePolicy yields no supported transport, so nativeWakeSuppression
// rejects the burst before any capability lookup, wake lease, or reservation. It
// does not reach Wake's dispatch guard, which stays unreachable while Supported()
// names exactly the handled transports.
func TestNativeWakeUnsupportedRuntimeTransportDoesNotReserve(t *testing.T) {
	f := newNudgeFixtureForRuntime(t, model.RuntimeGrok)
	prepares := 0
	f.waker.claude = func(relay.WakeCandidate) (claudewake.Send, error) {
		prepares++
		return nil, errors.New("wrong transport capability")
	}
	id := f.send("unsupported-transport")
	f.expect(0, 0)
	if prepares != 0 || f.waker.InUse() || len(f.waker.attempts) != 0 {
		t.Fatal("unsupported runtime read another transport's capability or spent a wake lease/budget")
	}
	if f.audits("wake suppressed (unsupported_runtime)") != 1 {
		t.Fatalf("unsupported-runtime audit = %#v", f.engine.Snapshot().Audit)
	}
	if c, ok := f.engine.WakeCandidate(id); !ok || !c.QueueStart || c.Reserved {
		t.Fatalf("unsupported runtime changed queued input = %#v, %v", c, ok)
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
	// The successor is now visible and audited before the restart as well as
	// once in the new waker, whose local suppression cache starts empty.
	if f.audits("wake suppressed (nudge_pending)") != 2 {
		t.Fatalf("audit = %#v", f.engine.Snapshot().Audit)
	}
	// Replay accepts the new reason, and the fallback bound still releases input.
	f.restart()
	if f.audits("wake suppressed (nudge_pending)") != 2 {
		t.Fatalf("replayed audit = %#v", f.engine.Snapshot().Audit)
	}
	f.advance(relay.WakeRenewAfter)
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	f.expect(2, 2)
}

// Acknowledged inbox progress can supersede Claude's wake need before Turn end.
// These fixtures establish relay transitions, not physical nudge consumption
// or an authenticated vendor session.
func TestNativeClaudeWakeCollectedBurstRechecksAtMinimumInterval(t *testing.T) {
	f := newNudgeFixtureForRuntime(t, model.RuntimeClaude)
	f.startBusyTurn()
	first := f.send("m1")
	f.expect(1, 1)
	f.advance(time.Second)
	f.collect()
	f.advance(30 * time.Second)
	head := f.send("m2")
	f.expect(1, 1)
	if f.audits("wake suppressed (minimum_interval)") != 1 || f.audits("wake suppressed (nudge_pending)") != 0 {
		t.Fatalf("Claude's new burst must use the rate limit, not Codex Turn-end tracking: %#v", f.engine.Snapshot().Audit)
	}
	// No Stop has occurred. Maintenance should wake this new burst at the
	// existing rate deadline, without waiting for the ten-minute fallback.
	f.advance(nativeWakeMinimumInterval - 31*time.Second)
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	f.expect(2, 2)
	if f.audits("wake submitted") != 2 {
		t.Fatalf("Claude outcomes = %#v; want two submitted effects", f.engine.Snapshot().Audit)
	}
	reservations := f.engine.WakeReservations()
	if reservations[0].MessageID != first || reservations[1].MessageID != head {
		t.Fatalf("wake reservations = %#v; want each burst reserved once", reservations)
	}
	// Replaying the still-queued second burst must never re-submit its effect.
	f.restart()
	f.advance(2 * time.Hour)
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	f.expect(2, 2)
}

func TestNativeClaudeWakeAfterMidTurnHandoffAndTurnEnd(t *testing.T) {
	f := newNudgeFixtureForRuntime(t, model.RuntimeClaude)
	f.startBusyTurn()
	f.send("m1")
	f.advance(time.Second)
	f.collect()
	f.advance(time.Second)
	f.stop()
	// The handoff superseded the earlier wake need. Turn end must not reinstate
	// it and delay fresh input; a held physical nudge may still arrive later.
	f.advance(nativeWakeMinimumInterval + time.Second)
	f.send("m2")
	f.expect(2, 2)
}

func TestNativeClaudeWakeUnconsumedNudgePreservesRoomBudget(t *testing.T) {
	f := newNudgeFixtureForRuntime(t, model.RuntimeClaude)
	f.startBusyTurn()
	// An inbound hold can leave the nudge undelivered while the target uses
	// other relay operations. Neither those calls nor Management cancellation
	// of the queued input prove that the wake reached the native session.
	for i := 0; i < nativeWakeHourlyLimit; i++ {
		if i > 0 {
			switch i % 3 {
			case 0:
				f.activity()
			case 1:
				if _, err := f.engine.Peer(f.auth[model.ActorSlot2]); err != nil {
					t.Fatal(err)
				}
			case 2:
				if _, err := f.engine.Send(f.auth[model.ActorSlot2], relay.SendRequest{ID: fmt.Sprintf("own-send-%d", i), Text: "update", To: model.ActorUser}); err != nil {
					t.Fatal(err)
				}
			}
		}
		id := f.send(fmt.Sprintf("cancelled-burst-%d", i))
		if err := f.engine.Cancel(id); err != nil {
			t.Fatal(err)
		}
		if i+1 < nativeWakeHourlyLimit {
			f.advance(nativeWakeMinimumInterval)
		}
	}
	if f.runs != 1 || f.reservations() != 1 {
		t.Errorf("unconsumed Claude nudge: vendor runs=%d reservations=%d, want 1/1", f.runs, f.reservations())
	}
	// The budget belongs to the whole Room. Claude's canceled input must not
	// prevent this independent Codex peer from receiving its first nudge.
	peer, err := f.engine.Send(f.auth[model.ActorSlot2], relay.SendRequest{ID: "codex-peer", Text: "peer task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.waker.Wake(context.Background(), peer.ID); err != nil {
		t.Fatal(err)
	}
	if f.audits("wake accepted") != 1 || f.audits("wake suppressed (hourly_limit)") != 0 {
		t.Errorf("Codex peer accepted=%d hourly-limit suppressions=%d, want 1/0", f.audits("wake accepted"), f.audits("wake suppressed (hourly_limit)"))
	}
	if f.runs != 2 || f.reservations() != 2 {
		t.Errorf("both peers: vendor runs=%d reservations=%d, want 2/2", f.runs, f.reservations())
	}
}

func TestNativeClaudeWakeUnconsumedNudgeRenewsAfterTenMinutes(t *testing.T) {
	for _, cancelHead := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled_head=%t", cancelHead), func(t *testing.T) {
			f := newNudgeFixtureForRuntime(t, model.RuntimeClaude)
			f.startBusyTurn()
			first := f.send("first")
			if cancelHead {
				if err := f.engine.Cancel(first); err != nil {
					t.Fatal(err)
				}
			}
			f.advance(relay.WakeRenewAfter - time.Second)
			f.activity()
			next := f.send("next")
			f.expect(1, 1)
			if !f.waker.InUse() {
				t.Fatal("unattempted successor lost its lease before renewal")
			}
			f.advance(time.Second)
			f.waker.Reconcile(context.Background())
			f.waker.workers.Wait()
			f.expect(2, 2)
			if got := f.engine.WakeReservations()[1].MessageID; got != next {
				t.Fatalf("renewal reserved %s; want new message %s", got, next)
			}
			// Renewing the successor never authorizes a retry of either effect.
			f.restart()
			f.advance(2 * time.Hour)
			f.waker.Reconcile(context.Background())
			f.waker.workers.Wait()
			f.expect(2, 2)
		})
	}
}

func TestNativeWakeNextHeadRechecksAfterFailureOrConsumption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		runtime model.RuntimeKind
		failed  bool
	}{
		{"Claude failure", model.RuntimeClaude, true},
		{"Codex failure", model.RuntimeCodex, true},
		{"idle Codex activity", model.RuntimeCodex, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNudgeFixtureForRuntime(t, tc.runtime)
			f.stop()
			f.advance(time.Second)
			if tc.failed {
				f.runErr = errors.New("fixture wake failure")
			}
			first := f.send("first")
			f.runErr = nil
			f.advance(time.Second)
			if !tc.failed {
				f.activity()
			}
			next := f.send("next")
			f.expect(1, 1)
			if f.audits("wake suppressed (minimum_interval)") != 1 || !f.waker.InUse() {
				t.Fatal("unattempted successor was neither rate-deferred nor leased")
			}
			f.advance(nativeWakeMinimumInterval - time.Second)
			f.waker.Reconcile(context.Background())
			f.waker.workers.Wait()
			f.expect(2, 2)
			if got := f.engine.WakeReservations()[1].MessageID; got != next {
				t.Fatalf("reservation = %s; want unattempted successor %s", got, next)
			}
			// Wake ownership must not change FIFO delivery or retry the first ID.
			if claim := f.claim(false); claim == nil || claim.ID != first {
				t.Fatalf("FIFO first claim = %#v; want %s", claim, first)
			}
		})
	}
}

func TestNativeWakeMaintenanceLeasesOutstandingSuccessorAfterRestart(t *testing.T) {
	f := newNudgeFixtureForRuntime(t, model.RuntimeClaude)
	f.startBusyTurn()
	first := f.send("first")
	f.advance(time.Minute)
	// Queue without running the publication callback. Maintenance must discover
	// the successor independently, including after the Service loses its maps.
	next, err := f.engine.Send(f.auth[model.ActorSlot1], relay.SendRequest{ID: "next", Text: "next"})
	if err != nil {
		t.Fatal(err)
	}
	f.restart()
	for i := 0; i < 3; i++ {
		f.waker.Reconcile(context.Background())
		f.waker.workers.Wait()
	}
	f.expect(1, 1)
	if f.audits("wake suppressed (nudge_pending)") != 1 || !f.waker.InUse() {
		t.Fatal("maintenance did not coalesce and lease the pending successor")
	}
	if heads := f.engine.WakeHeads(); len(heads) != 1 || heads[0].MessageID != next.ID || heads[0].Reserved {
		t.Fatalf("maintenance heads = %#v; want unattempted successor", heads)
	}
	if err := f.engine.Cancel(next.ID); err != nil {
		t.Fatal(err)
	}
	f.waker.Reconcile(context.Background())
	f.waker.workers.Wait()
	if f.waker.InUse() {
		t.Fatal("cancelled successor retained a runtime lease")
	}
	if heads := f.engine.WakeHeads(); len(heads) != 1 || heads[0].MessageID != first || !heads[0].Reserved {
		t.Fatalf("attempted head = %#v", heads)
	}
	f.expect(1, 1)
}

type blockedNudgeCandidate struct {
	*relay.Engine
	entered, release chan struct{}
	once             sync.Once
}

func (r *blockedNudgeCandidate) WakeCandidate(id string) (relay.WakeCandidate, bool) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return r.Engine.WakeCandidate(id)
}

func TestNativeWakeMaintenanceLeasesBeforeSuppressionWorkerStarts(t *testing.T) {
	f := newNudgeFixtureForRuntime(t, model.RuntimeClaude)
	f.startBusyTurn()
	f.send("first")
	if _, err := f.engine.Send(f.auth[model.ActorSlot1], relay.SendRequest{ID: "next", Text: "next"}); err != nil {
		t.Fatal(err)
	}
	r := &blockedNudgeCandidate{Engine: f.engine, entered: make(chan struct{}), release: make(chan struct{})}
	f.waker.relay = r
	t.Cleanup(func() { close(r.release); f.waker.workers.Wait() })
	f.waker.Reconcile(context.Background())
	select {
	case <-r.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("maintenance did not launch suppression observation")
	}
	// The worker has not observed its candidate or registered a deferral yet.
	// Maintenance's synchronous lease alone must exclude an idle close.
	if !f.waker.InUse() || f.waker.tryBeginIdleClose(func() bool { return true }) {
		t.Fatal("idle close passed before the suppression worker acquired its lease")
	}
}

func TestNativeWakeMaintenanceAuditsPendingAfterPriorWorkerFinishes(t *testing.T) {
	f := newNudgeFixtureForRuntime(t, model.RuntimeClaude)
	f.startBusyTurn()
	f.send("first")
	if _, err := f.engine.Send(f.auth[model.ActorSlot1], relay.SendRequest{ID: "next", Text: "next"}); err != nil {
		t.Fatal(err)
	}
	// Admission is held by an older target worker while maintenance discovers
	// the new head. Registering its lease must not mark the audit as completed.
	if !f.waker.begin(model.ActorSlot2) {
		t.Fatal("could not hold target worker admission")
	}
	f.waker.Reconcile(context.Background())
	if !f.waker.InUse() || f.audits("wake suppressed (nudge_pending)") != 0 {
		t.Fatal("maintenance lost the lease or audited without worker admission")
	}
	f.waker.end(model.ActorSlot2)
	for i := 0; i < 3; i++ {
		f.waker.Reconcile(context.Background())
		f.waker.workers.Wait()
	}
	if f.audits("wake suppressed (nudge_pending)") != 1 || !f.waker.InUse() {
		t.Fatal("pending audit was lost or duplicated after the older worker ended")
	}
}
