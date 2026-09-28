package room

import (
	"fmt"

	"github.com/sean2077/pairroom/internal/model"
)

// HasRecoverableWork replays a suspended Room's events read-only and reports
// whether activation would rebuild Room-owned FIFO work: an input that never
// crossed the native submission boundary and has not reached a terminal
// processing state. It applies no restore transition and appends nothing, so a
// Service can decide to resume a Room without opening its writer.
func HasRecoverableWork(events []model.Event) (bool, error) {
	e := &Engine{}
	for _, event := range events {
		if err := e.applyLocked(event); err != nil {
			return false, fmt.Errorf("replay event %d (%s): %w", event.Seq, event.Kind, err)
		}
	}
	for _, message := range e.snapshot.Messages {
		for target, state := range message.Delivery {
			if (state == model.DeliveryPending || state == model.DeliveryQueued) && !message.Processing[target].Terminal() {
				return true, nil
			}
		}
	}
	return false, nil
}
