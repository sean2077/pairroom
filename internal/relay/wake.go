package relay

import (
	"errors"

	"github.com/sean2077/pairroom/internal/model"
)

// WakeEnabled reports the per-Room automatic-wake configuration. Absence of a
// durable native.wake.updated fact means enabled (per-Room default-on, DP2).
func (e *Engine) WakeEnabled() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.wakeEnabled
}

// SetWakeEnabled changes the per-Room automatic-wake configuration only at an
// idle Room boundary, mirroring the permission-profile rule: unresolved
// delivering/unknown deliveries must be reconciled first. The change is
// idempotent and durable; replay restores the last recorded configuration.
func (e *Engine) SetWakeEnabled(enabled bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	if e.wakeEnabled == enabled {
		return nil
	}
	for _, id := range e.order {
		switch e.messages[id].State {
		case "delivering", "unknown":
			return ErrWakeRoomBusy
		}
	}
	return e.append(EventWakeConfig, model.ActorUser, map[string]bool{"enabled": enabled})
}

// ReserveWake durably records the pre-command reservation for one wake
// attempt, keyed by PairRoom transport message ID. The reservation must exist
// before any vendor command runs, so an interrupted Service never mistakes an
// attempted vendor effect for a safe automatic retry. Reserving the same
// message twice fails with ErrWakeReserved instead of overwriting history.
func (e *Engine) ReserveWake(messageID string, target model.ActorID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	if !validID(messageID) || !target.ValidParticipant() {
		return errors.New("invalid wake reservation request")
	}
	if e.wakeReserved[messageID] {
		return ErrWakeReserved
	}
	return e.append(EventWakeReserved, model.ActorSystem, WakeReservation{MessageID: messageID, Target: target})
}

// RecordWake durably records the redacted outcome of one wake attempt. The
// outcome and reason must come from the fixed vocabulary; anything else is
// rejected so vendor thread identity, message bodies, and command output can
// never reach the Event Log through this surface.
func (e *Engine) RecordWake(outcome, reason string, target model.ActorID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.healthy(); err != nil {
		return err
	}
	if !wakeOutcomes[outcome] || !target.ValidParticipant() {
		return errors.New("invalid wake outcome")
	}
	if reason != "" && !wakeReasons[reason] {
		return errors.New("invalid wake reason")
	}
	// accepted carries no reason; failed/suppressed must carry one, so the
	// audit vocabulary stays self-consistent on replay.
	if (outcome == "accepted") != (reason == "") {
		return errors.New("wake reason does not match outcome")
	}
	payload := struct {
		Outcome string        `json:"outcome"`
		Reason  string        `json:"reason,omitempty"`
		Target  model.ActorID `json:"target"`
	}{Outcome: outcome, Reason: reason, Target: target}
	return e.append(EventWakeAttempted, model.ActorSystem, payload)
}

// WakeReservations returns the durable reservation history in append order.
// The Service injects it when a waker activates so rate limits and the
// no-retry rule survive runtime suspend/restart without persisting vendor
// metadata anywhere.
func (e *Engine) WakeReservations() []WakeReservation {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]WakeReservation(nil), e.wakeReservations...)
}

// WakeCandidate answers atomically, under the Engine lock, whether a durable
// queued message should consider waking its target: the message must still be
// queued for a participant, and the result reports whether it began a fresh
// pending burst, whether a foreground/park collector is blocked in Claim for
// the target right now, and whether an unacknowledged delivery to the target
// is in flight. ok=false means there is nothing to evaluate (unknown,
// already claimed, cancelled, user-directed, or unavailable engine); the
// caller skips silently. Policy stays in the waker; the Engine only states
// facts. The answer is a point-in-time observation: a collector can attach
// immediately afterwards, in which case single-owner FIFO claim still
// delivers the message exactly once and the wake degenerates to at most one
// redundant vendor turn.
func (e *Engine) WakeCandidate(messageID string) (WakeCandidate, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fatal != nil || e.closed {
		return WakeCandidate{}, false
	}
	m, ok := e.messages[messageID]
	if !ok || m.State != "queued" || !m.To.ValidParticipant() {
		return WakeCandidate{}, false
	}
	candidate := WakeCandidate{MessageID: m.ID, Target: m.To, Enabled: e.wakeEnabled}
	candidate.Runtime = e.cfg.Runtimes[m.To]
	if b := e.bindings[m.To]; b.Active {
		if candidate.Runtime == "" {
			candidate.Runtime = b.Runtime
		}
		candidate.SessionID = b.SessionID
	}
	firstQueued := ""
	for _, id := range e.order {
		other := e.messages[id]
		if other.To != m.To {
			continue
		}
		switch other.State {
		case "queued":
			if firstQueued == "" {
				firstQueued = id
			}
		case "delivering":
			candidate.Delivering = true
		}
	}
	candidate.QueueStart = firstQueued == m.ID
	candidate.WaiterActive = e.waiters[m.To] > 0
	return candidate, true
}
