package relayclient

import (
	"context"
	"errors"
	"time"
)

func validateForegroundTimeout(seconds int) error {
	if seconds < 1 || seconds > 1800 {
		return errors.New("foreground wait timeout must be 1–1800 seconds")
	}
	return nil
}

// waitForInbox renews only successful empty long polls inside this CLI process.
// It never retries a failed claim or a delivery, and never publishes anything.
// The last poll is allowed to finish normally (including stdout/ack): cancelling
// it at our budget boundary could turn a just-issued claim into unknown.
func waitForInbox(ctx context.Context, budget, window time.Duration, poll func(context.Context, time.Duration) (bool, error)) (bool, error) {
	if window <= 0 {
		return false, errors.New("inbox poll window must be positive")
	}
	deadline := time.Now().Add(budget)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, nil
		}
		span := min(window, remaining)
		pollEnd := time.Now().Add(span)
		delivered, err := poll(ctx, span)
		if err != nil || delivered {
			return delivered, err
		}
		// The Service normally spends this whole window waiting for an event.
		// An early empty response must not turn into a tight polling loop.
		pause := min(time.Until(pollEnd), time.Until(deadline))
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
