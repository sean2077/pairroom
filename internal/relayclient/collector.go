package relayclient

import (
	"context"
	"errors"
	"time"
)

var errCollectorBusy = errors.New("another collector is active for this slot; keep its native tool pending or cancel it before collecting again; no message was sent or claimed by this invocation")

// The separate, stable directory is only a kernel-lock anchor, not a queue or
// saved owner record. Process death releases the lock. State/status/send/unbind
// keep using the short-lived slot lock and remain available during a long wait.
func acquireCollector(ctx context.Context, dir string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	anchor, err := secureDir(dir, "collector")
	if err != nil {
		return nil, err
	}
	attempt, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	release, err := lockSlot(attempt, anchor)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errCollectorBusy
		}
		return nil, err
	}
	return release, nil
}
