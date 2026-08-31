package service

import (
	"context"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
)

func TestEmbeddedRuntimeArchiveInterruptsActiveTurnAndClosesRuntime(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	durable, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Active archive Room",
		Bindings:  specs(BindingNew, BindingNew, "active-archive"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}

	factory := EmbeddedRuntimeFactory(registry, EmbeddedRuntimeConfig{
		Mock:        true,
		RoutingMode: model.RoutingManual,
		Claude:      agent.Config{MockDelay: 5 * time.Second},
		Codex:       agent.Config{MockDelay: 5 * time.Second},
	})
	manager, err := NewRuntimeManager(registry, factory, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Errorf("shutdown Runtime Manager: %v", err)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	value, _, err := manager.Activate(ctx, durable.ID)
	if err != nil {
		t.Fatal(err)
	}
	runtime, ok := value.(*embeddedRuntime)
	if !ok {
		t.Fatalf("Runtime type=%T, want *embeddedRuntime", value)
	}
	if _, err := runtime.engine.Send(ctx, room.SendRequest{
		Text: "keep this turn active until archive interrupts it",
		To:   []model.ActorID{model.ActorClaude},
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for !runtime.Busy() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !runtime.Busy() {
		t.Fatal("mock turn never entered the active Runtime boundary")
	}

	archiveCtx, archiveCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer archiveCancel()
	if err := manager.InterruptAndSuspend(archiveCtx, durable.ID); err != nil {
		t.Fatalf("archive active embedded Runtime: %v", err)
	}
	if status := manager.Status(durable.ID); status.Phase != RuntimeSuspended || status.OccupiesCapacity {
		t.Fatalf("Runtime status after archive stop=%#v", status)
	}
	if runtime.Busy() {
		t.Fatal("embedded Runtime remained busy after archive stop")
	}
	runtime.closeMu.Lock()
	closed := runtime.closed
	runtime.closeMu.Unlock()
	if !closed {
		t.Fatal("archive returned before the embedded Runtime closed")
	}
	snapshot := runtime.engine.Snapshot()
	claude := snapshot.Participants[model.ActorClaude]
	if claude.CurrentTurn != "" || claude.State == model.StateStarting || claude.State == model.StateWorking || claude.State == model.StateWaiting {
		t.Fatalf("active Claude turn did not settle before archive: %#v", claude)
	}
}
