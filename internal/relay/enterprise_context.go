package relay

import (
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

// EnterpriseContext is an authorized, body-free transport view. Reading it
// neither claims input nor changes the automatic publication context.
type EnterpriseContext struct {
	Revision       uint64              `json:"revision"`
	Summary        Summary             `json:"summary"`
	Binding        Binding             `json:"binding"`
	Peer           Binding             `json:"peer"`
	PeerGeneration uint64              `json:"peer_generation"`
	Bootstrap      string              `json:"bootstrap"`
	Collaboration  string              `json:"collaboration"`
	ReplyExpected  bool                `json:"reply_expected"`
	ClaimReady     bool                `json:"claim_ready"`
	Next           *EnterpriseIncoming `json:"next,omitempty"`
}

func DescribeEnterpriseSnapshot(snapshot EnterpriseRoomSnapshot, slot model.ActorID, bindingID string, generation uint64, at time.Time) (EnterpriseContext, error) {
	if !slot.ValidParticipant() || at.IsZero() {
		return EnterpriseContext{}, ErrAuth
	}
	if err := validateEnterpriseSnapshot(snapshot.RoomID, snapshot.DeploymentID, snapshot); err != nil {
		return EnterpriseContext{}, err
	}
	view := emptyEngine(Config{RoomID: snapshot.RoomID, Now: func() time.Time { return at }})
	view.enterprise = true
	if err := view.replaySnapshotLocked(snapshot); err != nil {
		return EnterpriseContext{}, err
	}
	bound := view.bindings[slot].Binding
	if !bound.Active || bound.BindID != bindingID || bound.Generation != generation {
		return EnterpriseContext{}, ErrAuth
	}
	bootstrap, peerGeneration, err := view.BootstrapContextForBinding(bound)
	if err != nil {
		return EnterpriseContext{}, err
	}
	creation, err := model.DecodeEnterpriseRoomCreation(snapshot.Events[0])
	if err != nil {
		return EnterpriseContext{}, err
	}
	peerSlot := model.OtherParticipant(slot)
	peer := view.bindings[peerSlot].Binding
	if peer.Slot == "" {
		peer.Slot, peer.Runtime = peerSlot, view.cfg.Runtimes[peerSlot]
	}
	return EnterpriseContext{Revision: snapshot.Revision, Summary: view.Summary(), Binding: bound, Peer: peer, PeerGeneration: peerGeneration, Bootstrap: bootstrap, Collaboration: protocol.CollaborationInstructions(slot, creation.Collaboration), ReplyExpected: view.replyExpectedLocked(slot), ClaimReady: enterpriseContextClaimReady(view, slot, generation, at), Next: enterpriseContextIncoming(view, slot, generation, at)}, nil
}

func enterpriseContextIncoming(view *Engine, slot model.ActorID, generation uint64, at time.Time) *EnterpriseIncoming {
	for id := range view.inFlight {
		message := view.messages[id]
		if message.To == slot && message.State == "delivering" && at.Before(message.ClaimedAt.Add(DeliveryLease)) {
			return nil
		}
	}
	queued := view.queued[slot]
	if len(queued) == 0 {
		return nil
	}
	head := view.messages[queued[0]]
	if head.TargetGeneration != generation || len(head.Attachments) == 0 {
		return nil
	}
	return &EnterpriseIncoming{MessageID: head.ID, TargetGeneration: head.TargetGeneration, Attachments: append([]model.Attachment(nil), head.Attachments...)}
}

func enterpriseContextClaimReady(view *Engine, slot model.ActorID, generation uint64, at time.Time) bool {
	inFlight := make([]Message, 0, len(view.inFlight))
	for id := range view.inFlight {
		inFlight = append(inFlight, view.messages[id])
	}
	// Expiry remains a persisted transition in the subsequent atomic Claim,
	// not a mutation by this peek. An expired delivery must not make all
	// collectors wait forever for a delivering count that only Claim can reap.
	if len(planExpiryFacts(inFlight, at, DeliveryLease, false)) > 0 {
		return true
	}
	var head *Message
	if queued := view.queued[slot]; len(queued) > 0 {
		message := view.messages[queued[0]]
		head = &message
	}
	return selectClaimMessage(head, view.counts[slot].Delivering > 0, generation) != nil
}
