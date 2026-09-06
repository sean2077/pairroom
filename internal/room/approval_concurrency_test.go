package room

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type gatedApprovalAdapter struct {
	*fakeAdapter
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
	err     error
}

func (a *gatedApprovalAdapter) ResolveApproval(ctx context.Context, _ string, _ model.ApprovalResolution) error {
	if a.calls.Add(1) == 1 {
		close(a.started)
	}
	select {
	case <-a.release:
		return a.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestApprovalHasSingleResolutionOwner(t *testing.T) {
	e, adapters := newTestEngine(t, "")
	a := &gatedApprovalAdapter{fakeAdapter: adapters[model.ActorClaude], started: make(chan struct{}), release: make(chan struct{})}
	e.mu.Lock()
	e.adapters[model.ActorClaude] = a
	e.mu.Unlock()
	if _, err := e.record(EventApprovalUpdated, model.ActorClaude, model.Approval{ID: "ask", Agent: model.ActorClaude, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.ResolveApproval(ctx, "ask", model.ApprovalResolution{Decision: "accept"}) }()
	select {
	case <-a.started:
	case <-ctx.Done():
		t.Fatal("adapter was not entered")
	}
	// A second browser/API caller must fail before touching the native RPC.
	second, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if err := e.ResolveApproval(second, "ask", model.ApprovalResolution{Decision: "decline"}); err == nil {
		t.Error("concurrent conflicting decision accepted")
	}
	if a.calls.Load() != 1 {
		t.Errorf("native adapter called %d times", a.calls.Load())
	}
	close(a.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := e.ResolveApproval(ctx, "ask", model.ApprovalResolution{Decision: "accept"}); err == nil {
		t.Fatal("terminal approval accepted again")
	}
	if got := e.Snapshot().Approvals[0]; got.Status != "resolved" || got.Decision != "accept" {
		t.Fatalf("unexpected durable resolution: %+v", got)
	}
}

func TestInvalidApprovalResolutionReleasesReservation(t *testing.T) {
	e, adapters := newTestEngine(t, "")
	a := &gatedApprovalAdapter{fakeAdapter: adapters[model.ActorClaude], started: make(chan struct{}), release: make(chan struct{}), err: errors.New("invalid native choice")}
	close(a.release)
	e.mu.Lock()
	e.adapters[model.ActorClaude] = a
	e.mu.Unlock()
	if _, err := e.record(EventApprovalUpdated, model.ActorClaude, model.Approval{ID: "ask", Agent: model.ActorClaude, Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.ResolveApproval(ctx, "ask", model.ApprovalResolution{}); !errors.Is(err, context.Canceled) || a.calls.Load() != 0 {
		t.Fatal("cancelled request crossed the adapter boundary")
	}
	for range 2 {
		if err := e.ResolveApproval(context.Background(), "ask", model.ApprovalResolution{}); err == nil {
			t.Fatal("invalid decision accepted")
		}
	}
	if a.calls.Load() != 2 || e.Snapshot().Approvals[0].Status != "pending" {
		t.Fatal("failed native validation consumed or locked the pending approval")
	}
}
