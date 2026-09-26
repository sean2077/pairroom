package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lifecycleHookRegistry provisions one Room whose Registry clock runs hook
// exactly once when armed. Archive and rename read that clock after the
// runtime has been suspended and before the lifecycle event is appended, which
// is the window a concurrent native relay request could previously reopen.
func lifecycleHookRegistry(t *testing.T) (*Registry, Room, *atomic.Pointer[func()]) {
	t.Helper()
	var hook atomic.Pointer[func()]
	var once sync.Once
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir(), Now: func() time.Time {
		if fn := hook.Load(); fn != nil {
			once.Do(*fn)
		}
		return time.Now().UTC()
	}})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(context.Background(), testGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Lifecycle Room", Bindings: specs(BindingNew, BindingNew, "lifecycle"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	return registry, room, &hook
}

func TestLifecycleAppendRejectsActivationBetweenSuspendAndCommit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "archive", method: http.MethodPost, path: "/archive"},
		{name: "rename", method: http.MethodPatch, path: "", body: `{"name":"Renamed Lifecycle Room"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry, room, hook := lifecycleHookRegistry(t)
			factory := &fakeRuntimeFactory{}
			manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
				Limit: 2, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer shutdownRuntimeManager(t, manager, factory)
			server, err := NewManagementServer(ManagementServerConfig{
				Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, Token: "management-secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			first := activateRuntime(t, manager, room.ID)
			before, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}

			// Run a relay-style activation inside the window. It must be refused
			// outright rather than reopening the Room's Event Log writer.
			var racedErr error
			var racedRuntime RoomRuntime
			fn := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				racedRuntime, _, racedErr = manager.Activate(ctx, room.ID)
			}
			hook.Store(&fn)

			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, managementRequest(tc.method, "/api/v1/rooms/"+room.ID+tc.path, tc.body, true))
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s status=%d body=%s", tc.name, recorder.Code, recorder.Body.String())
			}
			if racedRuntime != nil || !errors.Is(racedErr, ErrRuntimeLifecycleInProgress) {
				t.Fatalf("activation inside the lifecycle window runtime=%v err=%v, want ErrRuntimeLifecycleInProgress", racedRuntime, racedErr)
			}
			if status := manager.Status(room.ID); status.Phase != RuntimeSuspended {
				t.Fatalf("runtime after %s = %#v, want suspended", tc.name, status)
			}
			if got := factory.order(); len(got) != 1 {
				t.Fatalf("runtime starts=%v, want only the original runtime", got)
			}
			if first.(*fakeRuntime).closeCount.Load() != 1 {
				t.Fatalf("original runtime close count=%d", first.(*fakeRuntime).closeCount.Load())
			}
			after, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before)+1 {
				t.Fatalf("events before=%d after=%d, want exactly one lifecycle event", len(before), len(after))
			}

			if tc.name == "rename" {
				// The barrier is released once the lifecycle change commits.
				activateRuntime(t, manager, room.ID)
			}
		})
	}
}

// A lifecycle append opens its own Event Log writer. It must refuse while a
// runtime still owns that log instead of racing it for the next sequence.
func TestRegistryLifecycleAppendRefusesWhileRuntimeOwnsEventLog(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	factory := &fakeRuntimeFactory{}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)
	activateRuntime(t, manager, rooms[0].ID)
	before, err := readEventsReadOnly(filepath.Join(rooms[0].DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ArchiveRoom(context.Background(), rooms[0].ID); !errors.Is(err, ErrRoomLogOwnedByRuntime) {
		t.Fatalf("archive with an active runtime err=%v, want ErrRoomLogOwnedByRuntime", err)
	}
	if _, err := registry.RenameRoom(context.Background(), rooms[0].ID, "Other name"); !errors.Is(err, ErrRoomLogOwnedByRuntime) {
		t.Fatalf("rename with an active runtime err=%v, want ErrRoomLogOwnedByRuntime", err)
	}
	after, err := readEventsReadOnly(filepath.Join(rooms[0].DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("refused lifecycle append still wrote events: before=%d after=%d", len(before), len(after))
	}
	if room, _ := registry.Room(rooms[0].ID); room.Archived() || room.Name != rooms[0].Name {
		t.Fatalf("refused lifecycle append changed the projection: %#v", room)
	}
}
