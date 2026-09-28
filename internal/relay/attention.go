package relay

import (
	"encoding/json"

	"github.com/sean2077/pairroom/internal/model"
)

// Attention is a body-free observation that a Native Room may need a human.
// Kind comes from a fixed vocabulary; Key identifies the fact (message ID or
// event sequence) for deduplication. No message text, session identity,
// receipt or vendor output is carried.
type Attention struct {
	Kind string
	Slot model.ActorID
	Key  string
}

// Attention kinds shared with Embedded Rooms where the meaning matches.
const (
	AttentionHumanTurn         = "human_turn"
	AttentionDeliveryUncertain = "delivery_uncertain"
	AttentionAgentFailed       = "agent_failed"
	AttentionWakeFailed        = "wake_failed"
	AttentionInputWaiting      = "input_waiting"
)

// AttentionFromEvent classifies one appended Native fact. It reads only
// states, targets and fixed outcome fields.
func AttentionFromEvent(ev model.Event) (Attention, bool) {
	switch ev.Kind {
	case EventPublication, EventPublicationGap:
		var p Publication
		if json.Unmarshal(ev.Data, &p) != nil || p.Message == nil {
			return Attention{}, false
		}
		if p.Message.To == model.ActorUser && p.Message.From.ValidParticipant() {
			return Attention{Kind: AttentionHumanTurn, Slot: p.Message.From, Key: p.Message.ID}, true
		}
	case EventMessage:
		var m messageFact
		if json.Unmarshal(ev.Data, &m) != nil {
			return Attention{}, false
		}
		switch {
		case m.State == "human" && m.From.ValidParticipant():
			return Attention{Kind: AttentionHumanTurn, Slot: m.From, Key: m.ID}, true
		case m.State == "unknown" && m.To.ValidParticipant():
			return Attention{Kind: AttentionDeliveryUncertain, Slot: m.To, Key: m.ID}, true
		}
	case EventFailure:
		if ev.Actor.ValidParticipant() {
			return Attention{Kind: AttentionAgentFailed, Slot: ev.Actor, Key: ev.ID}, true
		}
	case EventWakeAttempted:
		var p struct {
			Outcome string        `json:"outcome"`
			Target  model.ActorID `json:"target"`
		}
		if json.Unmarshal(ev.Data, &p) == nil && p.Outcome == "failed" && p.Target.ValidParticipant() {
			return Attention{Kind: AttentionWakeFailed, Slot: p.Target, Key: ev.ID}, true
		}
	}
	return Attention{}, false
}
