package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type observedPipeWriter struct {
	io.WriteCloser
	started chan struct{}
}

func (w observedPipeWriter) Write(data []byte) (int, error) {
	close(w.started)
	return w.WriteCloser.Write(data)
}

// The peer holds stdin open without reading. Cancellation must bound the pipe
// write itself, not just the subsequent RPC response wait. These are real OS
// pipes, not a vendor/model acceptance test.
func TestNativeControlCancellationUnblocksStdin(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "grok", "gemini"} {
		t.Run(runtime, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			started := make(chan struct{})
			stdin := observedPipeWriter{writer, started}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sink := func(model.RuntimeEvent) {}
			payload := map[string]any{"text": strings.Repeat("x", 4<<20)}
			done := make(chan error, 1)
			switch runtime {
			case "claude":
				adapter := NewClaude(Config{}, sink)
				adapter.stdin = stdin
				go func() { _, err := adapter.sendControlRequest(ctx, payload); done <- err }()
			case "codex":
				adapter := NewCodex(Config{}, sink)
				adapter.stdin = stdin
				go func() { _, err := adapter.call(ctx, "test/cancel", payload); done <- err }()
			default:
				adapter := NewGrok(Config{}, sink)
				if runtime == "gemini" {
					adapter = NewGemini(Config{}, sink)
				}
				adapter.stdin = stdin
				go func() { _, err := adapter.call(ctx, "test/cancel", payload); done <- err }()
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("write never started")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(time.Second):
				// Unblock and join the old implementation before failing the test.
				_ = writer.Close()
				<-done
				t.Fatal("cancellation did not unblock the native stdin write")
			}
		})
	}
}

type exitBeforeWriteReturns struct {
	io.WriteCloser
	exited <-chan struct{}
}

func (w exitBeforeWriteReturns) Write(data []byte) (int, error) {
	n, err := w.WriteCloser.Write(data)
	<-w.exited
	return n, err
}

func TestGeminiExitDuringPromptWriteRetainsExactBinding(t *testing.T) {
	adapter, events, ctx := geminiTestAdapter(t, "exit", Config{})
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := adapter.ensureSession(ctx); err != nil {
		t.Fatal(err)
	}
	adapter.mu.Lock()
	adapter.stdin = exitBeforeWriteReturns{adapter.stdin, adapter.done}
	adapter.mu.Unlock()
	// The process consumes the prompt then exits before the caller observes the
	// successful Write. This ordering must not allow a fresh replacement session.
	err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "exit-write", Text: "BODY 🌟"})
	if adapter.SessionID() != "gemini-new-session" {
		t.Fatalf("accepted prompt lost exact session binding: %q (submission: %v)", adapter.SessionID(), err)
	}
	if adapter.State() == model.StateWorking {
		t.Fatal("late write completion resurrected an exited process")
	}
	if !errors.Is(err, ErrSubmissionUnknown) {
		t.Fatalf("ambiguous submit returned %v", err)
	}
	geminiEvent(t, ctx, events, model.RuntimeTurnCompleted)
	if err := adapter.Start(ctx); !errors.Is(err, errGeminiExactResume) {
		t.Fatalf("unsafe restart: %v", err)
	}
}

func TestQueuedStdinCancellationDoesNotCloseActiveWriter(t *testing.T) {
	reader, pipe := io.Pipe()
	defer reader.Close()
	defer pipe.Close()
	started := make(chan struct{})
	firstWriter := observedPipeWriter{pipe, started}
	var writer nativeStdinWriter
	first := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { first <- writer.write(ctx, func() io.WriteCloser { return firstWriter }, []byte("first")) }()
	<-started
	queued, cancelQueued := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancelQueued()
	if err := writer.write(queued, func() io.WriteCloser {
		t.Error("cancelled queued writer inspected the live transport")
		return pipe
	}, []byte("second")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued result: %v", err)
	}
	body := make([]byte, 5)
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "first" {
		t.Fatalf("interleaved frame: %q", body)
	}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	// Cancelling an already completed caller must not close a reused endpoint.
	cancel()
	again := make(chan error, 1)
	go func() {
		again <- writer.write(context.Background(), func() io.WriteCloser { return pipe }, []byte("third"))
	}()
	if _, err := io.ReadFull(reader, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "third" {
		t.Fatal(string(body))
	}
	if err := <-again; err != nil {
		t.Fatal(err)
	}
}

func TestNativePartialPromptWriteKeepsCorrelationUntilExit(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "grok", "gemini"} {
		t.Run(runtime, func(t *testing.T) {
			sink := func(event model.RuntimeEvent) {
				if event.Kind == model.RuntimeInputFailed || event.Kind == model.RuntimeTurnCompleted {
					t.Errorf("ownership settled before process-exit evidence: %s", event.Kind)
				}
			}
			// The process sentinel only bypasses startup; no process is started or
			// signalled. The nil process tree and short writer isolate wire semantics.
			cmd := &exec.Cmd{Process: &os.Process{}}
			short := acpShortWriter{count: 1, err: io.ErrClosedPipe}
			input := model.AgentInput{MessageID: "partial", Text: "keep correlation"}
			var adapter Adapter
			var retained func() bool
			switch runtime {
			case "claude":
				a := NewClaude(Config{}, sink)
				a.cmd, a.stdin, a.state = cmd, short, model.StateIdle
				adapter = a
				retained = func() bool { return len(a.pending) == 1 && a.pending[0].input.MessageID == input.MessageID }
			case "codex":
				a := NewCodex(Config{}, sink)
				a.cmd, a.stdin, a.state, a.threadID = cmd, short, model.StateIdle, "bound"
				adapter = a
				retained = func() bool { return a.startingInput != nil && a.startingInput.MessageID == input.MessageID }
			default:
				a := NewGrok(Config{}, sink)
				if runtime == "gemini" {
					a = NewGemini(Config{}, sink)
				}
				a.cmd, a.stdin, a.state, a.sessionID, a.sessionOpened = cmd, short, model.StateIdle, "bound", true
				adapter = a
				retained = func() bool { return a.turn != nil && a.turn.inputs[0].MessageID == input.MessageID && a.sessionEngaged }
			}
			if err := adapter.StartTurn(context.Background(), input); !errors.Is(err, ErrSubmissionUnknown) {
				t.Fatalf("partial write reported as replayable: %v", err)
			}
			if !retained() {
				t.Fatal("partial write discarded staged correlation")
			}
			if err := adapter.StartTurn(context.Background(), model.AgentInput{MessageID: "next"}); err == nil {
				t.Fatal("accepted a second Turn before real exit")
			}
		})
	}
}

func TestGeminiStderrOverflowSettlesPromptOnRealExit(t *testing.T) {
	adapter, events, ctx := geminiTestAdapter(t, "stderr-overflow", Config{})
	// Exit may race the caller observing the completed Write. Either accepted
	// or unknown is valid; only real process exit may settle the input below.
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "stderr", Text: "BODY 🌟"}); err != nil && !errors.Is(err, ErrSubmissionUnknown) {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	terminal := geminiEvent(t, deadline, events, model.RuntimeTurnCompleted)
	if terminal.Name != "process_exited" || adapter.SessionID() != "gemini-new-session" {
		t.Fatalf("terminal lost binding: %+v", terminal)
	}
}

func TestNativeStderrReadFailureIsFatal(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "grok", "gemini"} {
		t.Run(runtime, func(t *testing.T) {
			reported := false
			sink := func(event model.RuntimeEvent) {
				if event.Kind == model.RuntimeError && event.Name == "adapter.stream_error" {
					reported = true
				}
			}
			reader := strings.NewReader(strings.Repeat("x", 2<<20))
			switch runtime {
			case "claude":
				NewClaude(Config{}, sink).readStderr(reader)
			case "codex":
				NewCodex(Config{}, sink).readStderr(reader)
			case "grok":
				NewGrok(Config{}, sink).readStderr(reader)
			case "gemini":
				NewGemini(Config{}, sink).readStderr(reader)
			}
			if !reported {
				t.Fatal("stderr reader silently abandoned a live transport")
			}
		})
	}
}

type terminalBeforeWriteReturns struct {
	io.WriteCloser
	complete func()
}

func (w terminalBeforeWriteReturns) Write(data []byte) (int, error) {
	n, err := w.WriteCloser.Write(data)
	w.complete()
	return n, err
}

// A completed result or process exit can be observed before the parent's Write
// returns. Neither permits that late completion to resurrect a Working Turn.
func TestClaudePromptWriteCannotResurrectTerminalTurn(t *testing.T) {
	for _, mode := range []string{"exit", "success"} {
		t.Run(mode, func(t *testing.T) {
			childMode := mode
			if mode == "success" {
				childMode = "hang"
			}
			t.Setenv("PAIRROOM_CLAUDE_SCRIPT", childMode)
			events := newEventLog()
			adapter := NewClaude(Config{Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(), SessionID: "bound-session", RequireExactSession: true}, events.add)
			t.Cleanup(func() { stopWithin(adapter, 10*time.Second) })
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := adapter.Start(ctx); err != nil {
				t.Fatal(err)
			}
			adapter.mu.Lock()
			if mode == "exit" {
				adapter.stdin = exitBeforeWriteReturns{adapter.stdin, adapter.procDone}
			} else {
				// Deliver a complete native record synchronously at the observed
				// write boundary, so the final state cannot race the assertion.
				adapter.stdin = terminalBeforeWriteReturns{adapter.stdin, func() {
					adapter.handleLine([]byte(`{"type":"result","subtype":"success","result":"ok","session_id":"bound-session"}`))
				}}
			}
			adapter.mu.Unlock()
			err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "late-write", Text: "hello"})
			if mode == "exit" && !errors.Is(err, ErrSubmissionUnknown) {
				t.Errorf("exit submission: %v", err)
			}
			if mode == "success" && err != nil {
				t.Errorf("completed submission: %v", err)
			}
			if adapter.State() == model.StateWorking {
				t.Fatal("late write completion resurrected the terminal Turn")
			}
			if adapter.SessionID() != "bound-session" {
				t.Fatal("terminal lost exact Binding")
			}
			sawTerminal := false
			for _, event := range events.snapshot() {
				if event.Kind == model.RuntimeTurnCompleted && event.CorrelationID == "late-write" {
					sawTerminal = true
				}
				if sawTerminal && (event.Kind == model.RuntimeTurnStarted || event.Kind == model.RuntimeInputProcessing) {
					t.Fatalf("late event: %+v", event)
				}
			}
			if !sawTerminal {
				t.Fatal("missing terminal evidence")
			}
		})
	}
}
