package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
	"github.com/sean2077/pairroom/internal/store"
)

type cleanupAdapter struct {
	agent.Adapter
	calls    atomic.Int64
	failures int64
}

func (a *cleanupAdapter) Stop(ctx context.Context) error {
	if a.calls.Add(1) <= a.failures {
		return errors.New("process pipes remain open")
	}
	return a.Adapter.Stop(ctx)
}

func TestEmbeddedCleanupArchiveRetriesOnceAndCommits(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	adapters := make(map[model.ActorID]*cleanupAdapter)
	var runtime *embeddedRuntime
	factory := func(ctx context.Context, value Room) (RoomRuntime, error) {
		writer, err := store.OpenExistingForRoom(value.DataDir, value.ID)
		if err != nil {
			return nil, err
		}
		adapterFactory := func(cfg agent.Config, sink agent.EventSink) agent.Adapter {
			a := &cleanupAdapter{Adapter: agent.NewMock(cfg, sink)}
			if cfg.Actor == model.ActorSlot1 {
				a.failures = 1
			}
			adapters[cfg.Actor] = a
			return a
		}
		engine, err := room.New(room.Config{Store: writer, Slot1Factory: adapterFactory, Slot2Factory: adapterFactory})
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		if err := engine.Start(ctx); err != nil {
			_ = engine.Close()
			return nil, err
		}
		runtime = &embeddedRuntime{engine: engine}
		return runtime, nil
	}
	manager, err := NewRuntimeManager(registry, factory, RuntimeManagerConfig{Limit: 1, PollInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, rooms[0].ID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	release, err := manager.BeginLifecycleChange(ctx, rooms[0].ID, true)
	if err != nil {
		t.Fatalf("archive cleanup: %v", err)
	}
	defer release()
	if manager.ownsEventLog(rooms[0].ID) {
		t.Fatal("completed cleanup retained writer ownership")
	}
	archived, err := registry.ArchiveRoom(ctx, rooms[0].ID)
	if err != nil || !archived.Archived() {
		t.Fatalf("archive commit: %v", err)
	}
	if status := manager.Status(rooms[0].ID); status.Phase != RuntimeSuspended || status.OccupiesCapacity {
		t.Fatalf("after archive: %#v", status)
	}
	if adapters[model.ActorSlot1].calls.Load() != 2 || adapters[model.ActorSlot2].calls.Load() != 1 {
		t.Fatal("cleanup repeated successful adapter Stop")
	}
	if err := runtime.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatalf("recovered error leaked into shutdown: %v", err)
	}
}

type cleanupRuntime struct {
	*fakeRuntime
	failures int64
	entered  chan struct{}
	release  chan struct{}
}

func (r *cleanupRuntime) Close(ctx context.Context) error {
	n := r.closeCount.Add(1)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if r.failures < 0 || n <= r.failures {
		return errors.Join(ErrRuntimeCleanupPending, errors.New("process still alive"))
	}
	if r.entered != nil {
		close(r.entered)
		select {
		case <-r.release:
		case <-ctx.Done():
			return errors.Join(ErrRuntimeCleanupPending, ctx.Err())
		}
	}
	return nil
}

func TestRuntimeCleanupActivationAndPersistentFailure(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		failures int64
		fatal    bool
	}{{"recover", 1, false}, {"fatal-explicit", 1, true}, {"persistent", -1, false}} {
		t.Run(scenario.name, func(t *testing.T) {
			failures := scenario.failures
			registry, rooms := provisionRuntimeRooms(t, 1)
			rt := &cleanupRuntime{fakeRuntime: newFakeRuntime(rooms[0].ID, false, time.Time{}), failures: failures}
			var starts atomic.Int64
			manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
				if starts.Add(1) == 1 {
					return rt, nil
				}
				return newFakeRuntime(rooms[0].ID, false, time.Time{}), nil
			}, RuntimeManagerConfig{Limit: 1, PollInterval: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			activateRuntime(t, manager, rooms[0].ID)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := manager.Suspend(ctx, rooms[0].ID); !errors.Is(err, ErrRuntimeCloseUncertain) {
				t.Fatalf("first suspend: %v", err)
			}
			status := manager.Status(rooms[0].ID)
			if !status.CleanupRetryable || !status.OccupiesCapacity || !manager.ownsEventLog(rooms[0].ID) {
				t.Fatalf("ownership after failure: %#v", status)
			}
			if scenario.fatal {
				manager.mu.Lock()
				manager.entries[rooms[0].ID].fatalStop = true
				manager.mu.Unlock()
			}
			_, status, err = manager.Activate(ctx, rooms[0].ID)
			if failures == 1 {
				if err != nil || status.Phase != RuntimeActive || starts.Load() != 2 {
					t.Fatalf("activation recovery: %#v %v starts=%d", status, err, starts.Load())
				}
				if err := manager.Shutdown(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil || status.Phase != RuntimeFailed || !status.OccupiesCapacity || starts.Load() != 1 {
					t.Fatalf("persistent failure: %#v %v", status, err)
				}
				if err := manager.Shutdown(ctx); !errors.Is(err, ErrRuntimeCleanupPending) {
					t.Fatalf("shutdown lost unresolved error: %v", err)
				}
			}
		})
	}
}

func TestRuntimeCleanupBackgroundBackoffAndCapacity(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 2)
	base := time.Now().UTC()
	var now atomic.Int64
	now.Store(base.UnixNano())
	rt := &cleanupRuntime{fakeRuntime: newFakeRuntime(rooms[0].ID, false, base), failures: 2}
	var starts atomic.Int64
	manager, err := NewRuntimeManager(registry, func(_ context.Context, value Room) (RoomRuntime, error) {
		starts.Add(1)
		if value.ID == rooms[0].ID {
			return rt, nil
		}
		return newFakeRuntime(value.ID, false, base), nil
	}, RuntimeManagerConfig{Limit: 1, PollInterval: time.Hour, Now: func() time.Time { return time.Unix(0, now.Load()).UTC() }})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, rooms[0].ID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = manager.Suspend(ctx, rooms[0].ID)
	manager.mu.Lock()
	manager.entries[rooms[0].ID].fatalStop = true
	manager.mu.Unlock()
	if _, err := manager.RequestActivation(rooms[1].ID); err != nil {
		t.Fatal(err)
	}
	manager.reconcile()
	if rt.closeCount.Load() != 1 || starts.Load() != 1 {
		t.Fatal("cleanup skipped backoff or released capacity")
	}
	now.Store(base.Add(time.Second).UnixNano())
	manager.reconcile()
	waitRuntimeStatus(t, manager, rooms[0].ID, func(s RuntimeStatus) bool { return s.Phase == RuntimeFailed && rt.closeCount.Load() == 2 })
	now.Store(base.Add(2 * time.Second).UnixNano())
	manager.reconcile()
	if rt.closeCount.Load() != 2 {
		t.Fatal("second failure did not back off")
	}
	now.Store(base.Add(3 * time.Second).UnixNano())
	manager.reconcile()
	waitRuntimeStatus(t, manager, rooms[0].ID, func(s RuntimeStatus) bool { return s.Phase == RuntimeSuspended })
	waitRuntimeStatus(t, manager, rooms[1].ID, func(s RuntimeStatus) bool { return s.Phase == RuntimeActive })
	if starts.Load() != 2 || manager.ownsEventLog(rooms[0].ID) {
		t.Fatal("background cleanup restarted failed Room or retained writer")
	}
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeCleanupDeletionAndShutdownShareOneAttempt(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	rt := &cleanupRuntime{fakeRuntime: newFakeRuntime(rooms[0].ID, false, time.Time{}), failures: 1, entered: make(chan struct{}), release: make(chan struct{})}
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) { return rt, nil }, RuntimeManagerConfig{Limit: 1, PollInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, rooms[0].ID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = manager.Suspend(ctx, rooms[0].ID)
	deletion := make(chan error, 1)
	go func() { deletion <- manager.PrepareRoomDeletion(ctx, rooms[0].ID) }()
	select {
	case <-rt.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	manager.mu.Lock()
	generation := manager.entries[rooms[0].ID].generation
	manager.mu.Unlock()
	manager.finishStop(rooms[0].ID, generation-1, nil)
	if manager.Status(rooms[0].ID).Phase != RuntimeStopping || !manager.ownsEventLog(rooms[0].ID) {
		t.Fatal("stale cleanup result released ownership")
	}
	if _, err := manager.RequestActivation(rooms[0].ID); !errors.Is(err, ErrRuntimeRoomDeleting) {
		t.Fatalf("activation crossed deletion gate: %v", err)
	}
	shutdown := make(chan error, 1)
	go func() { shutdown <- manager.Shutdown(ctx) }()
	close(rt.release)
	if err := <-deletion; err != nil {
		t.Fatal(err)
	}
	if err := <-shutdown; err != nil {
		t.Fatal(err)
	}
	if rt.closeCount.Load() != 2 || manager.ownsEventLog(rooms[0].ID) {
		t.Fatal("concurrent cleanup duplicated or released ownership too early")
	}
	manager.AbortRoomDeletion(rooms[0].ID)
}

func TestFailedEmbeddedStartRetainsEngineForCleanup(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	base := time.Now().UTC()
	var now atomic.Int64
	now.Store(base.UnixNano())
	var retained RoomRuntime
	manager, err := NewRuntimeManager(registry, func(ctx context.Context, value Room) (RoomRuntime, error) {
		writer, err := store.OpenExistingForRoom(value.DataDir, value.ID)
		if err != nil {
			return nil, err
		}
		factory := func(cfg agent.Config, sink agent.EventSink) agent.Adapter {
			return &cleanupAdapter{Adapter: agent.NewMock(cfg, sink), failures: 1}
		}
		engine, err := room.New(room.Config{Store: writer, Slot1Factory: factory, Slot2Factory: factory})
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		if err := engine.Start(ctx); err != nil {
			return nil, err
		}
		rt, err := failedEmbeddedStart(value.ID, engine, nil, errors.New("start failed after adapters were created"))
		retained = rt
		return rt, err
	}, RuntimeManagerConfig{Limit: 1, PollInterval: time.Hour, Now: func() time.Time { return time.Unix(0, now.Load()) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, status, err := manager.Activate(ctx, rooms[0].ID); err == nil || !status.CleanupRetryable {
		t.Fatalf("failed start did not retain safe cleanup: %#v %v", status, err)
	}
	if _, ok := retained.(*embeddedRuntime); !ok {
		t.Fatalf("lost Engine in %T", retained)
	}
	manager.reconcile()
	_ = manager.Statuses()
	now.Store(base.Add(time.Second).UnixNano())
	manager.reconcile()
	waitRuntimeStatus(t, manager, rooms[0].ID, func(s RuntimeStatus) bool { return s.Phase == RuntimeSuspended })
	if manager.ownsEventLog(rooms[0].ID) {
		t.Fatal("factory cleanup did not release writer")
	}
	if _, err := registry.ArchiveRoom(ctx, rooms[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
