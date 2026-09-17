package relay

import (
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
	Active      bool `json:"active"`
	Associated  bool `json:"associated"`
	ParkEnabled bool `json:"park_enabled"`
}

type InboxSummary struct {
	Queued     int `json:"queued"`
	Delivering int `json:"delivering"`
	Unknown    int `json:"unknown"`
}

type RecoveryReference struct {
	ID   string        `json:"id"`
	Slot model.ActorID `json:"slot"`
}

// Summary is intentionally bounded and body-free: no text, attachments,
// transcript paths, native session IDs or historical audit entries. It reports
// transport facts, never whether the native model is working or finished.
type Summary struct {
	HostMode model.HostMode                   `json:"host_mode"`
	RoomID   string                           `json:"room_id"`
	Sequence uint64                           `json:"sequence"`
	Bindings map[model.ActorID]BindingSummary `json:"bindings"`
	Inboxes  map[model.ActorID]InboxSummary   `json:"inboxes"`
	Recovery []RecoveryReference              `json:"recovery,omitempty"`
	Notice   string                           `json:"notice"`
}

func (e *Engine) AuthSummary(a Auth) (Summary, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, true)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{HostMode: model.HostNative, RoomID: e.cfg.RoomID, Sequence: e.sequence,
		Bindings: map[model.ActorID]BindingSummary{}, Inboxes: map[model.ActorID]InboxSummary{},
		Notice: "Transport state only; handed_off is stdout, not model acceptance. Full history is available through status --brief=false."}
	if b.SessionID == "" {
		s.Sequence = 0
		s.Bindings[a.Slot] = BindingSummary{Active: b.Active, ParkEnabled: b.ParkEnabled}
		s.Notice = "Binding is not associated with an official session; rebind inside the native session. No inbox access before association."
		return s, nil
	}
	for slot, b := range e.bindings {
		s.Bindings[slot] = BindingSummary{Active: b.Active, Associated: b.SessionID != "", ParkEnabled: b.ParkEnabled}
	}
	for _, id := range e.order {
		m := e.messages[id]
		if !m.To.ValidParticipant() {
			continue
		}
		counts := s.Inboxes[m.To]
		switch m.State {
		case "queued":
			counts.Queued++
		case "delivering":
			counts.Delivering++
		case "unknown":
			counts.Unknown++
			if len(s.Recovery) < 8 {
				s.Recovery = append(s.Recovery, RecoveryReference{ID: m.ID, Slot: m.To})
			}
		}
		s.Inboxes[m.To] = counts
	}
	return s, nil
}
