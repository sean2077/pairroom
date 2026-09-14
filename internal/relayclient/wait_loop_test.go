package relayclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestForegroundTimeoutBounds(t *testing.T) {
	for _, seconds := range []int{-1, 0, 1801} {
		if validateForegroundTimeout(seconds) == nil {
			t.Fatalf("accepted timeout %d", seconds)
		}
	}
	for _, seconds := range []int{1, 30, 31, 600, 1800} {
		if err := validateForegroundTimeout(seconds); err != nil {
			t.Fatalf("timeout %d: %v", seconds, err)
		}
	}
}

func TestInboxLoopRenewsOnlyEmptyPolls(t *testing.T) {
	calls := 0
	got, err := waitForInbox(context.Background(), time.Second, time.Millisecond, func(_ context.Context, span time.Duration) (bool, error) {
		calls++
		if span <= 0 || span > time.Millisecond {
			t.Fatalf("invalid poll window: %s", span)
		}
		return calls == 3, nil
	})
	if err != nil || !got || calls != 3 {
		t.Fatalf("delivered=%v calls=%d err=%v", got, calls, err)
	}
}

func TestInboxLoopStopsOnEveryError(t *testing.T) {
	for _, delivered := range []bool{false, true} {
		calls := 0
		failure := errors.New("claim, stdout or acknowledgement is uncertain")
		got, err := waitForInbox(context.Background(), time.Second, time.Millisecond, func(context.Context, time.Duration) (bool, error) {
			calls++
			return delivered, failure
		})
		if !errors.Is(err, failure) || got != delivered || calls != 1 {
			t.Fatalf("delivery error retried: delivered=%v calls=%d err=%v", got, calls, err)
		}
	}
}

func TestInboxLoopCancellationWhileWaiting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	got, err := waitForInbox(ctx, time.Minute, time.Minute, func(context.Context, time.Duration) (bool, error) {
		calls++
		cancel()
		return false, nil
	})
	if !errors.Is(err, context.Canceled) || got || calls != 1 {
		t.Fatalf("cancelled wait continued: delivered=%v calls=%d err=%v", got, calls, err)
	}
}

func TestInboxLoopAlreadyCancelledDoesNotPoll(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := waitForInbox(ctx, time.Minute, time.Second, func(context.Context, time.Duration) (bool, error) {
		t.Fatal("polled after cancellation")
		return false, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInboxLoopEmptyReplyIsPacedAndBounded(t *testing.T) {
	calls := 0
	start := time.Now()
	got, err := waitForInbox(context.Background(), 25*time.Millisecond, time.Second, func(_ context.Context, span time.Duration) (bool, error) {
		calls++
		if span > 25*time.Millisecond {
			t.Fatalf("poll exceeds remaining budget: %s", span)
		}
		return false, nil
	})
	if err != nil || got || calls != 1 || time.Since(start) < 25*time.Millisecond {
		t.Fatalf("empty reply spun or completed: delivered=%v calls=%d err=%v", got, calls, err)
	}
}

func TestInboxLoopDoesNotCancelFinalClaimAtBudgetEdge(t *testing.T) {
	got, err := waitForInbox(context.Background(), 20*time.Millisecond, time.Second, func(ctx context.Context, _ time.Duration) (bool, error) {
		time.Sleep(30 * time.Millisecond)
		if ctx.Err() != nil {
			t.Fatal("local budget cancelled a potentially issued claim")
		}
		return true, nil
	})
	if err != nil || !got {
		t.Fatalf("lost final delivery: delivered=%v err=%v", got, err)
	}
}

func TestInboxLoopExpiredBudgetDoesNotPoll(t *testing.T) {
	got, err := waitForInbox(context.Background(), 0, time.Second, func(context.Context, time.Duration) (bool, error) {
		t.Fatal("polled with expired budget")
		return false, nil
	})
	if err != nil || got {
		t.Fatalf("delivered=%v err=%v", got, err)
	}
}
