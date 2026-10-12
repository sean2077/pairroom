package relay

import (
	"errors"
	"fmt"

	"github.com/sean2077/pairroom/internal/model"
)

// ValidateNativeHistory applies the same Native fact validators used by Open
// without a Store or any restoration, delivery, wake, or identity effects.
// The Service owns provisioning, lifecycle, and Event Log ordering checks;
// runtimes is its immutable initial selection, not a current display snapshot.
func ValidateNativeHistory(roomID string, events []model.Event, runtimes map[model.ActorID]model.RuntimeKind, sharedSlot model.ActorID, dynamic bool) error {
	_, err := replayNativeHistory(roomID, events, runtimes, sharedSlot, dynamic)
	return err
}

func replayNativeHistory(roomID string, events []model.Event, runtimes map[model.ActorID]model.RuntimeKind, sharedSlot model.ActorID, dynamic bool) (*Engine, error) {
	if roomID == "" {
		return nil, errors.New("native relay requires a Room identity")
	}
	// Legacy LAN replay updates the effective remote runtime. It must never
	// rewrite the caller's provisioning selections during this read-only pass.
	kinds := make(map[model.ActorID]model.RuntimeKind, len(runtimes))
	for slot, kind := range runtimes {
		kinds[slot] = kind
	}
	cfg := Config{RoomID: roomID, Runtimes: kinds, SharedSlot: sharedSlot, DynamicBindings: dynamic}
	initializeDynamicRuntimes(&cfg)
	e := &Engine{cfg: cfg, replaying: true, bindings: map[model.ActorID]bindingFact{}, seenBinds: map[string]bool{}, messages: map[string]Message{}, inFlight: map[string]struct{}{}, sends: map[string]string{}, reports: map[string]Publication{}, lastReport: map[string]uint64{}, wakeEnabled: true, wakeReserved: map[string]bool{}, waiters: map[model.ActorID]int{}}
	for _, ev := range events {
		if ev.RoomID != roomID {
			return nil, errors.New("native relay Room identity mismatch")
		}
		if err := e.apply(ev); err != nil {
			return nil, fmt.Errorf("native replay at event %d: %w", ev.Seq, err)
		}
	}
	return e, nil
}
