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

func TestAdaptersStopRetryAfterDelayedCompletion(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			var mu sync.Mutex
			cancelled := 0
			sink := func(event model.RuntimeEvent) {
				if event.Kind == model.RuntimeInputCancelled && event.CorrelationID == "retry-stop" {
					mu.Lock()
					cancelled++
					mu.Unlock()
				}
			}
			var adapter Adapter
			if runtime == "claude" {
				t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "hang")
				adapter = NewClaude(Config{Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir()}, sink)
			} else {
				t.Setenv("PAIRROOM_CODEX_HELPER", "1")
				adapter = NewCodex(Config{Command: os.Args[0], Repo: t.TempDir()}, sink)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "retry-stop", Text: "hold until stopped"}); err != nil {
				t.Fatal(err)
			}
			// Withhold only the completion observation to exercise the adapter's
			// timeout/retry state. The helper process still terminates for real;
			// no process lifetime or vendor acceptance is inferred from this seam.
			delayed := make(chan struct{})
			var realDone chan struct{}
			switch a := adapter.(type) {
			case *ClaudeAdapter:
				a.mu.Lock()
				realDone = a.procDone
				a.procDone = delayed
				a.mu.Unlock()
			case *CodexAdapter:
				a.mu.Lock()
				realDone = a.procDone
				a.procDone = delayed
				a.mu.Unlock()
			}
			t.Cleanup(func() { _ = adapter.Stop(context.Background()) })
			stopCtx, stopCancel := context.WithCancel(context.Background())
			stopCancel()
			if err := adapter.Stop(stopCtx); err == nil || !strings.Contains(err.Error(), "stop state is uncertain") {
				t.Fatalf("first Stop: %v", err)
			}
			select {
			case <-realDone:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			mu.Lock()
			early := cancelled
			mu.Unlock()
			if early != 0 {
				t.Fatalf("input cancelled before completion evidence: %d", early)
			}
			close(delayed)
			if err := adapter.Stop(ctx); err != nil {
				t.Fatalf("retry Stop: %v", err)
			}
			if err := adapter.Stop(ctx); err != nil {
				t.Fatalf("repeated Stop: %v", err)
			}
			mu.Lock()
			count := cancelled
			mu.Unlock()
			if count != 1 {
				t.Fatalf("input cancellation count=%d, want 1", count)
			}
		})
	}
}
