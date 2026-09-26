package agent

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type eventLog struct {
	mu      sync.Mutex
	events  []model.RuntimeEvent
	changed chan struct{}
}

// stopWithin bounds cleanup so a regression that strands the vendor process
// fails the test instead of hanging the package.
func stopWithin(adapter Adapter, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = adapter.Stop(ctx)
}

func newEventLog() *eventLog { return &eventLog{changed: make(chan struct{}, 1)} }

func (l *eventLog) add(event model.RuntimeEvent) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
	select {
	case l.changed <- struct{}{}:
	default:
	}
}

func (l *eventLog) snapshot() []model.RuntimeEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]model.RuntimeEvent(nil), l.events...)
}

func (l *eventLog) waitFor(t *testing.T, timeout time.Duration, match func(model.RuntimeEvent) bool) model.RuntimeEvent {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		for _, event := range l.snapshot() {
			if match(event) {
				return event
			}
		}
		select {
		case <-l.changed:
		case <-deadline.C:
			t.Fatalf("event did not arrive within %s; got %d events", timeout, len(l.snapshot()))
		}
	}
}

// assertOversizedLineFailsTurn checks the observable contract: the vendor is
// stopped, the accepted input fails with the size reason, and the Turn owner
// is released by a process_exited terminal instead of hanging.
func assertOversizedLineFailsTurn(t *testing.T, events *eventLog, adapter Adapter, limit string) {
	t.Helper()
	events.waitFor(t, 20*time.Second, func(e model.RuntimeEvent) bool {
		return e.Kind == model.RuntimeTurnCompleted && e.Name == "process_exited" && e.CorrelationID == "m1"
	})
	failed := events.waitFor(t, time.Second, func(e model.RuntimeEvent) bool {
		return e.Kind == model.RuntimeInputFailed && e.CorrelationID == "m1"
	})
	if !strings.Contains(failed.Text, limit) {
		t.Fatalf("input failure does not name the stdout limit: %q", failed.Text)
	}
	events.waitFor(t, time.Second, func(e model.RuntimeEvent) bool {
		return e.Kind == model.RuntimeError && e.Name == "adapter.process_exited" && strings.Contains(e.Text, limit)
	})
	if state := adapter.State(); state != model.StateError {
		t.Fatalf("adapter state after stream failure = %s", state)
	}
}

func TestCodexOversizedStdoutRecordStopsAppServerAndFailsTurn(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_HELPER_MODE", "oversized")
	events := newEventLog()
	adapter := NewCodex(Config{Command: os.Args[0], Repo: t.TempDir()}, events.add)
	t.Cleanup(func() { stopWithin(adapter, 10*time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "m1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	assertOversizedLineFailsTurn(t, events, adapter, "16 MiB")
}

func TestClaudeOversizedStdoutRecordStopsProcessAndFailsTurn(t *testing.T) {
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "oversized")
	events := newEventLog()
	adapter := NewClaude(Config{Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir()}, events.add)
	t.Cleanup(func() { stopWithin(adapter, 10*time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "m1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	assertOversizedLineFailsTurn(t, events, adapter, "8 MiB")
}

func TestGrokOversizedStdoutRecordStopsProcessAndFailsTurn(t *testing.T) {
	t.Setenv("PAIRROOM_GROK_HELPER", "1")
	t.Setenv("PAIRROOM_GROK_HELPER_MODE", "oversized")
	events := newEventLog()
	adapter := NewGrok(Config{Actor: model.ActorSlot1, Command: os.Args[0], Repo: t.TempDir()}, events.add)
	t.Cleanup(func() { stopWithin(adapter, 10*time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "m1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	assertOversizedLineFailsTurn(t, events, adapter, "16 MiB")
}
