package agent

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
)

type processKillFunc func() error

func (kill processKillFunc) Kill() error { return kill() }

func TestStopProcessTreeWaitsForCompletionAfterKillError(t *testing.T) {
	killErr := errors.New("termination failed")
	for _, outcome := range []string{"completed", "pending", "missing"} {
		t.Run(outcome, func(t *testing.T) {
			done := make(chan struct{})
			attempted := make(chan struct{})
			kill := processKillFunc(func() error {
				close(attempted)
				return killErr
			})
			if outcome == "completed" {
				go func() {
					<-attempted
					// Reproduce the reader/wait goroutine's delayed final close,
					// after Kill has returned its error.
					timer := time.NewTimer(50 * time.Millisecond)
					defer timer.Stop()
					<-timer.C
					close(done)
				}()
				t.Cleanup(func() { <-done })
			} else if outcome == "missing" {
				done = nil
			}
			started := time.Now()
			err := stopProcessTree(kill, done, "helper")
			if outcome == "completed" {
				if err != nil {
					t.Fatalf("delayed completion did not settle Kill error: %v", err)
				}
				select {
				case <-done:
				default:
					t.Fatal("stop succeeded before completion evidence")
				}
			} else {
				if !errors.Is(err, killErr) {
					t.Fatalf("original Kill error was not preserved: %v", err)
				}
				if outcome == "pending" && time.Since(started) < processTreeExitTimeout {
					t.Fatal("Kill error returned before waiting for completion")
				}
			}
		})
	}
}

func TestStopProcessTreeStillWaitsForReaders(t *testing.T) {
	// Even a no-op Kill cannot replace reader/wait completion evidence.
	done := make(chan struct{})
	started := time.Now()
	var tree *execx.Tree
	err := stopProcessTree(tree, done, "helper")
	if err == nil || !strings.Contains(err.Error(), "stop state is uncertain") {
		t.Fatalf("stop with open completion channel: %v", err)
	}
	if time.Since(started) < processTreeExitTimeout {
		t.Fatal("stop released ownership before the process-tree timeout")
	}
}
