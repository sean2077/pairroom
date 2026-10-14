package relay

import (
	"encoding/json"
	"errors"
	"github.com/sean2077/pairroom/internal/model"
	"time"
)

// PrepareEnterpriseRecovery is startup-only planning over a strict isolated
// replay. Its only transition is delivering -> unknown; queued work and every
// original receipt, author and generation remain unchanged. It emits no input.
func PrepareEnterpriseRecovery(snapshot EnterpriseRoomSnapshot) (func(time.Time) ([]model.Event, error), error) {
	copy := cloneEnterpriseOperationSnapshot(snapshot)
	return func(at time.Time) ([]model.Event, error) {
		if at.IsZero() {
			return nil, errors.New("recovery requires a fixed timestamp")
		}
		view, e := enterpriseOperationView(copy)
		if e != nil {
			return nil, e
		}
		inFlight := make([]Message, 0, len(view.inFlight))
		for id := range view.inFlight {
			inFlight = append(inFlight, view.messages[id])
		}
		facts := planExpiryFacts(inFlight, at, view.cfg.Lease, true)
		events := make([]model.Event, 0, len(facts))
		for _, fact := range facts {
			data, e := json.Marshal(fact)
			if e != nil {
				return nil, e
			}
			events = append(events, model.Event{RoomID: snapshot.RoomID, Kind: EventMessage, Actor: model.ActorSystem, CreatedAt: at, Data: data})
		}
		return events, nil
	}, nil
}
