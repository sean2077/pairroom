package room

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestCancelMessageReadsProcessingDuringRuntimeUpdates(t *testing.T) {
	engine, adapters := newTestEngine(t, "")
	message, err := engine.Send(context.Background(), SendRequest{
		Text: "working input", To: []model.ActorID{model.ActorSlot1},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = receiveInput(t, adapters[model.ActorSlot1])
	waitForProcessingState(t, engine, message.ID, model.ActorSlot1, model.ProcessingWorking)

	// Cancelled request contexts exercise the initial in-flight lookup without
	// interrupting the input. Repeated native processing receipts update the
	// same lifecycle map, so the race detector checks that its reads stay under
	// the projection lock. The start barrier avoids relying on sleep timing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			<-start
			for range 256 {
				if err := engine.CancelMessage(ctx, message.ID, model.ActorSlot1); !errors.Is(err, context.Canceled) {
					t.Errorf("cancelled request = %v, want context cancellation", err)
					return
				}
			}
		})
	}
	workers.Go(func() {
		<-start
		for range 256 {
			engine.HandleRuntimeEvent(model.RuntimeEvent{
				Agent: model.ActorSlot1, Kind: model.RuntimeInputProcessing,
				CorrelationID: message.ID, Text: "still working",
			})
		}
	})
	close(start)
	workers.Wait()

	adapter := adapters[model.ActorSlot1]
	adapter.mu.Lock()
	interrupts := adapter.interrupts
	adapter.mu.Unlock()
	if interrupts != 0 {
		t.Fatalf("cancelled requests interrupted the native input %d times", interrupts)
	}
	if got := findMessage(t, engine.Snapshot(), message.ID).Processing[model.ActorSlot1]; got != model.ProcessingWorking {
		t.Fatalf("processing = %q, want working", got)
	}
	if err := engine.CancelMessage(context.Background(), message.ID, model.ActorSlot1); err != nil {
		t.Fatal(err)
	}
	if got := findMessage(t, engine.Snapshot(), message.ID).Processing[model.ActorSlot1]; got != model.ProcessingCancelled {
		t.Fatalf("processing after cancellation = %q, want cancelled", got)
	}
}
