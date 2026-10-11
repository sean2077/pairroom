package relay

import (
	"errors"
	"time"

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
	if !ok || candidate.Target != target || !candidate.Enabled || !candidate.QueueStart || candidate.SessionID == "" || !candidate.Runtime.NativeWakePolicy().Transport.Supported() || candidate.WaiterActive || candidate.Delivering || candidate.NudgePending {
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
	return e.recordWakeLocked(outcome, reason, target)
}

// RecordWakeAttempt completes only an already reserved effect. Drain rejects
// new work, but must not discard an outcome known by its admitted worker.
func (e *Engine) RecordWakeAttempt(messageID, outcome, reason string, target model.ActorID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.available(); err != nil {
		return err
	}
	m, ok := e.messages[messageID]
	if !ok || !e.wakeReserved[messageID] || m.To != target || outcome == "suppressed" {
		return errors.New("wake outcome requires a matching reserved effect")
	}
	return e.recordWakeLocked(outcome, reason, target)
}

func (e *Engine) recordWakeLocked(outcome, reason string, target model.ActorID) error {
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
	candidate.Runtime = e.wakeRuntimeLocked(m.To)
	if b := e.bindings[m.To]; b.Active && b.Generation == m.TargetGeneration {
		candidate.SessionID = b.SessionID
		candidate.Remote = b.RemoteKey != ""
		candidate.BindID = b.BindID
		candidate.Generation = b.Generation
	}
	owner, _ := e.wakeOwnerLocked(m.To)
	candidate.QueueStart = owner == m.ID
	candidate.Delivering = e.counts[m.To].Delivering > 0
	candidate.Reserved = e.wakeReserved[m.ID]
	candidate.WaiterActive = e.waiters[m.To] > 0
	candidate.NudgePending = candidate.Runtime.NativeWakePolicy().Transport.Supported() && e.wakeNudgePendingLocked(m.To)
	return candidate, true
}

// Only the immutable Room selection identifies the runtime. Binding.Runtime is
// an inspection projection, not a durable runtime source. An absent/unknown
// selection never gains a slot default or a vendor wake capability.
func (e *Engine) wakeRuntimeLocked(slot model.ActorID) model.RuntimeKind {
	return e.cfg.Runtimes[slot].Canonical()
}

// wakeNudge is the newest reserved nudge for one target. Settled means its
// outcome was recorded; Failed means that outcome proved no delivery. A
// reservation without an outcome may have reached the native queue.
type wakeNudge struct {
	MessageID   string
	At          time.Time
	Consumption model.NativeWakeConsumption
	MidTurn     bool
	Settled     bool
	Failed      bool
	HandedOff   bool
}

// noteWakeNudgeLocked starts outstanding-nudge tracking at a reservation. Claude
// can make progress within a Turn, but an ordinary relay call says nothing
// about its inbox: only an acknowledged collection supersedes the wake need.
// Codex holds a busy target's queue until Turn end; replay has no Turn-end
// observations and keeps that stricter rule.
func (e *Engine) noteWakeNudgeLocked(r WakeReservation) {
	policy := e.wakeRuntimeLocked(r.Target).NativeWakePolicy()
	if !policy.Transport.Supported() {
		return
	}
	midTurn := false
	if policy.Consumption == model.NativeWakeAfterTurnEnd {
		ended := e.turnEnded[r.Target]
		midTurn = ended.IsZero() || e.bindings[r.Target].LastActivity.After(ended)
	}
	e.wakeNudges[r.Target] = wakeNudge{MessageID: r.MessageID, At: r.At, Consumption: policy.Consumption, MidTurn: midTurn}
}

// observeWakeHandoffLocked derives progress from the same durable message fact
// produced by receipt-matched Ack. The input must have been claimed strictly
// after this reservation by the current binding generation. A late/duplicate
// acknowledgement of an older claim cannot release a newer outstanding wake.
// This supersedes the wake need, not the physical native inbox entry: a held
// nudge may still arrive later. Replay reconstructs this observation from facts.
func (e *Engine) observeWakeHandoffLocked(m Message) {
	n, ok := e.wakeNudges[m.To]
	b := e.bindings[m.To]
	if !ok || n.Consumption != model.NativeWakeAfterHandoff || m.State != "handed_off" || !b.Active || m.TargetGeneration != b.Generation || !m.ClaimedAt.After(n.At) {
		return
	}
	n.HandedOff = true
	e.wakeNudges[m.To] = n
}

// observeTurnEndLocked records an authenticated Turn end. A wake already
// released by an earlier progress/consumption observation is settled first,
// so a later Turn end without activity cannot make it outstanding again.
func (e *Engine) observeTurnEndLocked(slot model.ActorID) {
	e.initIndexes()
	if n, ok := e.wakeNudges[slot]; ok && e.wakeNudgeConsumedLocked(slot, n) {
		delete(e.wakeNudges, slot)
	}
	e.turnEnded[slot] = e.cfg.Now()
}

// wakeNudgeConsumedLocked applies the runtime's observation, not a vendor
// acknowledgement. Claude requires acknowledged inbox progress; an idle Codex
// target uses a later relay call, and a busy Codex target requires a call after
// a later Turn end. Management cancellation/inspection updates neither signal.
func (e *Engine) wakeNudgeConsumedLocked(slot model.ActorID, n wakeNudge) bool {
	activity := e.bindings[slot].LastActivity
	switch n.Consumption {
	case model.NativeWakeAfterHandoff:
		return n.HandedOff
	case model.NativeWakeAfterTurnEnd:
		// A declared Turn-end boundary may still observe an idle Codex target.
	default:
		return false
	}
	if !n.MidTurn {
		return activity.After(n.At)
	}
	ended := e.turnEnded[slot]
	return ended.After(n.At) && activity.After(ended)
}

// wakeNudgePendingLocked reports whether the target's newest possibly delivered
// nudge remains outstanding independently of its FIFO message. A new burst for
// that target is then not nudged again; its head stays unattempted. The state
// expires WakeRenewAfter after the reservation, so a deleted nudge, missing
// Stop hook or crashed session cannot strand queued input.
func (e *Engine) wakeNudgePendingLocked(slot model.ActorID) bool {
	n, ok := e.wakeNudges[slot]
	if !ok || n.Failed || !e.cfg.Now().Before(n.At.Add(WakeRenewAfter)) {
		return false
	}
	return !e.wakeNudgeConsumedLocked(slot, n)
}

// WakeRenewAfter is how long an outstanding wake suppresses a fresh head in
// the absence of a qualifying progress observation. A wake can be accepted
// without producing a native turn (a stale inbox socket, a held inbound policy, or a
// nudge dropped by the vendor); without renewal every later message would sit
// behind that head forever. The delay keeps a busy target that will still
// consume the first nudge from collecting a second one immediately.
const WakeRenewAfter = 10 * time.Minute

// wakeOwnerLocked returns the queued message that owns the target's next wake
// decision: the queue head when no queued message has an attempted wake, or the
// oldest message queued after the newest attempted one. Outstanding-nudge and
// rate suppression defer that unattempted owner; an old reservation must not
// hide it from maintenance, auditing or the runtime lease. The attempted ID is
// never offered again and remains available only for diagnostics.
func (e *Engine) wakeOwnerLocked(slot model.ActorID) (owner, attempted string) {
	ids := e.queued[slot]
	last := -1
	for i, id := range ids {
		if e.wakeReserved[id] {
			last = i
		}
	}
	if last < 0 {
		if len(ids) == 0 {
			return "", ""
		}
		return ids[0], ""
	}
	attempted = ids[last]
	if last+1 >= len(ids) {
		return "", attempted
	}
	return ids[last+1], attempted
}

// wakeHeadLocked is the candidate maintenance and diagnostics inspect: the
// current owner, else the newest attempted message still waiting.
func (e *Engine) wakeHeadLocked(slot model.ActorID) string {
	owner, attempted := e.wakeOwnerLocked(slot)
	if owner != "" {
		return owner
	}
	return attempted
}

// WakeHeads examines at most one candidate per slot, regardless of terminal
// history. Attempted heads remain visible to diagnostics but never earn an
// automatic retry; only input queued behind them can start a new burst.
func (e *Engine) WakeHeads() []WakeCandidate {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([]WakeCandidate, 0, 2)
	for _, slot := range model.SlotActors() {
		if id := e.wakeHeadLocked(slot); id != "" {
			if candidate, ok := e.wakeCandidateLocked(id); ok {
				result = append(result, candidate)
			}
		}
	}
	return result
}

// HasWakeWork replays a suspended Room's events read-only and reports whether
// wake is enabled and a slot bound to an active session has queued input that
// has not yet had a wake attempt. This startup probe does not require an
// external wake transport: an active Runtime also reports waiting input for
// Grok and Gemini. It opens no writer and applies no restore transition (a
// recovered delivering message is not turned unknown here). After activation,
// the waker still applies runtime capability, suppression, reservation and rate
// rules; this predicate does not grant an external effect or an idle lease.
func HasWakeWork(roomID string, events []model.Event, runtimes map[model.ActorID]model.RuntimeKind) (bool, error) {
	return HasWakeWorkWithSharedSlot(roomID, events, runtimes, "")
}

func HasWakeWorkWithSharedSlot(roomID string, events []model.Event, runtimes map[model.ActorID]model.RuntimeKind, sharedSlot model.ActorID) (bool, error) {
	e := &Engine{cfg: Config{RoomID: roomID, Runtimes: runtimes, SharedSlot: sharedSlot}, bindings: map[model.ActorID]bindingFact{}, seenBinds: map[string]bool{}, messages: map[string]Message{}, inFlight: map[string]struct{}{}, sends: map[string]string{}, reports: map[string]Publication{}, lastReport: map[string]uint64{}, wakeEnabled: true, wakeReserved: map[string]bool{}, waiters: map[model.ActorID]int{}}
	for _, ev := range events {
		if ev.RoomID != roomID {
			return false, errors.New("native relay Room identity mismatch")
		}
		if err := e.apply(ev); err != nil {
			return false, err
		}
	}
	if !e.wakeEnabled {
		return false, nil
	}
	for _, slot := range model.SlotActors() {
		b := e.bindings[slot]
		if !b.Active || b.SessionID == "" {
			continue
		}
		// Unattempted queued input for the current generation, including
		// input behind an attempted head that will own a renewal later.
		for _, id := range e.queued[slot] {
			if !e.wakeReserved[id] && e.messages[id].TargetGeneration == b.Generation {
				return true, nil
			}
		}
	}
	return false, nil
}
