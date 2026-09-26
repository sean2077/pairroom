package agent

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestClaudeExactBindingRejectsReportedSessionChange(t *testing.T) {
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "session-mismatch")
	events := newEventLog()
	adapter := NewClaude(Config{
		Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(),
		SessionID: "bound-session", RequireExactSession: true,
	}, events.add)
	t.Cleanup(func() { stopWithin(adapter, 10*time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "m1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	events.waitFor(t, 20*time.Second, func(e model.RuntimeEvent) bool {
		return e.Kind == model.RuntimeTurnCompleted && e.CorrelationID == "m1"
	})
	if got := adapter.SessionID(); got != "bound-session" {
		t.Fatalf("exact binding was retargeted to %q", got)
	}
	failed := events.waitFor(t, time.Second, func(e model.RuntimeEvent) bool {
		return e.Kind == model.RuntimeInputFailed && e.CorrelationID == "m1"
	})
	if !strings.Contains(failed.Text, "other-session") || !strings.Contains(failed.Text, "bound-session") {
		t.Fatalf("input failure does not explain the session mismatch: %q", failed.Text)
	}
	events.waitFor(t, time.Second, func(e model.RuntimeEvent) bool {
		return e.Kind == model.RuntimeError && e.Name == "adapter.session_mismatch"
	})
	for _, event := range events.snapshot() {
		if event.Kind == model.RuntimeFinal || (event.Kind == model.RuntimeInputCompleted && event.CorrelationID == "m1") {
			t.Fatalf("output from the wrong session settled the Turn: %#v", event)
		}
		if event.Kind == model.RuntimeSession && event.SessionID == "other-session" {
			t.Fatalf("wrong session was published: %#v", event)
		}
	}
}

func TestClaudeUnboundAdapterStillAdoptsReportedSession(t *testing.T) {
	adapter := NewClaude(Config{}, func(model.RuntimeEvent) {})
	adapter.pending = []claudePending{{input: model.AgentInput{MessageID: "m1"}, turnID: "t1"}}
	adapter.handleLine([]byte(`{"type":"system","subtype":"init","session_id":"native-new"}`))
	if got := adapter.SessionID(); got != "native-new" {
		t.Fatalf("unbound adapter did not adopt the reported session: %q", got)
	}
}
