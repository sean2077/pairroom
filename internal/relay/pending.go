package relay

import "context"

// WaitForPending is a hook-readiness probe for hosts with clipped hook feedback.
// It authenticates each wake and obeys park, generation, drain and FIFO admission,
// but never constructs an envelope, claims a message, or issues a receipt.
// The caller must later use foreground Claim(false) for actual delivery.
func (e *Engine) WaitForPending(ctx context.Context, a Auth) (bool, error) {
	for {
		e.mu.Lock()
		b, err := e.auth(a, false)
		if err != nil {
			e.mu.Unlock()
			return false, err
		}
		if err := ctx.Err(); err != nil {
			e.mu.Unlock()
			return false, err
		}
		if !b.ParkEnabled {
			e.mu.Unlock()
			return false, nil
		}
		if err := e.reapLocked(false); err != nil {
			e.mu.Unlock()
			return false, err
		}
		ready, busy := false, false
		for _, id := range e.order {
			m := e.messages[id]
			if m.To != a.Slot || m.TargetGeneration != a.Generation {
				continue
			}
			if m.State == "delivering" {
				busy = true
				break
			}
			ready = ready || m.State == "queued"
		}
		if ready && !busy {
			e.mu.Unlock()
			return true, nil
		}
		changed := e.changed
		e.mu.Unlock()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-changed:
		}
	}
}
