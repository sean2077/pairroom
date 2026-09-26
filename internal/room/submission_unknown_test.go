package room

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
)

// startUnknownSubmission sends one message to slot1, whose adapter reports an
// unknown submission outcome, and queues a cross-Agent message behind it.
func startUnknownSubmission(t *testing.T, configure func(*Engine)) (*Engine, map[model.ActorID]*fakeAdapter, model.Message, model.Message) {
	t.Helper()
	engine, adapters := newTestEngine(t, "")
	if configure != nil {
		configure(engine)
	}
	adapters[model.ActorSlot1].submitErr = fmt.Errorf("%w: turn/start was not answered", agent.ErrSubmissionUnknown)
	first, err := engine.Send(context.Background(), SendRequest{Text: "first", To: []model.ActorID{model.ActorSlot1}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.Send(context.Background(), SendRequest{Text: "second", To: []model.ActorID{model.ActorSlot2}})
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the first delivery goroutine has finished its submission.
	deadline := time.Now().Add(2 * time.Second)
	for {
		engine.turnMu.Lock()
		submitting := engine.turnSubmitting
		engine.turnMu.Unlock()
		message := findMessage(t, engine.Snapshot(), first.ID)
		if submitting == 0 && (strings.Contains(message.ProcessingDetail[model.ActorSlot1], "unknown") || message.Processing[model.ActorSlot1].Terminal()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("submission did not finish: %#v", message)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return engine, adapters, first, second
}

func TestUnknownStartSubmissionKeepsTurnOwnedUntilRuntimeEvidence(t *testing.T) {
	materialized := make(chan string, 2)
	engine, adapters, first, second := startUnknownSubmission(t, func(engine *Engine) {
		engine.cfg.OnSessionMaterialized = func(_ context.Context, actor model.ActorID, sessionID string) error {
			materialized <- string(actor) + "/" + sessionID
			return nil
		}
	})
	message := findMessage(t, engine.Snapshot(), first.ID)
	if message.Delivery[model.ActorSlot1] != model.DeliverySubmitting || message.Processing[model.ActorSlot1] != model.ProcessingWaiting {
		t.Fatalf("unknown submission = delivery %s processing %s", message.Delivery[model.ActorSlot1], message.Processing[model.ActorSlot1])
	}
	engine.turnMu.Lock()
	owner := engine.turnOwner
	engine.turnMu.Unlock()
	if owner != model.ActorSlot1 {
		t.Fatalf("Turn owner = %q after an unknown submission", owner)
	}
	select {
	case input := <-adapters[model.ActorSlot2].submissions:
		t.Fatalf("queued work %s was admitted without terminal evidence", input.MessageID)
	default:
	}
	if _, err := engine.Retry(context.Background(), first.ID, RetryRequest{}); err == nil {
		t.Fatal("an unknown submission was offered for automatic retry")
	}

	// The late native acceptance and its completion settle the input.
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "turn-late", CorrelationID: first.ID})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeInputProcessing, TurnID: "turn-late", CorrelationID: first.ID, Name: string(model.ProcessingWorking)})
	waitForDeliveryState(t, engine, first.ID, model.ActorSlot1, model.DeliveryStarted)
	// A new Binding materializes on the accepted native input.
	select {
	case got := <-materialized:
		if got != "slot1/fake-slot1" {
			t.Fatalf("materialized %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late acceptance did not materialize the Binding")
	}
	select {
	case input := <-adapters[model.ActorSlot2].submissions:
		t.Fatalf("queued work %s overtook the accepted Turn", input.MessageID)
	default:
	}
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeInputCompleted, TurnID: "turn-late", CorrelationID: first.ID})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnCompleted, TurnID: "turn-late", CorrelationID: first.ID, Name: "completed"})
	if input := receiveInput(t, adapters[model.ActorSlot2]); input.MessageID != second.ID {
		t.Fatalf("next FIFO item = %s", input.MessageID)
	}
	waitForProcessingState(t, engine, first.ID, model.ActorSlot1, model.ProcessingCompleted)
}

func TestUnknownStartSubmissionRejectedByRuntimeBecomesRetryable(t *testing.T) {
	engine, adapters, first, second := startUnknownSubmission(t, nil)
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeInputFailed, CorrelationID: first.ID, Text: "Codex rejected turn/start"})
	engine.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnCompleted, CorrelationID: first.ID, Name: "failed"})
	if input := receiveInput(t, adapters[model.ActorSlot2]); input.MessageID != second.ID {
		t.Fatalf("next FIFO item = %s", input.MessageID)
	}
	waitForDeliveryState(t, engine, first.ID, model.ActorSlot1, model.DeliveryFailed)
	waitForProcessingState(t, engine, first.ID, model.ActorSlot1, model.ProcessingFailed)
}
