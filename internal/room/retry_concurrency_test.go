package room

import (
	"context"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestConcurrentRetryHasOnePendingChild(t *testing.T) {
	e, _ := newTestEngine(t, "")
	original := model.Message{ID: "failed-input", From: model.ActorUser, To: []model.ActorID{model.ActorCodex}, Text: "Do not execute twice", ThreadID: "thread",
		Delivery:   map[model.ActorID]model.DeliveryState{model.ActorCodex: model.DeliveryFailed},
		Processing: map[model.ActorID]model.ProcessingState{model.ActorCodex: model.ProcessingFailed}}
	if _, err := e.record(EventMessageCreated, model.ActorUser, original); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	gate := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			_, err := e.Retry(context.Background(), original.ID, RetryRequest{To: original.To})
			results <- err
		}()
	}
	close(gate)
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d concurrent retries for one pending execution; want 1", accepted)
	}
	children := 0
	for _, msg := range e.Snapshot().Messages {
		if msg.RetryOf == original.ID {
			children++
		}
	}
	if children != 1 {
		t.Fatalf("persisted %d retry children; want 1", children)
	}
}

func TestTerminalRetryDoesNotBlockExplicitRetry(t *testing.T) {
	for _, terminal := range []model.ProcessingState{model.ProcessingCompleted, model.ProcessingCancelled, model.ProcessingFailed} {
		t.Run(string(terminal), func(t *testing.T) {
			e, _ := newTestEngine(t, "")
			original := model.Message{ID: "failed-input", From: model.ActorUser, To: []model.ActorID{model.ActorCodex}, Text: "retry", ThreadID: "thread",
				Delivery: map[model.ActorID]model.DeliveryState{model.ActorCodex: model.DeliveryFailed}, Processing: map[model.ActorID]model.ProcessingState{model.ActorCodex: model.ProcessingFailed}}
			child := original
			child.ID = "old-retry"
			child.RetryOf = original.ID
			child.Processing = map[model.ActorID]model.ProcessingState{model.ActorCodex: terminal}
			for _, msg := range []model.Message{original, child} {
				if _, err := e.record(EventMessageCreated, model.ActorUser, msg); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.Retry(context.Background(), original.ID, RetryRequest{To: original.To}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
