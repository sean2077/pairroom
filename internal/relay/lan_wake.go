package relay

import (
	"errors"

	"github.com/sean2077/pairroom/internal/model"
)

func (e *Engine) LANWakeCandidate(a Auth, id string) (*WakeCandidate, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return nil, err
	}
	if id == "" {
		ids := e.queued[a.Slot]
		if len(ids) == 0 {
			return nil, nil
		}
		id = ids[0]
	}
	c, ok := e.wakeCandidateLocked(id)
	if !ok || c.Target != a.Slot {
		return nil, nil
	}
	c.SessionID = ""
	return &c, nil
}
func (e *Engine) ReserveLANWake(a Auth, id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return err
	}
	if e.wakeReserved[id] {
		return ErrWakeReserved
	}
	c, ok := e.wakeCandidateLocked(id)
	if !ok || c.Target != a.Slot || c.Generation != a.Generation || !c.Enabled || !c.QueueStart || !c.Runtime.NativeWakePolicy().Transport.Supported() || c.WaiterActive || c.Delivering || c.NudgePending {
		return ErrWakeIneligible
	}
	return e.append(EventWakeReserved, model.ActorSystem, WakeReservation{MessageID: id, Target: a.Slot})
}
func (e *Engine) RecordLANWake(a Auth, id, outcome, reason string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return err
	}
	if id != "" {
		m, ok := e.messages[id]
		if !ok || m.To != a.Slot || m.TargetGeneration != a.Generation || !e.wakeReserved[id] || outcome == "suppressed" {
			return errors.New("wake outcome requires this generation's reserved message")
		}
	}
	if id == "" {
		if outcome != "suppressed" {
			return errors.New("a LAN wake effect outcome requires its reservation ID")
		}
		return e.recordWakeLocked(outcome, reason, a.Slot)
	}
	if !wakeOutcomes[outcome] || (reason != "" && !wakeReasons[reason]) || (outcome == "accepted" || outcome == "submitted") != (reason == "") {
		return errors.New("invalid LAN wake outcome")
	}
	if prior, ok := e.lanWakeResults[id]; ok {
		if prior == outcome+"/"+reason {
			return nil
		}
		return errors.New("LAN wake outcome already recorded")
	}
	return e.append(EventWakeAttempted, model.ActorSystem, struct {
		MessageID string        `json:"message_id"`
		Outcome   string        `json:"outcome"`
		Reason    string        `json:"reason,omitempty"`
		Target    model.ActorID `json:"target"`
	}{id, outcome, reason, a.Slot})
}

// AuthorizedLANEffect rechecks a certificate membership under the same lock
// as a bounded storage effect. It must not call back into the Engine.
func (e *Engine) AuthorizedLANEffect(a Auth, effect func() error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return err
	}
	return effect()
}
