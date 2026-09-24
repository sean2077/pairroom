package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

func wakeEngine(t *testing.T, clock *atomic.Int64) (*relay.Engine, map[model.ActorID]relay.Auth, string) {
	t.Helper()
	dir := t.TempDir()
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := relay.Open(relay.Config{RoomID: "live", Store: log, Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() }, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeCodex, model.ActorSlot2: model.RuntimeCodex}})
	if err != nil {
		t.Fatal(err)
	}
	a := map[model.ActorID]relay.Auth{}
	for _, slot := range model.SlotActors() {
		b, err := e.Bind(slot, relay.BindRequest{BindID: "bind-" + string(slot), CredentialHash: relay.Digest("secret"), SessionID: "session-" + string(slot)})
		if err != nil {
			t.Fatal(err)
		}
		a[slot] = relay.Auth{Slot: slot, BindID: b.BindID, Generation: b.Generation, SessionID: b.SessionID, Secret: "secret"}
	}
	t.Cleanup(func() { _ = e.Close() })
	return e, a, dir
}

func TestNativeWakeDeferredHeadsProgressWithoutNewTraffic(t *testing.T) {
	var clock, calls atomic.Int64
	clock.Store(1800000000)
	e, a, _ := wakeEngine(t, &clock)
	w := newNativeWaker(nativeWakerConfig{Relay: e, Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() }, Wait: func(context.Context, time.Duration) error { return nil }, Run: func(context.Context, string, ...string) error { calls.Add(1); return nil }})
	defer w.Close()
	reconcile := func() { w.Reconcile(context.Background()); w.workers.Wait() }
	send := func(slot model.ActorID, id string) relay.Message {
		m, err := e.Send(a[slot], relay.SendRequest{ID: id, Text: "fixture"})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	collect := func(slot model.ActorID) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		m, err := e.Claim(ctx, a[slot], false)
		if err != nil || m == nil {
			t.Fatalf("claim %+v %v", m, err)
		}
		if err := e.Ack(a[slot], m.ID, m.Receipt); err != nil {
			t.Fatal(err)
		}
	}
	send(model.ActorSlot1, "first")
	reconcile()
	collect(model.ActorSlot2)
	send(model.ActorSlot2, "reply")
	reconcile()
	collect(model.ActorSlot1)
	if calls.Load() != 2 {
		t.Fatal("opposite slots share cooldown", calls.Load())
	}
	pending := send(model.ActorSlot1, "followup")
	reconcile()
	if calls.Load() != 2 || !w.InUse() {
		t.Fatal("unattempted head not deferred")
	}
	for i := 0; i < 5; i++ {
		reconcile()
	}
	clock.Add(61)
	reconcile()
	if calls.Load() != 3 {
		t.Fatal("cooldown expiry stranded head", calls.Load())
	}
	page, _ := e.History(relay.HistoryQuery{ID: pending.ID})
	if page.Messages[0].State != "queued" {
		t.Fatal("wake consumed body")
	}
	// Accepted-but-uncollected wake never repeats, including in a new waker.
	again := newNativeWaker(nativeWakerConfig{Relay: e, Now: func() time.Time { return time.Unix(clock.Load(), 0) }, Wait: func(context.Context, time.Duration) error { return nil }, Run: func(context.Context, string, ...string) error { calls.Add(1); return nil }})
	clock.Add(3601)
	again.Reconcile(context.Background())
	again.Close()
	if calls.Load() != 3 {
		t.Fatal("reserved effect retried")
	}
}

func TestNativeWakeHourlyDeferralSurvivesReplay(t *testing.T) {
	var clock, calls atomic.Int64
	clock.Store(1800000000)
	e, a, dir := wakeEngine(t, &clock)
	config := nativeWakerConfig{Relay: e, HourlyLimit: 1, Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() }, Wait: func(context.Context, time.Duration) error { return nil }, Run: func(context.Context, string, ...string) error { calls.Add(1); return nil }}
	w := newNativeWaker(config)
	first, _ := e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "one", Text: "one"})
	if err := w.Wake(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	_ = e.Cancel(first.ID)
	next, _ := e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "two", Text: "two"})
	clock.Add(61)
	if err := w.Wake(context.Background(), next.ID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || !w.InUse() {
		t.Fatal("missing hourly deferral")
	}
	w.Close()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := relay.Open(relay.Config{RoomID: "live", Store: log, Now: config.Now, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot2: model.RuntimeCodex}})
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	config.Relay = reloaded
	w = newNativeWaker(config)
	defer w.Close()
	w.Reconcile(context.Background())
	w.workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("lost durable hourly budget")
	}
	clock.Add(3540)
	w.Reconcile(context.Background())
	w.workers.Wait()
	if calls.Load() != 2 {
		t.Fatal("replayed unattempted head stranded")
	}
}

func TestNativeWakeRechecksCancelledHeadAndDisabledRoom(t *testing.T) {
	var clock, calls atomic.Int64
	clock.Store(1800000000)
	e, a, _ := wakeEngine(t, &clock)
	first, _ := e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "one", Text: "one"})
	e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "two", Text: "two"})
	var firstGrace atomic.Bool
	w := newNativeWaker(nativeWakerConfig{Relay: e, Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() }, Wait: func(context.Context, time.Duration) error {
		if firstGrace.CompareAndSwap(false, true) {
			return e.Cancel(first.ID)
		}
		return nil
	}, Run: func(context.Context, string, ...string) error { calls.Add(1); return nil }})
	defer w.Close()
	w.Wake(context.Background(), first.ID)
	w.Reconcile(context.Background())
	w.workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("new head lost grace ownership")
	}
	heads := e.WakeHeads()
	_ = e.Cancel(heads[0].MessageID)
	e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "three", Text: "three"})
	w.Reconcile(context.Background())
	w.workers.Wait()
	if !w.InUse() {
		t.Fatal("no deferred head")
	}
	if err := e.SetWakeEnabled(false); err != nil {
		t.Fatal(err)
	}
	clock.Add(61)
	w.Reconcile(context.Background())
	w.workers.Wait()
	if calls.Load() != 1 || w.InUse() {
		t.Fatal("disabled deferred wake fired or retained lease")
	}
	if err := e.SetWakeEnabled(true); err != nil {
		t.Fatal(err)
	}
	w.Reconcile(context.Background())
	w.workers.Wait()
	if calls.Load() != 2 {
		t.Fatal("enabled unattempted work stranded")
	}
}

// Only a durably reserved, possibly submitted wake (or a rate-limit deferral) may
// hold the Native runtime lease. A capability probe waiting out its grace delay
// carries no effect, so a Claude slot without a captured inbox must not keep a
// Runtime resident forever: the manager would refresh last-used every few seconds
// and the Room could never idle-suspend.
func TestNativeCapabilityProbeDoesNotHoldTheRuntimeLease(t *testing.T) {
	var clock, calls atomic.Int64
	clock.Store(1800000000)
	log, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, err := relay.Open(relay.Config{RoomID: "probe", Store: log, Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() }, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeClaude}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	a := map[model.ActorID]relay.Auth{}
	for _, slot := range model.SlotActors() {
		b, err := e.Bind(slot, relay.BindRequest{BindID: "bind-" + string(slot), CredentialHash: relay.Digest("secret"), SessionID: "session-" + string(slot)})
		if err != nil {
			t.Fatal(err)
		}
		a[slot] = relay.Auth{Slot: slot, BindID: b.BindID, Generation: b.Generation, SessionID: b.SessionID, Secret: "secret"}
	}
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	note := func() {
		select {
		case entered <- struct{}{}:
		default:
		}
	}
	// No Claude capability is wired, so the probe can only end in a deferral.
	w := newNativeWaker(nativeWakerConfig{Relay: e, Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() },
		Wait: func(context.Context, time.Duration) error { note(); <-release; return nil },
		Run:  func(context.Context, string, ...string) error { calls.Add(1); return nil }})
	// Release the grace delay before Close: a failing assertion must not hang.
	defer w.Close()
	defer unblock()
	m, err := e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "probe", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	w.Reconcile(context.Background())
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("wake worker never reached its grace delay")
	}
	if w.InUse() {
		t.Fatal("pre-reservation probe held the runtime lease")
	}
	unblock()
	w.workers.Wait()
	if calls.Load() != 0 || w.InUse() {
		t.Fatal("capability probe attempted a wake or kept the lease")
	}
	if page, err := e.History(relay.HistoryQuery{ID: m.ID}); err != nil || page.Messages[0].State != "queued" {
		t.Fatal("probe consumed the queued head")
	}
}

func TestNativeWakeSuppressionDoesNotFloodAudit(t *testing.T) {
	var clock atomic.Int64
	clock.Store(1800000000)
	e, a, _ := wakeEngine(t, &clock)
	w := newNativeWaker(nativeWakerConfig{Relay: e, Wait: func(context.Context, time.Duration) error { return nil }, Run: func(context.Context, string, ...string) error { return nil }})
	defer w.Close()
	_ = e.SetWakeEnabled(false)
	m, _ := e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "one", Text: "one"})
	for i := 0; i < 100; i++ {
		if err := w.Wake(context.Background(), m.ID); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, a := range e.Snapshot().Audit {
		if a.Kind == relay.EventWakeAttempted {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d duplicate suppression records", n)
	}
}

func TestNativeWakeCollectorExitRechecksWithoutAnotherPublication(t *testing.T) {
	var clock, calls atomic.Int64
	clock.Store(1800000000)
	e, a, _ := wakeEngine(t, &clock)
	w := newNativeWaker(nativeWakerConfig{Relay: e, Wait: func(context.Context, time.Duration) error { return nil }, Run: func(context.Context, string, ...string) error { calls.Add(1); return nil }})
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _, _ = e.WaitForPending(ctx, a[model.ActorSlot2]) }()
	deadline := time.Now().Add(time.Second)
	for !e.Summary().Bindings[model.ActorSlot2].CollectorActive {
		if time.Now().After(deadline) {
			t.Fatal("collector not registered")
		}
		time.Sleep(time.Millisecond)
	}
	// The pending-readiness collector exits without consuming the arriving input.
	_, err := e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "pending", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	w.Reconcile(context.Background())
	w.workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("collector exit stranded queued input")
	}
}

type deferredWakeRuntime struct {
	*leaseRuntime
	deferred atomic.Bool
}

func (r *deferredWakeRuntime) PendingWake() bool { return r.deferred.Load() }
func TestNativeDeferredWakeRetainsLeaseWithoutInventingHTTPActivity(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	r := &deferredWakeRuntime{leaseRuntime: &leaseRuntime{closed: make(chan struct{})}}
	r.deferred.Store(true)
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) { return r, nil }, RuntimeManagerConfig{Limit: 1, IdleTimeout: time.Minute, PollInterval: time.Millisecond, CloseTimeout: time.Second, Now: func() time.Time { return time.Unix(0, now.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, nil)
	activateRuntime(t, manager, rooms[0].ID)
	now.Add(int64(2 * time.Hour))
	time.Sleep(20 * time.Millisecond)
	status := manager.Status(rooms[0].ID)
	if status.Phase != RuntimeActive || status.HTTPInUse || !status.WakePending {
		t.Fatalf("wrong lease evidence %+v", status)
	}
	r.deferred.Store(false)
	now.Add(int64(2 * time.Minute))
	waitRuntimeStatus(t, manager, rooms[0].ID, func(s RuntimeStatus) bool { return s.Phase == RuntimeSuspended })
}
