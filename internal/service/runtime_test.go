package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeRuntime struct {
	id             string
	busy           atomic.Bool
	activity       atomic.Int64
	closeCount     atomic.Int64
	draining       atomic.Bool
	drainOnCount   atomic.Int64
	drainOffCount  atomic.Int64
	interruptCount atomic.Int64
	// interruptSettles controls whether InterruptActive clears busy. Real
	// runtimes settle asynchronously; tests that need archive to keep waiting
	// for the Turn (for example shutdown deadline coverage) leave it false.
	interruptSettles bool
	closeOnce        sync.Once
	closed           chan struct{}
	closeErr         error
}

func newFakeRuntime(id string, busy bool, activity time.Time) *fakeRuntime {
	runtime := &fakeRuntime{id: id, closed: make(chan struct{}), interruptSettles: true}
	runtime.busy.Store(busy)
	if !activity.IsZero() {
		runtime.activity.Store(activity.UnixNano())
	}
	return runtime
}

func (r *fakeRuntime) URL() string { return "http://room.invalid/" + r.id }
func (r *fakeRuntime) Busy() bool  { return r.busy.Load() }
func (r *fakeRuntime) SetDraining(value bool) {
	r.draining.Store(value)
	if value {
		r.drainOnCount.Add(1)
	} else {
		r.drainOffCount.Add(1)
	}
}
func (r *fakeRuntime) LastActivity() time.Time {
	value := r.activity.Load()
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}
func (r *fakeRuntime) InterruptActive(context.Context) error {
	r.interruptCount.Add(1)
	if r.interruptSettles {
		r.busy.Store(false)
	}
	return nil
}
func (r *fakeRuntime) Close(context.Context) error {
	r.closeCount.Add(1)
	r.closeOnce.Do(func() { close(r.closed) })
	return r.closeErr
}

type fakeRuntimeFactory struct {
	mu                  sync.Mutex
	busy                bool
	activity            time.Time
	stayBusyOnInterrupt bool
	runtimes            map[string]*fakeRuntime
	startOrder          []string
}

func (f *fakeRuntimeFactory) open(_ context.Context, room Room) (RoomRuntime, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runtimes == nil {
		f.runtimes = make(map[string]*fakeRuntime)
	}
	runtime := newFakeRuntime(room.ID, f.busy, f.activity)
	runtime.interruptSettles = !f.stayBusyOnInterrupt
	f.runtimes[room.ID] = runtime
	f.startOrder = append(f.startOrder, room.ID)
	return runtime, nil
}

func (f *fakeRuntimeFactory) get(id string) *fakeRuntime {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runtimes[id]
}

func (f *fakeRuntimeFactory) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.startOrder...)
}

func provisionRuntimeRooms(t *testing.T, count int) (*Registry, []Room) {
	t.Helper()
	registry, project := testRegistry(t, testGitRepo(t))
	rooms := make([]Room, 0, count)
	for index := 0; index < count; index++ {
		room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
			ProjectID: project.ID,
			Name:      fmt.Sprintf("Runtime Room %d", index+1),
			Bindings:  specs(BindingNew, BindingNew, fmt.Sprint(index)),
		}, SyntheticProvisioner{})
		if err != nil {
			t.Fatal(err)
		}
		rooms = append(rooms, room)
	}
	return registry, rooms
}

func waitRuntimeStatus(t *testing.T, manager *RuntimeManager, roomID string, predicate func(RuntimeStatus) bool) RuntimeStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status := manager.Status(roomID)
		if predicate(status) {
			return status
		}
		time.Sleep(5 * time.Millisecond)
	}
	status := manager.Status(roomID)
	t.Fatalf("runtime %s did not reach expected state; last=%#v", roomID, status)
	return RuntimeStatus{}
}

func activateRuntime(t *testing.T, manager *RuntimeManager, roomID string) RoomRuntime {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runtime, _, err := manager.Activate(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func shutdownRuntimeManager(t *testing.T, manager *RuntimeManager, factory *fakeRuntimeFactory) {
	t.Helper()
	if factory != nil {
		factory.mu.Lock()
		for _, runtime := range factory.runtimes {
			runtime.busy.Store(false)
		}
		factory.mu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown runtime manager: %v", err)
	}
}

func TestRuntimeManagerActivatesPendingNewBindingsButBlocksLegacyPendingBindings(t *testing.T) {
	repo := testGitRepo(t)
	registry, project := testRegistry(t, repo)
	pendingNew, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Pending new runtime",
		Bindings:  specs(BindingNew, BindingNew, "pending-new"),
	}, deferredNewProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	factory := &fakeRuntimeFactory{}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)
	if runtime := activateRuntime(t, manager, pendingNew.ID); runtime == nil {
		t.Fatal("pending new Room did not activate")
	}
}

func TestRuntimeManagerQueuesFIFOAndNeverPreemptsBusyTurns(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 3)
	factory := &fakeRuntimeFactory{busy: true}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)

	activateRuntime(t, manager, rooms[0].ID)
	if _, err := manager.RequestActivation(rooms[1].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.RequestActivation(rooms[2].ID); err != nil {
		t.Fatal(err)
	}
	second := waitRuntimeStatus(t, manager, rooms[1].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeQueued })
	third := waitRuntimeStatus(t, manager, rooms[2].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeQueued })
	if second.QueuePosition != 1 || third.QueuePosition != 2 {
		t.Fatalf("queue positions second=%d third=%d", second.QueuePosition, third.QueuePosition)
	}
	time.Sleep(40 * time.Millisecond)
	firstRuntime := factory.get(rooms[0].ID)
	if firstRuntime.closeCount.Load() != 0 {
		t.Fatal("busy Room was preempted to free capacity")
	}

	firstRuntime.busy.Store(false)
	waitRuntimeStatus(t, manager, rooms[1].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeActive })
	if status := manager.Status(rooms[2].ID); status.Phase != RuntimeQueued || status.QueuePosition != 1 {
		t.Fatalf("third Room did not remain first in FIFO: %#v", status)
	}
	secondRuntime := factory.get(rooms[1].ID)
	secondRuntime.busy.Store(false)
	waitRuntimeStatus(t, manager, rooms[2].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeActive })
	order := factory.order()
	if len(order) < 3 || order[0] != rooms[0].ID || order[1] != rooms[1].ID || order[2] != rooms[2].ID {
		t.Fatalf("runtime start order=%v", order)
	}
}

func TestRuntimeManagerEvictsIdleLRUAtCapacity(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 3)
	base := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(base.UnixNano())
	factory := &fakeRuntimeFactory{busy: false}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 2, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
		Now: func() time.Time { return time.Unix(0, now.Load()).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)

	activateRuntime(t, manager, rooms[0].ID)
	now.Store(base.Add(time.Minute).UnixNano())
	activateRuntime(t, manager, rooms[1].ID)
	now.Store(base.Add(2 * time.Minute).UnixNano())
	if _, err := manager.RequestActivation(rooms[2].ID); err != nil {
		t.Fatal(err)
	}
	waitRuntimeStatus(t, manager, rooms[2].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeActive })
	waitRuntimeStatus(t, manager, rooms[0].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeSuspended })
	if factory.get(rooms[0].ID).closeCount.Load() != 1 {
		t.Fatal("least recently used Room was not closed")
	}
	if factory.get(rooms[1].ID).closeCount.Load() != 0 {
		t.Fatal("newer idle Room was incorrectly evicted")
	}
}

func TestCanceledActivationRequestLeavesVisibleQueueDemand(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 2)
	factory := &fakeRuntimeFactory{busy: true}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)
	activateRuntime(t, manager, rooms[0].ID)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, _, err := manager.Activate(ctx, rooms[1].ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Activate error=%v", err)
	}
	status := manager.Status(rooms[1].ID)
	if status.Phase != RuntimeQueued || status.QueuePosition != 1 {
		t.Fatalf("canceled browser request removed queue demand: %#v", status)
	}
	factory.get(rooms[0].ID).busy.Store(false)
	waitRuntimeStatus(t, manager, rooms[1].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeActive })
}

func TestRuntimeActivityRefreshPreventsPrematureIdleSuspend(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	base := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(base.UnixNano())
	factory := &fakeRuntimeFactory{busy: false, activity: base}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: 10 * time.Minute, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
		Now: func() time.Time { return time.Unix(0, now.Load()).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)
	activateRuntime(t, manager, rooms[0].ID)
	runtime := factory.get(rooms[0].ID)
	runtime.activity.Store(base.Add(10*time.Minute + 30*time.Second).UnixNano())
	now.Store(base.Add(11 * time.Minute).UnixNano())
	time.Sleep(30 * time.Millisecond)
	if status := manager.Status(rooms[0].ID); status.Phase != RuntimeActive {
		t.Fatalf("recent Room HTTP activity was ignored: %#v", status)
	}
	now.Store(base.Add(21 * time.Minute).UnixNano())
	waitRuntimeStatus(t, manager, rooms[0].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeSuspended })
}

func TestBusyRuntimeReceivesFullIdleWindowAfterTurnCompletes(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	base := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC)
	var now atomic.Int64
	now.Store(base.UnixNano())
	factory := &fakeRuntimeFactory{busy: true, activity: base}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: 10 * time.Minute, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
		Now: func() time.Time { return time.Unix(0, now.Load()).UTC() },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)
	activateRuntime(t, manager, rooms[0].ID)
	runtime := factory.get(rooms[0].ID)

	// Simulate a background turn that runs for thirty minutes without any
	// browser request. Observing the busy runtime must refresh lastUsed.
	now.Store(base.Add(30 * time.Minute).UnixNano())
	status := manager.Status(rooms[0].ID)
	if got, want := status.LastUsedAt, base.Add(30*time.Minute); !got.Equal(want) {
		t.Fatalf("busy Room last used=%s, want %s", got, want)
	}
	runtime.busy.Store(false)

	// Nine idle minutes is still inside the ten-minute window measured from
	// completion, even though the original activation is thirty-nine minutes old.
	now.Store(base.Add(39 * time.Minute).UnixNano())
	time.Sleep(30 * time.Millisecond)
	if status := manager.Status(rooms[0].ID); status.Phase != RuntimeActive {
		t.Fatalf("Room was suspended before its post-turn idle timeout: %#v", status)
	}

	now.Store(base.Add(41 * time.Minute).UnixNano())
	waitRuntimeStatus(t, manager, rooms[0].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeSuspended })
}

func TestRuntimeManagerShutdownReportsRuntimeCloseErrors(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	runtime := newFakeRuntime(rooms[0].ID, false, time.Time{})
	runtime.closeErr = errors.New("synthetic runtime close failure")
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		return runtime, nil
	}, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, rooms[0].ID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = manager.Shutdown(ctx)
	if err == nil || !strings.Contains(err.Error(), "synthetic runtime close failure") || !strings.Contains(err.Error(), rooms[0].ID) {
		t.Fatalf("shutdown error=%v", err)
	}
	if runtime.closeCount.Load() != 1 {
		t.Fatalf("runtime close count=%d", runtime.closeCount.Load())
	}
}

func TestWaitAndSuspendClosesAdmissionAndReopensAfterCancellation(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	factory := &fakeRuntimeFactory{busy: true}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)
	activateRuntime(t, manager, rooms[0].ID)
	runtime := factory.get(rooms[0].ID)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- manager.WaitAndSuspend(ctx, rooms[0].ID) }()
	waitUntil := time.Now().Add(time.Second)
	for !runtime.draining.Load() && time.Now().Before(waitUntil) {
		time.Sleep(time.Millisecond)
	}
	if !runtime.draining.Load() {
		t.Fatal("WaitAndSuspend did not close Room mutation admission while waiting")
	}
	if runtime.closeCount.Load() != 0 {
		t.Fatal("WaitAndSuspend attempted to close a busy runtime")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitAndSuspend error=%v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled WaitAndSuspend did not return")
	}
	waitUntil = time.Now().Add(time.Second)
	for runtime.draining.Load() && time.Now().Before(waitUntil) {
		time.Sleep(time.Millisecond)
	}
	if runtime.draining.Load() {
		t.Fatal("canceled archive left Room mutation admission closed")
	}
	if runtime.drainOnCount.Load() == 0 || runtime.drainOffCount.Load() == 0 {
		t.Fatalf("drain transitions on=%d off=%d", runtime.drainOnCount.Load(), runtime.drainOffCount.Load())
	}
	if status := manager.Status(rooms[0].ID); status.Phase != RuntimeActive {
		t.Fatalf("canceled archive changed runtime phase: %#v", status)
	}
}

func TestRuntimeManagerShutdownClosesAdmissionBeforeWaitingForActiveTurn(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	factory := &fakeRuntimeFactory{busy: true}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, rooms[0].ID)
	runtime := factory.get(rooms[0].ID)

	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result <- manager.Shutdown(ctx)
	}()
	waitUntil := time.Now().Add(time.Second)
	for !runtime.draining.Load() && time.Now().Before(waitUntil) {
		time.Sleep(time.Millisecond)
	}
	if !runtime.draining.Load() {
		t.Fatal("shutdown did not close Room mutation admission before waiting")
	}
	if runtime.closeCount.Load() != 0 {
		t.Fatal("shutdown attempted to close an active Turn")
	}
	runtime.busy.Store(false)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after active Turn settled")
	}
	if runtime.closeCount.Load() != 1 {
		t.Fatalf("runtime close count=%d", runtime.closeCount.Load())
	}
	if runtime.drainOffCount.Load() != 0 {
		t.Fatalf("service shutdown unexpectedly reopened admission %d times", runtime.drainOffCount.Load())
	}
}

func TestRuntimeManagerShutdownWaitsForActiveTurn(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	factory := &fakeRuntimeFactory{busy: true}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, rooms[0].ID)
	runtime := factory.get(rooms[0].ID)

	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result <- manager.Shutdown(ctx)
	}()
	select {
	case err := <-result:
		t.Fatalf("shutdown returned during active Turn: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if runtime.closeCount.Load() != 0 {
		t.Fatal("shutdown interrupted active Turn")
	}
	runtime.busy.Store(false)
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown did not finish after Turn became idle")
	}
	if runtime.closeCount.Load() != 1 {
		t.Fatalf("close count=%d", runtime.closeCount.Load())
	}
}

func TestRuntimeCloseFailureRetainsCapacityAndBlocksReactivation(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 2)
	factory := &fakeRuntimeFactory{busy: false}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, rooms[0].ID)
	first := factory.get(rooms[0].ID)
	closeFailure := errors.New("vendor process close state unknown")
	first.closeErr = closeFailure

	if _, err := manager.RequestActivation(rooms[1].ID); err != nil {
		t.Fatal(err)
	}
	failed := waitRuntimeStatus(t, manager, rooms[0].ID, func(status RuntimeStatus) bool {
		return status.Phase == RuntimeFailed
	})
	if !strings.Contains(failed.LastError, closeFailure.Error()) {
		t.Fatalf("failed status does not expose close diagnostic: %#v", failed)
	}
	time.Sleep(30 * time.Millisecond)
	if status := manager.Status(rooms[1].ID); status.Phase != RuntimeQueued || status.QueuePosition != 1 {
		t.Fatalf("queued Room escaped retained capacity: %#v", status)
	}
	if order := factory.order(); len(order) != 1 || order[0] != rooms[0].ID {
		t.Fatalf("factory started another runtime after uncertain close: %v", order)
	}
	if _, err := manager.RequestActivation(rooms[0].ID); !errors.Is(err, ErrRuntimeCloseUncertain) {
		t.Fatalf("reactivation error=%v, want ErrRuntimeCloseUncertain", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Suspend(ctx, rooms[0].ID); !errors.Is(err, ErrRuntimeCloseUncertain) {
		t.Fatalf("Suspend error=%v, want ErrRuntimeCloseUncertain", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	shutdownErr := manager.Shutdown(shutdownCtx)
	if !errors.Is(shutdownErr, closeFailure) || !strings.Contains(shutdownErr.Error(), rooms[0].ID) {
		t.Fatalf("Shutdown error=%v", shutdownErr)
	}
	if first.closeCount.Load() != 1 {
		t.Fatalf("uncertain runtime was retried or discarded; close count=%d", first.closeCount.Load())
	}
}

func TestRuntimeFactoryCleanupUncertaintyRetainsCapacity(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	uncertain := newFakeRuntime(rooms[0].ID, false, time.Time{})
	startFailure := errors.New("runtime start failed after partial initialization")
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		return uncertain, startFailure
	}, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, status, err := manager.Activate(ctx, rooms[0].ID); err == nil || !strings.Contains(err.Error(), startFailure.Error()) || status.Phase != RuntimeFailed {
		t.Fatalf("Activate status=%#v error=%v", status, err)
	}
	if _, err := manager.RequestActivation(rooms[0].ID); !errors.Is(err, ErrRuntimeCloseUncertain) {
		t.Fatalf("RequestActivation error=%v, want ErrRuntimeCloseUncertain", err)
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	shutdownErr := manager.Shutdown(shutdownCtx)
	if !errors.Is(shutdownErr, startFailure) || !strings.Contains(shutdownErr.Error(), rooms[0].ID) {
		t.Fatalf("Shutdown error=%v", shutdownErr)
	}
	if uncertain.closeCount.Load() != 0 {
		t.Fatalf("manager retried an explicitly uncertain factory cleanup: %d", uncertain.closeCount.Load())
	}
}

func TestRuntimeManagerPolicyAndCapacityObservability(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	factory := &fakeRuntimeFactory{}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 3, IdleTimeout: 27 * time.Minute, PollInterval: 125 * time.Millisecond, CloseTimeout: 7 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)

	policy := manager.Policy()
	if policy.Limit != 3 || policy.IdleTimeoutSeconds != int64((27*time.Minute)/time.Second) ||
		policy.PollIntervalMilliseconds != 125 || policy.CloseTimeoutSeconds != 7 {
		t.Fatalf("unexpected runtime policy: %#v", policy)
	}
	if status := manager.Status(rooms[0].ID); status.OccupiesCapacity {
		t.Fatalf("suspended runtime unexpectedly occupies capacity: %#v", status)
	}

	activateRuntime(t, manager, rooms[0].ID)
	status := manager.Status(rooms[0].ID)
	if status.Phase != RuntimeActive || !status.OccupiesCapacity {
		t.Fatalf("active runtime did not report capacity use: %#v", status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Suspend(ctx, rooms[0].ID); err != nil {
		t.Fatal(err)
	}
	status = manager.Status(rooms[0].ID)
	if status.Phase != RuntimeSuspended || status.OccupiesCapacity {
		t.Fatalf("suspended runtime retained capacity: %#v", status)
	}
}
