package relay

import (
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// CheckAuth is a non-persisting admission gate for an already-active Runtime. The
// effect still reauthenticates under the Engine lock, including after rebind.
// Completion receipts can pass this gate while the Runtime is draining.
func (e *Engine) CheckAuth(a Auth) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.authenticate(a, true)
	return err
}

// Sequence reads the SSE cursor without cloning the transcript and audit log.
func (e *Engine) Sequence() uint64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.sequence
}

type BindingSummary struct {
	Active          bool      `json:"active"`
	Associated      bool      `json:"associated"`
	ParkEnabled     bool      `json:"park_enabled"`
	CollectorActive bool      `json:"collector_active"`
	LastActivity    time.Time `json:"last_activity,omitempty"`
}

type InboxSummary struct {
	Queued         int        `json:"queued"`
	Delivering     int        `json:"delivering"`
	Unknown        int        `json:"unknown"`
	OldestQueuedAt *time.Time `json:"oldest_queued_at,omitempty"`
}

type RecoveryReference struct {
	ID   string        `json:"id"`
	Slot model.ActorID `json:"slot"`
}

// Summary is intentionally bounded and body-free: no text, attachments,
// transcript paths, native session IDs or historical audit entries. It reports
// transport facts, never whether the native model is working or finished.
type Summary struct {
	LastUserMessage string                            `json:"last_user_message,omitempty"`
	HostMode        model.HostMode                    `json:"host_mode"`
	RoomID          string                            `json:"room_id"`
	Sequence        uint64                            `json:"sequence"`
	WakeEnabled     bool                              `json:"wake_enabled"`
	Bindings        map[model.ActorID]BindingSummary  `json:"bindings"`
	Inboxes         map[model.ActorID]InboxSummary    `json:"inboxes"`
	Recovery        []RecoveryReference               `json:"recovery,omitempty"`
	LastWake        map[model.ActorID]WakeObservation `json:"last_wake"`
	Notice          string                            `json:"notice"`
}

type WakeObservation struct {
	Outcome string    `json:"outcome"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"at"`
}

func (e *Engine) Summary() Summary {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.summaryLocked()
}

func (e *Engine) AuthSummary(a Auth) (Summary, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, true)
	if err != nil {
		return Summary{}, err
	}
	if b.SessionID == "" {
		// A replayed pre-upgrade binding has no confirmed official session yet, so
		// it keeps the documented body-free projection: no inbox counts, recovery
		// IDs, wake state or sequence, and no inbox access before association.
		return Summary{HostMode: model.HostNative, RoomID: e.cfg.RoomID, WakeEnabled: e.wakeEnabled,
			Bindings: map[model.ActorID]BindingSummary{a.Slot: {Active: b.Active, ParkEnabled: b.ParkEnabled}},
			Inboxes:  map[model.ActorID]InboxSummary{}, LastWake: map[model.ActorID]WakeObservation{},
			Notice: "Binding is not associated with an official session; rebind inside the native session. No inbox access before association."}, nil
	}
	return e.summaryLocked(), nil
}

func (e *Engine) summaryLocked() Summary {
	s := Summary{LastUserMessage: e.lastUserMessage, HostMode: model.HostNative, RoomID: e.cfg.RoomID, Sequence: e.sequence, WakeEnabled: e.wakeEnabled,
		Bindings: map[model.ActorID]BindingSummary{}, Inboxes: map[model.ActorID]InboxSummary{}, LastWake: map[model.ActorID]WakeObservation{},
		Notice: "Transport state only; handed_off is stdout, not model acceptance. Inspect one message with relay history --id; --pending pages unresolved work without replay."}

	for slot, b := range e.bindings {
		s.Bindings[slot] = BindingSummary{Active: b.Active, Associated: b.SessionID != "", ParkEnabled: b.ParkEnabled, CollectorActive: e.waiters[slot] > 0, LastActivity: b.LastActivity}
	}
	for _, slot := range model.SlotActors() {
		counts := e.counts[slot]
		if ids := e.queued[slot]; len(ids) > 0 {
			t := e.messages[ids[0]].CreatedAt
			counts.OldestQueuedAt = &t
		}
		s.Inboxes[slot] = counts
	}
	for slot, observation := range e.lastWake {
		s.LastWake[slot] = observation
	}
	for _, id := range e.unresolved {
		m := e.messages[id]
		if m.State == "unknown" {
			s.Recovery = append(s.Recovery, RecoveryReference{ID: id, Slot: m.To})
			if len(s.Recovery) == 8 {
				break
			}
		}
	}
	return s
}

// InspectTransport observes the two bindings/heads and summary at one sequence.
// Binding credentials stay excluded; session identifiers are internal only.
func (e *Engine) InspectTransport() (Summary, map[model.ActorID]Binding, []WakeCandidate) {
	e.mu.Lock()
	defer e.mu.Unlock()
	bindings := map[model.ActorID]Binding{}
	heads := []WakeCandidate{}
	for _, slot := range model.SlotActors() {
		bindings[slot] = e.bindings[slot].Binding
		if ids := e.queued[slot]; len(ids) > 0 {
			if c, ok := e.wakeCandidateLocked(ids[0]); ok {
				heads = append(heads, c)
			}
		}
	}
	return e.summaryLocked(), bindings, heads
}
