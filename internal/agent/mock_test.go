package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestMockStopCancelsActiveAndRejectsAdapterQueue(t *testing.T) {
	var mu sync.Mutex
	var events []model.RuntimeEvent
	started := make(chan struct{}, 1)
	adapter := NewMock(Config{Actor: model.ActorClaude, MockDelay: 2 * time.Second}, func(event model.RuntimeEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
		if event.Kind == model.RuntimeTurnStarted {
			select {
			case started <- struct{}{}:
			default:
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "msg-active"}); err != nil {
		t.Fatal(err)
	}
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "msg-queued"}); err == nil {
		t.Fatal("mock adapter accepted a second queued turn")
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("mock turn did not start")
	}
	if err := adapter.Stop(ctx); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	cancelled := map[string]bool{}
	for _, event := range events {
		if event.Kind == model.RuntimeInputCancelled {
			cancelled[event.CorrelationID] = true
		}
	}
	if !cancelled["msg-active"] || cancelled["msg-queued"] {
		t.Fatalf("stop did not settle only the active input: %#v", events)
	}
}

func TestMockPreservesConfiguredSessionID(t *testing.T) {
	const sessionID = "durable-room-session"
	adapter := NewMock(Config{Actor: model.ActorClaude, SessionID: sessionID}, func(model.RuntimeEvent) {})
	if got := adapter.SessionID(); got != sessionID {
		t.Fatalf("SessionID()=%q, want %q", got, sessionID)
	}
}
