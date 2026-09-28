package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type idleBoundaryRuntime struct {
	*nativeHostRuntime
	cancelWake context.CancelFunc
	closed     chan struct{}
	once       sync.Once
	// Extend the interval after the manager's last advisory lease read.
	armed   atomic.Bool
	reads   atomic.Int64
	sampled chan struct{}
	resume  chan struct{}
}

func (r *idleBoundaryRuntime) URL() string { return "http://fixture.invalid" }
func (r *idleBoundaryRuntime) PendingWake() bool {
	value := r.waker.InUse()
	if r.armed.Load() && r.reads.Add(1) == 2 {
		close(r.sampled)
		<-r.resume
	}
	return value
}
func (r *idleBoundaryRuntime) Close(context.Context) error {
	r.once.Do(func() {
		r.engine.SetDraining(true)
		r.cancelWake()
		r.waker.Close()
		close(r.closed)
	})
	return nil
}

func newIdleBoundaryFixture(t *testing.T, cfg nativeWakerConfig) (*RuntimeManager, *idleBoundaryRuntime, string, relay.Message, *atomic.Int64) {
	t.Helper()
	clock := &atomic.Int64{}
	clock.Store(time.Now().Unix())
	e, a, _ := wakeEngine(t, clock)
	m, err := e.Send(a[model.ActorSlot1], relay.SendRequest{ID: "idle-boundary", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Relay = e
	ctx, cancel := context.WithCancel(context.Background())
	rt := &idleBoundaryRuntime{nativeHostRuntime: &nativeHostRuntime{engine: e, waker: newNativeWaker(cfg), wakeCtx: ctx}, cancelWake: cancel, closed: make(chan struct{}), sampled: make(chan struct{}), resume: make(chan struct{})}
	rt.last.Store(time.Unix(clock.Load()-7200, 0).UnixNano())
	registry, rooms := provisionRuntimeRooms(t, 1)
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) { return rt, nil }, RuntimeManagerConfig{Limit: 1, IdleTimeout: time.Minute, PollInterval: time.Hour, Now: func() time.Time { return time.Unix(clock.Load(), 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { shutdownRuntimeManager(t, manager, nil) })
	activateRuntime(t, manager, rooms[0].ID)
	return manager, rt, rooms[0].ID, m, clock
}

type heldWakeReservation struct {
	*relay.Engine
	committed chan struct{}
	release   chan struct{}
}

func (r heldWakeReservation) ReserveWake(id string, target model.ActorID) error {
	if err := r.Engine.ReserveWake(id, target); err != nil {
		return err
	}
	close(r.committed)
	<-r.release
	return nil
}

func TestNativeIdleCloseCannotCrossAdmittedReservation(t *testing.T) {
	manager, rt, id, m, clock := newIdleBoundaryFixture(t, nativeWakerConfig{Wait: func(context.Context, time.Duration) error { return nil }, Run: func(context.Context, string, ...string) error { return nil }})
	committed, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	rt.waker.relay = heldWakeReservation{Engine: rt.engine, committed: committed, release: release}
	rt.scheduleWake(m.ID)
	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("reservation did not reach durable boundary")
	}
	clock.Add(120)
	if rt.TryBeginIdleClose(time.Unix(clock.Load(), 0)) {
		t.Fatal("idle close crossed admitted reservation")
	}
	manager.reconcile()
	if phase := manager.Status(id).Phase; phase != RuntimeActive {
		t.Fatalf("phase=%s", phase)
	}
	unblock()
	rt.waker.workers.Wait()
	if len(rt.engine.WakeReservations()) != 1 {
		t.Fatal("reservation was duplicated or lost")
	}
}

func TestNativeAdmittedIdleCloseBlocksUnattemptedProbe(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int64
	manager, rt, id, m, clock := newIdleBoundaryFixture(t, nativeWakerConfig{Wait: func(context.Context, time.Duration) error { close(entered); <-release; return nil }, Run: func(context.Context, string, ...string) error { calls.Add(1); return nil }})
	defer unblock()
	rt.scheduleWake(m.ID)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not enter grace")
	}
	if rt.PendingWake() {
		t.Fatal("grace probe pinned runtime")
	}
	clock.Add(120)
	manager.reconcile()
	if phase := manager.Status(id).Phase; phase != RuntimeStopping {
		t.Fatalf("idle close not admitted: %s", phase)
	}
	unblock()
	select {
	case <-rt.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("idle close did not join probe")
	}
	if calls.Load() != 0 || len(rt.engine.WakeReservations()) != 0 {
		t.Fatal("closed admission consumed an attempt")
	}
	if err := rt.engine.ReserveWake(m.ID, model.ActorSlot2); err == nil {
		t.Fatal("draining engine accepted a fresh reservation")
	}
}

func TestNativeIdleCloseRechecksStaleManagerObservation(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	manager, rt, id, m, clock := newIdleBoundaryFixture(t, nativeWakerConfig{Wait: func(context.Context, time.Duration) error { return nil }, Run: func(ctx context.Context, _ string, _ ...string) error {
		close(started)
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	defer unblock()
	rt.armed.Store(true)
	clock.Add(120)
	decided := make(chan struct{})
	go func() { manager.reconcile(); close(decided) }()
	<-rt.sampled
	rt.scheduleWake(m.ID)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(rt.resume)
		t.Fatal("wake did not start")
	}
	close(rt.resume)
	<-decided
	if phase := manager.Status(id).Phase; phase != RuntimeActive {
		t.Fatalf("stale idle observation stopped admitted effect: %s", phase)
	}
	unblock()
	rt.waker.workers.Wait()
}

func TestNativeIdleCloseRechecksRecentHTTPUse(t *testing.T) {
	_, rt, _, _, clock := newIdleBoundaryFixture(t, nativeWakerConfig{})
	release := rt.acquire()
	if rt.TryBeginIdleClose(time.Unix(clock.Load()+120, 0)) {
		t.Fatal("idle close crossed HTTP request")
	}
	release()
	if rt.TryBeginIdleClose(time.Now().Add(-time.Minute)) {
		t.Fatal("idle close ignored recently completed HTTP request")
	}
}
