package relay

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// PrepareEnterpriseBindingRetirement captures an already-authorized SQL
// snapshot and a fixed event identity. It creates exactly one retirement fact;
// the shared apply path derives queued cancellation and delivering unknown.
// It neither grants revocation authority nor writes/reaps delivery state.
func PrepareEnterpriseBindingRetirement(snapshot EnterpriseRoomSnapshot, bindingID, reason, eventID string) (func(time.Time) (model.Event, error), error) {
	if !validEnterpriseOperationID(bindingID) || !validEnterpriseOperationID(eventID) {
		return nil, errors.New("enterprise retirement requires fixed binding and event identities")
	}
	copy := cloneEnterpriseOperationSnapshot(snapshot)
	return func(at time.Time) (model.Event, error) {
		view, err := enterpriseOperationView(copy)
		if err != nil {
			return model.Event{}, err
		}
		return view.enterpriseBindingRetirementFact(bindingID, reason, eventID, at)
	}, nil
}

func (view *Engine) enterpriseBindingRetirementFact(bindingID, reason, eventID string, at time.Time) (model.Event, error) {
	for _, slot := range model.SlotActors() {
		old := view.bindings[slot]
		if old.BindID != bindingID || !old.Active || old.enterprise == nil {
			continue
		}
		at = at.UTC()
		binding := model.EnterpriseBinding{Version: model.EnterpriseBindingVersion, RoomID: view.cfg.RoomID, Slot: slot, BindID: old.BindID, Generation: old.Generation,
			AgentID: old.enterprise.agentID, Runtime: old.Runtime, Active: false, ParkEnabled: old.ParkEnabled, RetiredAt: &at, RetiredReason: reason}
		data, err := json.Marshal(binding)
		if err != nil {
			return model.Event{}, err
		}
		fact := model.Event{ID: eventID, RoomID: view.cfg.RoomID, Kind: model.EnterpriseBindingUpdatedKind, Actor: slot, CreatedAt: at, Data: data}
		if _, err := model.DecodeEnterpriseBindingUpdate(fact); err != nil {
			return model.Event{}, err
		}
		return fact, nil
	}
	return model.Event{}, errors.New("enterprise retirement requires the exact active Binding")
}

func cloneEnterpriseOperationSnapshot(snapshot EnterpriseRoomSnapshot) EnterpriseRoomSnapshot {
	copy := snapshot
	copy.Events = make([]model.Event, len(snapshot.Events))
	for i, event := range snapshot.Events {
		copy.Events[i] = event
		copy.Events[i].Data = append(json.RawMessage(nil), event.Data...)
	}
	return copy
}

func enterpriseOperationView(snapshot EnterpriseRoomSnapshot) (*Engine, error) {
	if err := validateEnterpriseSnapshot(snapshot.RoomID, snapshot.DeploymentID, snapshot); err != nil {
		return nil, err
	}
	view := emptyEngine(Config{RoomID: snapshot.RoomID, Lease: DeliveryLease})
	view.enterprise = true
	if err := view.replaySnapshotLocked(snapshot); err != nil {
		return nil, err
	}
	return view, nil
}
