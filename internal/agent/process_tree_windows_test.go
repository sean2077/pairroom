//go:build windows

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// writeBatchShim reproduces the npm launcher shape: a .cmd file that runs the
// real CLI (here the test binary) as a grandchild of PairRoom.
func writeBatchShim(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".cmd")
	body := "@echo off\r\n\"" + os.Args[0] + "\" %*\r\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// windowsProcessSurvives reports whether pid is still running after grace.
// Job termination closes a process's handles (so its pipes report EOF) just
// before the process object is signaled; the short grace absorbs that, while
// a CLI that was never killed stays alive far longer.
func windowsProcessSurvives(pid int, grace time.Duration) bool {
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	state, err := syscall.WaitForSingleObject(handle, uint32(grace/time.Millisecond))
	return err == nil && state == syscall.WAIT_TIMEOUT
}

func killLeftover(t *testing.T, pid int) {
	t.Cleanup(func() {
		if process, err := os.FindProcess(pid); err == nil {
			_ = process.Kill()
		}
	})
}

func TestCodexStopKillsCLIBehindBatchShim(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_HELPER_PID_FILE", pidFile)
	t.Setenv("PAIRROOM_HELPER_IGNORE_EOF", "1")
	adapter := NewCodex(Config{Command: writeBatchShim(t, "codex"), Repo: t.TempDir()}, func(model.RuntimeEvent) {})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adapter.Start(ctx); err != nil {
		t.Fatal(err)
	}
	pid, err := readHelperPID(pidFile, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	killLeftover(t, pid)
	if err := adapter.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if windowsProcessSurvives(pid, 2*time.Second) {
		t.Fatal("Stop reported success while the CLI behind the .cmd shim was still running")
	}
}

func TestClaudeInterruptSettlesTurnOnlyAfterShimTreeExits(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv("PAIRROOM_CLAUDE_SCRIPT", "hang")
	t.Setenv("PAIRROOM_HELPER_PID_FILE", pidFile)
	t.Setenv("PAIRROOM_HELPER_IGNORE_EOF", "1")
	var mu sync.Mutex
	pid := 0
	completedWhileAlive := false
	completed := false
	adapter := NewClaude(Config{Command: writeBatchShim(t, "claude"), Repo: t.TempDir(), DataDir: t.TempDir()}, func(event model.RuntimeEvent) {
		if event.Kind != model.RuntimeTurnCompleted {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		completed = true
		if pid != 0 && windowsProcessSurvives(pid, 2*time.Second) {
			completedWhileAlive = true
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := adapter.StartTurn(ctx, model.AgentInput{MessageID: "m1", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	found, err := readHelperPID(pidFile, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	killLeftover(t, found)
	mu.Lock()
	pid = found
	mu.Unlock()
	if err := adapter.Interrupt(context.Background()); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !completed {
		t.Fatal("interrupt did not settle the pending Turn")
	}
	if completedWhileAlive || windowsProcessSurvives(found, 2*time.Second) {
		t.Fatal("interrupt released the Turn while the CLI behind the .cmd shim was still running")
	}
}

func readHelperPID(path string, deadline time.Duration) (int, error) {
	stop := time.Now().Add(deadline)
	for {
		raw, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(raw)) != "" {
			return strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		if time.Now().After(stop) {
			return 0, fmt.Errorf("helper PID file %s was not written", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
