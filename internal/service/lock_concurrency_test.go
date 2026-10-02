package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/execx"
)

// The subprocess has its own Go mutexes and descriptors. Its readiness line is
// emitted immediately before trying the real public acquisition boundary.
func TestServiceLockProcessHelper(t *testing.T) {
	root := os.Getenv("PAIRROOM_TEST_SERVICE_LOCK_ROOT")
	if root == "" {
		return
	}
	fmt.Println("ready")
	if os.Getenv("PAIRROOM_TEST_SERVICE_LOCK_MODE") == "abandon" {
		unlock, err := lockServiceRoot(root)
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		fmt.Println("held")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0) // deliberately abandon the kernel guard without defers
	}
	lock, err := AcquireServiceLock(root, true)
	if err != nil {
		if !errors.Is(err, ErrServiceAlreadyRunning) && !errors.Is(err, ErrServiceLockOwnerRunning) {
			t.Fatal(err)
		}
		fmt.Println("blocked")
		return
	}
	fmt.Println("acquired")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func startServiceLockHelper(t *testing.T, root, mode string) (*bufio.Reader, io.WriteCloser, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServiceLockProcessHelper$")
	execx.NoConsole(cmd)
	cmd.Env = append(os.Environ(), "PAIRROOM_TEST_SERVICE_LOCK_ROOT="+root, "PAIRROOM_TEST_SERVICE_LOCK_MODE="+mode)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	finish := func() {
		once.Do(func() {
			_ = stdin.Close()
			if err := cmd.Wait(); err != nil {
				t.Errorf("Service lock helper: %v: %s", err, stderr.String())
			}
		})
	}
	t.Cleanup(finish)
	reader := bufio.NewReader(stdout)
	if line, err := reader.ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	return reader, stdin, finish
}

func TestServiceLockRecoveryDoesNotExposeVacancyToAnotherProcess(t *testing.T) {
	root := t.TempDir()
	writeDeadServiceLock(t, root)
	moved, resume := make(chan struct{}), make(chan struct{})
	serviceLockRecoveryHook = func(string, string) {
		close(moved)
		<-resume
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	defer release() // unblock recovery before subprocess cleanup on any failure
	t.Cleanup(func() { release(); serviceLockRecoveryHook = nil })
	type result struct {
		lock *ServiceLock
		err  error
	}
	first := make(chan result, 1)
	go func() { lock, err := AcquireServiceLock(root, true); first <- result{lock, err} }()
	<-moved
	reader, _, finish := startServiceLockHelper(t, root, "acquire")
	type probeResult struct {
		line string
		err  error
	}
	next := make(chan probeResult, 1)
	go func() { line, err := reader.ReadString('\n'); next <- probeResult{strings.TrimSpace(line), err} }()
	var early probeResult
	premature := false
	select {
	case early = <-next:
		premature = true
	case <-time.After(100 * time.Millisecond):
	}
	release()
	owner := <-first
	if owner.lock != nil {
		defer owner.lock.Close()
	}
	if premature {
		t.Fatalf("another process finished acquisition during recovery's vacancy: %s, %v", early.line, early.err)
	}
	if owner.err != nil {
		t.Fatal(owner.err)
	}
	if reply := <-next; reply.err != nil || reply.line != "blocked" {
		t.Fatalf("second process acquisition = %q, %v; want blocked by the published live owner", reply.line, reply.err)
	}
	finish()
}

func TestServiceRootGuardReleasedWhenProcessExits(t *testing.T) {
	root := t.TempDir()
	reader, stdin, finish := startServiceLockHelper(t, root, "abandon")
	if line, err := reader.ReadString('\n'); err != nil || line != "held\n" {
		t.Fatalf("helper guard = %q, %v", line, err)
	}
	_, _ = io.WriteString(stdin, "exit\n")
	finish()
	lock, err := AcquireServiceLock(root, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverServiceLockMissingRootDoesNotCreateData(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	if err := RecoverServiceLock(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery created a missing data root: %v", err)
	}
}

func TestServiceLockReleasePreservesReplacementOwner(t *testing.T) {
	root := t.TempDir()
	lock, err := AcquireServiceLock(root, false)
	if err != nil {
		t.Fatal(err)
	}
	replacement := mustJSON(t, serviceLockMetadata{PID: os.Getpid(), StartedAt: time.Now().UTC(), Nonce: "replacement"})
	path := filepath.Join(root, "service.lock")
	if err := os.WriteFile(path, replacement, 0600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err == nil {
		t.Fatal("release accepted a different owner's nonce")
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, replacement) {
		t.Fatalf("release changed replacement ownership: %s, %v", after, err)
	}
}

func TestServiceLockConcurrentRecoveryKeepsOneOwner(t *testing.T) {
	// Supplement the cross-process barrier test with competing stale readers.
	// Hold every successful owner until every attempt has finished, so a second
	// success proves overlapping ownership rather than a legitimate later start.
	for iteration := 0; iteration < 20; iteration++ {
		root := t.TempDir()
		writeDeadServiceLock(t, root)
		const workers = 16
		results := make(chan *ServiceLock, workers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				lock, err := AcquireServiceLock(root, true)
				if err == nil {
					results <- lock
				}
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		var locks []*ServiceLock
		for lock := range results {
			locks = append(locks, lock)
		}
		for _, lock := range locks {
			_ = lock.Close()
		}
		if len(locks) != 1 {
			t.Fatalf("iteration %d admitted %d simultaneous owners, want 1", iteration, len(locks))
		}
	}
}

func writeDeadServiceLock(t *testing.T, root string) {
	t.Helper()
	data := []byte(`{"pid":99999999,"started_at":"2026-01-01T00:00:00Z","nonce":"dead"}`)
	if err := os.WriteFile(filepath.Join(root, "service.lock"), data, 0600); err != nil {
		t.Fatal(err)
	}
}
