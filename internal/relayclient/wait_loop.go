package relayclient

import (
	"context"
	"errors"
	"time"
)

const maxForegroundTimeoutSeconds = 6 * 60 * 60

func validateForegroundTimeout(seconds int) error {
	if seconds < 0 || seconds > maxForegroundTimeoutSeconds {
		return errors.New("foreground wait timeout must be 0–21600 seconds (0 waits until cancellation)")
	}
	return nil
}

// waitForInbox renews only successful empty long polls inside this CLI process.
// It never retries a failed claim or a delivery, and never publishes anything.
// A zero budget means no PairRoom total deadline: caller cancellation, native
// harness limits, transport/auth failures, or process/service shutdown still
// stop the wait. Finite budgets let the last poll finish normally (including
// stdout/ack), because cancelling at our budget boundary could turn a just-issued
// claim into unknown.
func waitForInbox(ctx context.Context, budget, window time.Duration, poll func(context.Context, time.Duration) (bool, error)) (bool, error) {
	if budget < 0 {
		return false, errors.New("inbox wait budget must not be negative")
	}
	if window <= 0 {
		return false, errors.New("inbox poll window must be positive")
	}
	unbounded := budget == 0
	var deadline time.Time
	if !unbounded {
		deadline = time.Now().Add(budget)
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		span := window
		var remaining time.Duration
		if !unbounded {
			remaining = time.Until(deadline)
			if remaining <= 0 {
				return false, nil
			}
			span = min(window, remaining)
		}
		pollEnd := time.Now().Add(span)
		delivered, err := poll(ctx, span)
		if err != nil || delivered {
			return delivered, err
		}
		// The Service normally spends this whole window waiting for an event.
		// An early empty response must not turn into a tight polling loop.
		pause := time.Until(pollEnd)
		if !unbounded {
			pause = min(pause, time.Until(deadline))
		}
		if pause > 0 {
			timer := time.NewTimer(pause)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false, ctx.Err()
			case <-timer.C:
			}
		}
	}
}
