package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestMockReservesOwnershipBeforeWorkerStarts(t *testing.T) {
	adapter := NewMock(Config{Actor: model.ActorClaude}, func(model.RuntimeEvent) {})
	// Model an idle worker without starting a goroutine, so the dequeue-to-start
	// interval is deterministic rather than dependent on scheduler timing.
	adapter.state = model.StateIdle
	ctx := context.Background()
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "first"}); err != nil {
		t.Fatal(err)
	}
	if input := <-adapter.queue; input.MessageID != "first" {
		t.Fatalf("unexpected admitted input: %#v", input)
	}
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "second"}); err == nil {
		t.Fatal("dequeue released ownership before the worker started the first turn")
	}
	if adapter.State() != model.StateWorking || len(adapter.queue) != 0 {
		t.Fatal("rejected admission changed the active ownership or queued work")
	}
}

func TestMockCancelledAdmissionReleasesOwnership(t *testing.T) {
	adapter := NewMock(Config{Actor: model.ActorClaude}, func(model.RuntimeEvent) {})
	adapter.state = model.StateIdle
	adapter.queue = make(chan model.AgentInput) // No receiver: admission must cancel.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "cancelled"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled admission returned %v", err)
	}
	if adapter.State() != model.StateIdle {
		t.Fatal("cancelled admission retained ownership")
	}
}
