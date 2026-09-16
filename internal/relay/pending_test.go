package relay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestHookReadinessNeverClaimsOrCopiesInbox(t *testing.T) {
	e, a, _ := testEngine(t)
	receiver := a[model.ActorCodex]
	m, err := e.Send(a[model.ActorClaude], SendRequest{ID: "large", Text: strings.Repeat("界", 12000)})
	if err != nil {
		t.Fatal(err)
	}
	seq := e.Snapshot().Sequence
	for i := 0; i < 3; i++ {
		ready, err := e.WaitForPending(context.Background(), receiver)
		if err != nil || !ready {
			t.Fatalf("readiness=%v err=%v", ready, err)
		}
	}
	snapshot := e.Snapshot()
	if snapshot.Sequence != seq || snapshot.Messages[0].State != "queued" || snapshot.Messages[0].ID != m.ID {
		t.Fatal("readiness consumed or audited an envelope")
	}
	claim, err := e.Claim(context.Background(), receiver, false)
	if err != nil || claim.ID != m.ID || !strings.Contains(claim.Envelope, m.Text) {
		t.Fatal("foreground lost original message")
	}
	_, err = e.Send(a[model.ActorClaude], SendRequest{ID: "next", Text: "next"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if ready, err := e.WaitForPending(ctx, receiver); ready || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("readiness bypassed an in-flight delivery")
	}
	if err := e.Ack(receiver, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	if ready, err := e.WaitForPending(context.Background(), receiver); err != nil || !ready {
		t.Fatal("ack did not release the next FIFO")
	}
}

func TestHookReadinessHonorsDisabledParkCancellationAndRevocation(t *testing.T) {
	e, a, _ := testEngine(t)
	receiver := a[model.ActorCodex]
	if err := e.Park(receiver.Slot, false); err != nil {
		t.Fatal(err)
	}
	if ready, err := e.WaitForPending(context.Background(), receiver); err != nil || ready {
		t.Fatal("disabled park waited")
	}
	if err := e.Park(receiver.Slot, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ready, err := e.WaitForPending(ctx, receiver); ready || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled readiness continued")
	}
	if err := e.Unbind(receiver.Slot); err != nil {
		t.Fatal(err)
	}
	if ready, err := e.WaitForPending(context.Background(), receiver); ready || !errors.Is(err, ErrAuth) {
		t.Fatal("revoked binding can probe")
	}
}

func TestHookReadinessWakesWithoutPublicationOrClaim(t *testing.T) {
	e, a, _ := testEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		ready, err := e.WaitForPending(ctx, a[model.ActorCodex])
		if err == nil && !ready {
			err = errors.New("missing readiness")
		}
		done <- err
	}()
	if _, err := e.Send(a[model.ActorClaude], SendRequest{ID: "new", Text: "new input"}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if e.Snapshot().Messages[0].State != "queued" {
		t.Fatal("probe consumed the message")
	}
}
