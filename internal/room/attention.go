package room

import (
	"encoding/json"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// Attention is a body-free observation that an Embedded Room may need a
// human. Kind uses the same vocabulary as Native attention where the meaning
// matches; Key identifies the fact for deduplication. No message text, tool
// input, approval detail or vendor output is carried.
type Attention struct {
	Kind string
	Slot model.ActorID
	Key  string
}

const (
	AttentionHumanTurn       = "human_turn"
	AttentionApproval        = "approval_requested"
	AttentionAgentFailed     = "agent_failed"
	AttentionStallWarning    = "stall_warning"
	stallNoticeMarker        = "has produced no runtime event for"
	attentionSystemNoticeKey = "notice"
)

// AttentionFromEvent classifies one appended or published Embedded event.
func AttentionFromEvent(ev model.Event) (Attention, bool) {
	switch ev.Kind {
	case EventMessageCreated:
		var m model.Message
		if json.Unmarshal(ev.Data, &m) != nil || !m.From.ValidParticipant() {
			return Attention{}, false
		}
		// An Agent answer addressed only to the human ends relay here.
		for _, to := range m.To {
			if to != model.ActorUser {
				return Attention{}, false
			}
		}
		return Attention{Kind: AttentionHumanTurn, Slot: m.From, Key: m.ID}, true
	case EventApprovalUpdated:
		var a model.Approval
		if json.Unmarshal(ev.Data, &a) == nil && a.Status == "pending" && a.Agent.ValidParticipant() {
			return Attention{Kind: AttentionApproval, Slot: a.Agent, Key: a.ID}, true
		}
	case EventProcessingUpdated:
		var u model.ProcessingUpdate
		if json.Unmarshal(ev.Data, &u) == nil && u.State == model.ProcessingFailed && u.Target.ValidParticipant() {
			return Attention{Kind: AttentionAgentFailed, Slot: u.Target, Key: u.MessageID}, true
		}
	case EventSystemNotice:
		var n model.SystemNotice
		if json.Unmarshal(ev.Data, &n) == nil && n.Level == "warning" && strings.Contains(n.Text, stallNoticeMarker) {
			return Attention{Kind: AttentionStallWarning, Key: attentionSystemNoticeKey + "-" + ev.ID}, true
		}
	}
	return Attention{}, false
}
