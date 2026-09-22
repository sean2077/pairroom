package relay

import (
	"errors"

	"github.com/sean2077/pairroom/internal/model"
)

// ErrWakeIneligible means the candidate changed before the durable effect
// boundary. No command was authorized and no reservation was consumed.
var ErrWakeIneligible = errors.New("wake candidate is no longer eligible")

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
	for _, counts := range e.counts {
		if counts.Delivering > 0 || counts.Unknown > 0 {
			return ErrWakeRoomBusy
		}
	}
	return e.append(EventWakeConfig, model.ActorUser, map[string]bool{"enabled": enabled})
}

// ReserveWake durably records the pre-command reservation for one wake
// attempt, keyed by PairRoom transport message ID. Revalidate the message,
// binding generation, policy and collector under the SAME lock as the append.
// A prior WakeCandidate is only an observation, not authorization to run a
// vendor command after cancel/unbind/replace or collection has won the race.
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
	candidate, ok := e.wakeCandidateLocked(messageID)
	if !ok || candidate.Target != target || !candidate.Enabled || !candidate.QueueStart || candidate.SessionID == "" || candidate.WaiterActive || candidate.Delivering {
		return ErrWakeIneligible
	}
	// Replacement cancels queued work for the old generation; the candidate
	// exposes a session only when the message's generation is still active.
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
	// accepted/submitted carry no reason; failed/suppressed must carry one, so the
	// audit vocabulary stays self-consistent on replay.
	if (outcome == "accepted" || outcome == "submitted") != (reason == "") {
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

// WakeCandidate is a point-in-time observation of a queued message and its
// target, not a reservation. A later collector/policy/binding change must be
// checked again by ReserveWake at the durable authorization boundary. A
// collector arriving after that boundary can still cause one redundant wake;
// FIFO ownership, not a wake nudge, decides delivery.
func (e *Engine) WakeCandidate(messageID string) (WakeCandidate, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.wakeCandidateLocked(messageID)
}

func (e *Engine) wakeCandidateLocked(messageID string) (WakeCandidate, bool) {
	if e.healthy() != nil {
		return WakeCandidate{}, false
	}
	m, ok := e.messages[messageID]
	if !ok || m.State != "queued" || !m.To.ValidParticipant() {
		return WakeCandidate{}, false
	}
	candidate := WakeCandidate{MessageID: m.ID, Target: m.To, Enabled: e.wakeEnabled}
	candidate.Runtime = e.cfg.Runtimes[m.To]
	if b := e.bindings[m.To]; b.Active && b.Generation == m.TargetGeneration {
		if candidate.Runtime == "" {
			candidate.Runtime = b.Runtime
		}
		candidate.SessionID = b.SessionID
		candidate.BindID = b.BindID
		candidate.Generation = b.Generation
	}
	ids := e.queued[m.To]
	candidate.QueueStart = len(ids) > 0 && ids[0] == m.ID
	candidate.Delivering = e.counts[m.To].Delivering > 0
	candidate.Reserved = e.wakeReserved[m.ID]
	candidate.WaiterActive = e.waiters[m.To] > 0
	return candidate, true
}

// WakeHeads examines at most two queue heads, regardless of terminal history.
// Reserved heads remain visible to diagnostics but never earn an automatic retry.
func (e *Engine) WakeHeads() []WakeCandidate {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]WakeCandidate, 0, 2)
	for _, slot := range model.SlotActors() {
		if ids := e.queued[slot]; len(ids) > 0 {
			if candidate, ok := e.wakeCandidateLocked(ids[0]); ok {
				result = append(result, candidate)
			}
		}
	}
	return result
}
