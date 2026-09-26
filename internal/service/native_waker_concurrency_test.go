package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type blockedSuppressionRelay struct {
	*fakeNativeWakeRelay
	started chan struct{}
	finish  chan struct{}
	calls   atomic.Int64
}

func (f *blockedSuppressionRelay) RecordWake(outcome, reason string, target model.ActorID) error {
	if f.calls.Add(1) == 1 {
		close(f.started)
		<-f.finish
	}
	return f.fakeNativeWakeRelay.RecordWake(outcome, reason, target)
}

func TestNativeWakerSerializesSuppressionBeforeAudit(t *testing.T) {
	candidate := codexCandidate("queued")
	candidate.WaiterActive = true
	state := &blockedSuppressionRelay{
		fakeNativeWakeRelay: &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{"queued": candidate}},
		started:             make(chan struct{}),
		finish:              make(chan struct{}),
	}
	var release sync.Once
	defer release.Do(func() { close(state.finish) })
	w := newNativeWaker(nativeWakerConfig{
		Relay: state,
		Run: func(context.Context, string, ...string) error {
			t.Error("suppressed head invoked a vendor")
			return errors.New("unexpected effect")
		},
		Wait: func(context.Context, time.Duration) error {
			t.Error("suppressed head started a grace wait")
			return errors.New("unexpected wait")
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Wake(ctx, "queued") }()
	select {
	case <-state.started:
	case <-ctx.Done():
		t.Fatal("first suppression did not reach the audit boundary")
	}
	// The first append has not populated lastRecorded yet. A second same-head
	// request must coalesce, rather than race that cache and append again.
	if err := w.Wake(ctx, "queued"); err != nil {
		t.Fatal(err)
	}
	release.Do(func() { close(state.finish) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	reservations, records := state.snapshot()
	if state.calls.Load() != 1 || len(records) != 1 || len(reservations) != 0 || records[0].reason != "waiter_active" {
		t.Fatalf("duplicate suppression audit: calls=%d records=%+v reservations=%+v", state.calls.Load(), records, reservations)
	}
	if err := w.Wake(ctx, "queued"); err != nil || state.calls.Load() != 1 {
		t.Fatalf("unchanged condition appended again: %v", err)
	}
}
