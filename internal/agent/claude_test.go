package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestClaudeSuccessfulResultSettlesHeadAndStartsQueuedInput(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.state = model.StateWorking
	adapter.pending = []claudePending{
		{input: model.AgentInput{MessageID: "msg-first"}, turnID: "turn-first"},
		{input: model.AgentInput{MessageID: "msg-second"}, turnID: "turn-second"},
	}
	adapter.output.WriteString("streamed fallback")

	adapter.handleLine([]byte(`{"type":"result","subtype":"success","result":"final answer","session_id":"session-new","duration_ms":12,"usage":{"input_tokens":3}}`))

	var final *model.RuntimeEvent
	completed := false
	startedNext := false
	processingNext := false
	for i := range events {
		event := &events[i]
		switch {
		case event.Kind == model.RuntimeFinal:
			final = event
		case event.Kind == model.RuntimeInputCompleted && event.CorrelationID == "msg-first":
			completed = true
		case event.Kind == model.RuntimeTurnStarted && event.CorrelationID == "msg-second":
			startedNext = true
		case event.Kind == model.RuntimeInputProcessing && event.CorrelationID == "msg-second":
			processingNext = true
		}
	}
	if final == nil || final.Text != "final answer" || final.CorrelationID != "msg-first" || final.TurnID != "turn-first" {
		t.Fatalf("unexpected final event: %#v", final)
	}
	if !completed || !startedNext || !processingNext {
		t.Fatalf("queue lifecycle was incomplete: %#v", events)
	}
	if adapter.SessionID() != "session-new" || adapter.State() != model.StateWorking || len(adapter.pending) != 1 {
		t.Fatalf("unexpected adapter state after result: session=%q state=%q pending=%#v", adapter.SessionID(), adapter.State(), adapter.pending)
	}
}

func TestClaudeInterruptedResultIsCancelledWithoutFinalMessage(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.state = model.StateWorking
	adapter.pending = []claudePending{{input: model.AgentInput{MessageID: "msg-1"}, turnID: "turn-1"}}

	adapter.handleLine([]byte(`{"type":"result","subtype":"interrupted","is_error":true,"error":"user interrupted"}`))

	cancelled := false
	for _, event := range events {
		if event.Kind == model.RuntimeFinal {
			t.Fatalf("interrupted result must not publish a final room message: %#v", event)
		}
		if event.Kind == model.RuntimeInputCancelled && event.CorrelationID == "msg-1" && event.Text == "user interrupted" {
			cancelled = true
		}
	}
	if !cancelled {
		t.Fatalf("interrupted input was not settled as cancelled: %#v", events)
	}
	if adapter.State() != model.StateError {
		t.Fatalf("adapter should surface the interrupted process state, got %q", adapter.State())
	}
}

func TestClaudeInitUpdatesSessionAndRuntimeInfo(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{Model: "configured"}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	adapter.runtimeInfo = model.RuntimeInfo{
		Available: true, Protocol: "claude-stream-json", Capabilities: []string{"stream-json"},
	}

	adapter.handleLine([]byte(`{"type":"system","subtype":"init","session_id":"session-real","model":"claude-opus","version":"2.1.231","permissionMode":"auto","capabilities":{"hooks":true,"unused":false}}`))

	if adapter.SessionID() != "session-real" {
		t.Fatalf("session was not updated: %q", adapter.SessionID())
	}
	var sessionSeen, infoSeen bool
	for _, event := range events {
		if event.Kind == model.RuntimeSession && event.SessionID == "session-real" {
			sessionSeen = true
		}
		if event.Kind == model.RuntimeInfoUpdated && event.Runtime != nil {
			infoSeen = true
			if event.Runtime.Model != "claude-opus" || event.Runtime.Version != "2.1.231" || event.Runtime.PermissionMode != "auto" {
				t.Fatalf("unexpected runtime info: %#v", event.Runtime)
			}
			encoded, _ := json.Marshal(event.Runtime.Capabilities)
			if string(encoded) != `["hooks","stream-json"]` {
				t.Fatalf("unexpected capabilities: %s", encoded)
			}
		}
	}
	if !sessionSeen || !infoSeen {
		t.Fatalf("missing init projections: %#v", events)
	}
}

type testWriteCloser struct{ bytes.Buffer }

func (w *testWriteCloser) Close() error { return nil }

func TestClaudeSubmitQueuesInputExactlyOnce(t *testing.T) {
	var events []model.RuntimeEvent
	adapter := NewClaude(Config{}, func(event model.RuntimeEvent) {
		events = append(events, event)
	})
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	writer := &testWriteCloser{}
	adapter.cmd = &exec.Cmd{Process: process}
	adapter.stdin = writer
	adapter.state = model.StateIdle
	adapter.protocolSent = true

	state, err := adapter.Submit(context.Background(), model.AgentInput{MessageID: "msg-once", ThreadID: "thread-1"})
	if err != nil {
		t.Fatal(err)
	}
	if state != model.DeliveryStarted {
		t.Fatalf("unexpected delivery state: %q", state)
	}
	if len(adapter.pending) != 1 || adapter.pending[0].input.MessageID != "msg-once" {
		t.Fatalf("input was not queued exactly once: %#v", adapter.pending)
	}
	if got := bytes.Count(writer.Bytes(), []byte{'\n'}); got != 1 {
		t.Fatalf("expected one stream-json input line, got %d: %q", got, writer.String())
	}
}
