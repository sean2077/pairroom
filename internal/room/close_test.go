package room

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestCloseRetriesOnlyPendingAdaptersAndKeepsWriter(t *testing.T) {
	e, adapters := newTestEngine(t, "")
	cause := errors.New("process still owns pipes")
	first, second := 0, 0
	adapters[model.ActorSlot1].stopErr = cause
	t.Cleanup(func() {
		adapters[model.ActorSlot1].mu.Lock()
		adapters[model.ActorSlot1].stopErr = nil
		adapters[model.ActorSlot1].mu.Unlock()
		_ = e.Close()
	})
	adapters[model.ActorSlot1].onStop = func() { first++ }
	adapters[model.ActorSlot2].onStop = func() { second++ }
	if err := e.Close(); !errors.Is(err, ErrClosePending) || !errors.Is(err, cause) {
		t.Fatalf("first Close: %v", err)
	}
	// A surviving adapter must still be able to report durable facts while
	// new work is closed and its eventual termination is uncertain.
	e.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeError, Text: "late process evidence"})
	events, err := e.cfg.Store.Load()
	found := false
	for _, event := range events {
		if event.Kind == EventRuntime && event.Actor == model.ActorSlot1 && bytes.Contains(event.Data, []byte("late process evidence")) {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatalf("pending cleanup lost runtime evidence: %v", err)
	}
	if _, err := e.Send(context.Background(), SendRequest{Text: "new work", To: []model.ActorID{model.ActorSlot1}}); err == nil {
		t.Fatal("Close admitted new work")
	}
	adapters[model.ActorSlot1].mu.Lock()
	adapters[model.ActorSlot1].stopErr = nil
	adapters[model.ActorSlot1].mu.Unlock()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if first != 2 || second != 1 {
		t.Fatalf("Stop calls: %d/%d", first, second)
	}
	if _, err := e.record(EventSystemNotice, model.ActorSystem, model.SystemNotice{Text: "closed"}); err == nil {
		t.Fatal("writer remained open after cleanup completed")
	}
}

func TestClosePreservesTerminalSummaryError(t *testing.T) {
	e, _ := newTestEngine(t, "")
	started := time.Now().UTC()
	e.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeTurnStarted, TurnID: "summary", CreatedAt: started})
	e.HandleRuntimeEvent(model.RuntimeEvent{Agent: model.ActorSlot1, Kind: model.RuntimeToolStarted, TurnID: "summary", ItemID: "tool", CreatedAt: started.Add(time.Second)})
	if err := e.cfg.Store.Close(); err != nil {
		t.Fatal(err)
	}
	first := e.Close()
	if first == nil || errors.Is(first, ErrClosePending) {
		t.Fatalf("summary error classification: %v", first)
	}
	if second := e.Close(); second != first {
		t.Fatalf("terminal error changed on repeated Close: %v / %v", first, second)
	}
}
