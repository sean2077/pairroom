package room

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/execx"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
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

func TestCloseStartupProcessHelper(t *testing.T) {
	if os.Getenv("PAIRROOM_CLOSE_START_HELPER") == "1" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

// A real helper process models OS startup already admitted before Close.
// Cancellation cannot undo that creation; Close must wait, then stop it.
type startupProcessAdapter struct {
	agent.Adapter
	entered chan struct{}
	release chan struct{}
	started chan struct{}
	mu      sync.Mutex
	stopMu  sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	tree    *execx.Tree
	stops   atomic.Int64
}

func (a *startupProcessAdapter) Start(ctx context.Context) error {
	close(a.entered)
	<-a.release
	defer close(a.started)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCloseStartupProcessHelper$")
	cmd.Env = append(os.Environ(), "PAIRROOM_CLOSE_START_HELPER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	tree, err := execx.StartTree(cmd)
	if err != nil {
		_ = stdin.Close()
		return err
	}
	a.mu.Lock()
	a.cmd = cmd
	a.stdin = stdin
	a.tree = tree
	a.mu.Unlock()
	if err := a.Adapter.Start(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

func (a *startupProcessAdapter) Stop(ctx context.Context) error {
	a.stopMu.Lock()
	defer a.stopMu.Unlock()
	a.stops.Add(1)
	a.mu.Lock()
	cmd, stdin, tree := a.cmd, a.stdin, a.tree
	a.mu.Unlock()
	if cmd == nil {
		return nil
	}
	_ = stdin.Close()
	err := cmd.Wait()
	tree.Release()
	a.mu.Lock()
	a.cmd = nil
	a.stdin = nil
	a.tree = nil
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return a.Adapter.Stop(ctx)
}

func TestCloseWaitsForAdmittedAutoStartBeforeStoppingProcess(t *testing.T) {
	writer, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := &startupProcessAdapter{entered: make(chan struct{}), release: make(chan struct{}), started: make(chan struct{})}
	factory := func(cfg agent.Config, sink agent.EventSink) agent.Adapter {
		if cfg.Actor == model.ActorSlot1 {
			a.Adapter = agent.NewMock(cfg, sink)
			return a
		}
		return agent.NewMock(cfg, sink)
	}
	e, err := New(Config{Name: "startup", Repo: t.TempDir(), Store: writer, AutoStart: true, Slot1Factory: factory, Slot2Factory: factory})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(a.release) }) }
	t.Cleanup(func() { release(); <-a.started; _ = e.Close(); _ = a.Stop(context.Background()) })
	select {
	case <-a.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("AutoStart did not enter the native startup boundary")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = e.CloseContext(ctx)
	cancel()
	if !errors.Is(err, ErrClosePending) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close before startup settled: %v", err)
	}
	if a.stops.Load() != 0 {
		t.Fatal("Close reported Stop before admitted startup finished")
	}
	release()
	<-a.started
	a.mu.Lock()
	cmd := a.cmd
	a.mu.Unlock()
	if cmd == nil {
		t.Fatal("real startup helper was not created")
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() || a.State() != model.StateStopped {
		t.Fatal("Close left the admitted process running")
	}
	if e.Fatal() != nil {
		t.Fatalf("startup close wrote evidence after Store closed: %v", e.Fatal())
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

func TestClosedEngineRejectsAgentStarts(t *testing.T) {
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending", false: "completed"}[pending], func(t *testing.T) {
			e, adapters := newTestEngine(t, "")
			if pending {
				adapters[model.ActorSlot1].stopErr = errors.New("still stopping")
			}
			t.Cleanup(func() {
				adapters[model.ActorSlot1].mu.Lock()
				adapters[model.ActorSlot1].stopErr = nil
				adapters[model.ActorSlot1].mu.Unlock()
				_ = e.Close()
			})
			_ = e.Close()
			for _, start := range []func(context.Context, model.ActorID) error{e.StartAgent, e.RestartAgent} {
				if err := start(context.Background(), model.ActorSlot2); err == nil {
					t.Fatal("closed Engine admitted an adapter start")
				}
			}
			adapters[model.ActorSlot2].mu.Lock()
			starts := adapters[model.ActorSlot2].starts
			adapters[model.ActorSlot2].mu.Unlock()
			if starts != 0 {
				t.Fatalf("closed Engine started adapter %d times", starts)
			}
		})
	}
}
