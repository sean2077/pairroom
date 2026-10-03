package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestCodexExitBeforePromptAckRetainsExactBinding(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_HELPER_MODE", "exit-before-turn-ack")
	requests := filepath.Join(t.TempDir(), "requests.jsonl")
	t.Setenv("PAIRROOM_HELPER_REQUESTS_FILE", requests)
	adapter := NewCodex(Config{Command: os.Args[0], Repo: t.TempDir(), RequireExactSession: true}, func(model.RuntimeEvent) {})
	t.Cleanup(func() { stopWithin(adapter, 5*time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	adapter.stdin = exitBeforeWriteReturns{adapter.stdin, adapter.procDone}
	adapter.mu.Unlock()
	err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "consumed-before-ack", Text: "review"})
	if !errors.Is(err, ErrSubmissionUnknown) {
		t.Fatalf("submission: %v", err)
	}
	raw, err := os.ReadFile(requests)
	if err != nil || !strings.Contains(string(raw), "consumed-before-ack") {
		t.Fatalf("child did not record the actual prompt: %s (%v)", raw, err)
	}
	if got := adapter.SessionID(); got != "thread-new" {
		t.Fatalf("possibly engaged exact thread lost on exit: %q", got)
	}
	if err := os.WriteFile(requests, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAIRROOM_CODEX_HELPER_MODE", "resume-error")
	if err := adapter.Start(ctx); err == nil || !strings.Contains(err.Error(), "resume required Codex thread") {
		t.Fatalf("missing fail-closed resume error: %v", err)
	}
	raw, err = os.ReadFile(requests)
	if err != nil || !strings.Contains(string(raw), "thread/resume") || strings.Contains(string(raw), "thread/start") {
		t.Fatalf("recovery replaced the possibly engaged thread: %s (%v)", raw, err)
	}
	if got := adapter.SessionID(); got != "thread-new" {
		t.Fatalf("failed resume discarded the preserved thread: %q", got)
	}
}

func TestNativePromptCancellationStopsBlockedProcess(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_HELPER_MODE", "stdin-block")
	events := newEventLog()
	adapter := NewCodex(Config{Command: os.Args[0], Repo: t.TempDir()}, events.add)
	t.Cleanup(func() { stopWithin(adapter, 5*time.Second) })
	startup, stopStartup := context.WithTimeout(context.Background(), 15*time.Second)
	defer stopStartup()
	if err := adapter.Start(startup); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	adapter.mu.Lock()
	done := adapter.procDone
	adapter.stdin = observedPipeWriter{adapter.stdin, started}
	adapter.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- adapter.StartTurn(ctx, model.AgentInput{MessageID: "blocked-process", Text: strings.Repeat("x", 4<<20)})
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("native prompt write did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, ErrSubmissionUnknown) {
			t.Fatalf("uncertain cancelled prompt: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release the native caller")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked child was not terminated")
	}
	terminalSeen := false
	for _, event := range events.snapshot() {
		if event.Kind == model.RuntimeTurnCompleted && event.CorrelationID == "blocked-process" {
			terminalSeen = event.Name == "process_exited"
		}
	}
	if !terminalSeen || adapter.SessionID() != "thread-new" {
		t.Fatal("real exit did not retain and settle the possibly engaged thread")
	}
}

func TestCodexUnsentPromptDoesNotEngageEphemeralThread(t *testing.T) {
	for _, mode := range []string{"cancelled", "missing-stdin"} {
		t.Run(mode, func(t *testing.T) {
			adapter := NewCodex(Config{}, func(model.RuntimeEvent) {})
			adapter.cmd = &exec.Cmd{Process: &os.Process{}}
			adapter.threadID, adapter.state = "ephemeral", model.StateIdle
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				adapter.stdin = &testWriteCloser{}
				cancel()
			}
			if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "unsent", Text: "review"}); err == nil {
				t.Fatal("unsent prompt succeeded")
			}
			adapter.handleUnexpectedProcessExit(nil)
			if got := adapter.SessionID(); got != "" {
				t.Fatalf("unsent prompt retained ephemeral thread: %q", got)
			}
		})
	}
}

func TestCodexRejectedPromptDoesNotEngageEphemeralThread(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_HELPER_MODE", "reject-before-exit")
	adapter := NewCodex(Config{Command: os.Args[0], Repo: t.TempDir()}, func(model.RuntimeEvent) {})
	t.Cleanup(func() { stopWithin(adapter, 5*time.Second) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	adapter.stdin = exitBeforeWriteReturns{adapter.stdin, adapter.procDone}
	adapter.mu.Unlock()
	err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "rejected", Text: "review"})
	var rejection codexRPCError
	if !errors.As(err, &rejection) || rejection.Code != -32000 {
		t.Fatalf("native rejection was lost: %v", err)
	}
	if got := adapter.SessionID(); got != "" {
		t.Fatalf("rejected prompt retained an ephemeral thread: %q", got)
	}
}

func TestCodexMalformedPromptAckSettlesOnlyOnExit(t *testing.T) {
	for _, mode := range []string{"malformed-turn-ack", "invalid-turn-id"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("PAIRROOM_CODEX_HELPER", "1")
			t.Setenv("PAIRROOM_CODEX_HELPER_MODE", mode)
			requests := filepath.Join(t.TempDir(), "requests.jsonl")
			t.Setenv("PAIRROOM_HELPER_REQUESTS_FILE", requests)
			events := newEventLog()
			adapter := NewCodex(Config{Command: os.Args[0], Repo: t.TempDir()}, events.add)
			t.Cleanup(func() { stopWithin(adapter, 5*time.Second) })
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := adapter.Start(ctx); err != nil {
				t.Fatal(err)
			}
			adapter.mu.Lock()
			done := adapter.procDone
			adapter.mu.Unlock()
			err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "possibly-accepted", Text: "review"})
			if !errors.Is(err, ErrSubmissionUnknown) || !errors.Is(err, errCodexMalformedTurnStart) {
				t.Fatalf("malformed native acknowledgement treated as definite failure: %v", err)
			}
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("damaged transport did not exit")
			}
			if got := adapter.SessionID(); got != "thread-new" {
				t.Fatalf("possibly accepted thread was discarded: %q", got)
			}
			raw, err := os.ReadFile(requests)
			if err != nil || strings.Count(string(raw), "possibly-accepted") != 1 {
				t.Fatalf("prompt was lost or replayed: %s (%v)", raw, err)
			}
			terminals := 0
			for _, event := range events.snapshot() {
				if event.Kind == model.RuntimeTurnCompleted {
					terminals++
					if event.Name != "process_exited" || event.CorrelationID != "possibly-accepted" {
						t.Fatalf("submission settled without exit evidence: %+v", event)
					}
				}
			}
			if terminals != 1 {
				t.Fatalf("terminal count=%d, want one actual exit", terminals)
			}
		})
	}
}

func TestGeminiApprovalWriteCannotResurrectCompletedTurn(t *testing.T) {
	adapter, events, ctx := geminiTestAdapter(t, "permission", Config{})
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "approval-late", Text: "BODY 🌟"}); err != nil {
		t.Fatal(err)
	}
	approval := geminiEvent(t, ctx, events, model.RuntimeApprovalRequested)
	adapter.mu.Lock()
	adapter.stdin = terminalBeforeWriteReturns{adapter.stdin, func() {
		geminiEvent(t, ctx, events, model.RuntimeTurnCompleted)
	}}
	adapter.mu.Unlock()
	if err := adapter.ResolveApproval(ctx, approval.Approval.ID, model.ApprovalResolution{Decision: "accept"}); err != nil {
		t.Fatal(err)
	}
	if got := adapter.State(); got != model.StateIdle {
		t.Fatalf("late approval Write resurrected completed Turn: %s", got)
	}
}

func TestClaudeLateApprovalPreservesTerminalState(t *testing.T) {
	for _, mode := range []string{"success", "error"} {
		t.Run(mode, func(t *testing.T) {
			adapter := NewClaude(Config{SessionID: "bound-session"}, func(model.RuntimeEvent) {})
			adapter.cmd = &exec.Cmd{}
			adapter.state = model.StateWaiting
			adapter.pending = []claudePending{{input: model.AgentInput{MessageID: "late-approval"}, turnID: "approval-turn"}}
			adapter.approvals["approval"] = claudeApprovalRequest{requestID: "native", toolName: "Bash"}
			adapter.stdin = terminalBeforeWriteReturns{&testWriteCloser{}, func() {
				result := map[string]any{"type": "result", "subtype": "success", "result": "ok", "session_id": "bound-session"}
				if mode == "error" {
					result["subtype"], result["is_error"], result["error"] = "error_during_execution", true, "native failure"
				}
				record, _ := json.Marshal(result)
				adapter.handleLine(record)
			}}
			if err := adapter.ResolveApproval(context.Background(), "approval", model.ApprovalResolution{Decision: "accept"}); err != nil {
				t.Fatal(err)
			}
			want := model.StateIdle
			if mode == "error" {
				want = model.StateError
			}
			if got := adapter.State(); got != want {
				t.Fatalf("late approval overwrote terminal state: %s, want %s", got, want)
			}
		})
	}
}

func TestCodexApprovalBeforeStartAckResumesRequestedTurn(t *testing.T) {
	for _, turnID := range []string{"approval-turn", ""} {
		t.Run("request-turn="+turnID, func(t *testing.T) {
			adapter := NewCodex(Config{}, func(model.RuntimeEvent) {})
			adapter.cmd = &exec.Cmd{}
			input := model.AgentInput{MessageID: "pre-ack-approval"}
			adapter.startingInput = &input
			params, _ := json.Marshal(map[string]any{"turnId": turnID})
			adapter.handleServerRequest(json.RawMessage("1"), "item/commandExecution/requestApproval", params)
			var approvalID string
			for id := range adapter.approvals {
				approvalID = id
			}
			adapter.stdin = terminalBeforeWriteReturns{&testWriteCloser{}, func() {
				if err := adapter.acceptTurnStart(input, json.RawMessage("{\"turn\":{\"id\":\"approval-turn\"}}")); err != nil {
					t.Error(err)
				}
			}}
			if err := adapter.ResolveApproval(context.Background(), approvalID, model.ApprovalResolution{Decision: "accept"}); err != nil {
				t.Fatal(err)
			}
			if got := adapter.State(); got != model.StateWorking {
				t.Fatalf("approval did not resume the acknowledged turn: %s", got)
			}
		})
	}
}

// Hold a Working event at the sink while the reader publishes a real terminal
// record. State publication, not only the active-state check, must be ordered.
func TestApprovalStatePublicationOrdersTerminal(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "codex-resolved", "grok", "gemini"} {
		t.Run(runtime, func(t *testing.T) {
			working := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			events := newEventLog()
			sink := func(event model.RuntimeEvent) {
				if event.Kind == model.RuntimeState && event.State == model.StateWorking {
					close(working)
					<-release
				}
				events.add(event)
			}
			input := model.AgentInput{MessageID: "approval-input", Text: "review"}
			var resolve func() error
			var complete func()
			if runtime == "claude" {
				adapter := NewClaude(Config{SessionID: "bound-session"}, sink)
				adapter.cmd = &exec.Cmd{Process: &os.Process{}}
				adapter.stdin, adapter.state = &testWriteCloser{}, model.StateWaiting
				adapter.pending = []claudePending{{input: input, turnID: "approval-turn"}}
				adapter.approvals["approval"] = claudeApprovalRequest{requestID: "native", toolName: "Bash"}
				resolve = func() error {
					return adapter.ResolveApproval(context.Background(), "approval", model.ApprovalResolution{Decision: "accept"})
				}
				complete = func() {
					adapter.handleLine([]byte("{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"ok\",\"session_id\":\"bound-session\"}"))
				}
			} else if runtime == "codex" || runtime == "codex-resolved" {
				adapter := NewCodex(Config{SessionID: "bound-session"}, sink)
				adapter.cmd = &exec.Cmd{Process: &os.Process{}}
				adapter.stdin, adapter.state = &testWriteCloser{}, model.StateWaiting
				adapter.currentTurn = "approval-turn"
				adapter.turnInputs["approval-turn"] = []model.AgentInput{input}
				adapter.approvals["approval"] = pendingApproval{rawID: json.RawMessage("1"), method: "item/commandExecution/requestApproval", turnID: "approval-turn"}
				resolve = func() error {
					return adapter.ResolveApproval(context.Background(), "approval", model.ApprovalResolution{Decision: "accept"})
				}
				if runtime == "codex-resolved" {
					resolve = func() error {
						adapter.handleServerRequestResolved(json.RawMessage("{\"requestId\":1}"))
						return nil
					}
				}
				complete = func() {
					adapter.handleTurnCompleted(json.RawMessage("{\"turn\":{\"id\":\"approval-turn\",\"status\":\"completed\"}}"))
				}
			} else {
				adapter := NewGrok(Config{}, sink)
				if runtime == "gemini" {
					adapter = NewGemini(Config{}, sink)
				}
				adapter.cmd = &exec.Cmd{}
				adapter.stdin, adapter.state = &testWriteCloser{}, model.StateWaiting
				turn := &acpTurn{turnID: "approval-turn", inputs: []model.AgentInput{input}}
				adapter.turn = turn
				adapter.approvals["approval"] = acpPendingApproval{rawID: json.RawMessage("1"), options: []acpPermissionOption{{ID: "once", Kind: "allow_once"}}}
				resolve = func() error {
					return adapter.ResolveApproval(context.Background(), "approval", model.ApprovalResolution{Decision: "accept"})
				}
				complete = func() {
					reply := make(chan acpRPCReply, 1)
					reply <- acpRPCReply{result: json.RawMessage("{\"stopReason\":\"end_turn\"}")}
					adapter.awaitPrompt(turn, reply)
				}
			}
			resolved := make(chan error, 1)
			go func() { resolved <- resolve() }()
			select {
			case <-working:
			case <-time.After(5 * time.Second):
				t.Fatal("approval did not publish Working")
			}
			terminalStarted, terminalDone := make(chan struct{}), make(chan struct{})
			go func() {
				close(terminalStarted)
				complete()
				close(terminalDone)
			}()
			<-terminalStarted
			// An unprotected terminal completes while Working publication is held.
			// The protected path waits for that publication to be released.
			select {
			case <-terminalDone:
			case <-time.After(100 * time.Millisecond):
			}
			unblock()
			select {
			case err := <-resolved:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("approval did not finish")
			}
			select {
			case <-terminalDone:
			case <-time.After(5 * time.Second):
				t.Fatal("terminal did not finish")
			}
			terminalSeen := false
			for _, event := range events.snapshot() {
				if event.Kind == model.RuntimeTurnCompleted {
					terminalSeen = true
				}
				if terminalSeen && event.Kind == model.RuntimeState && event.State == model.StateWorking {
					t.Fatalf("Working published after terminal: %+v", event)
				}
			}
			if !terminalSeen {
				t.Fatal("missing native terminal")
			}
		})
	}
}
