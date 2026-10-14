package relay

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/review"
)

// PrepareEnterpriseOperation captures only immutable input values. It does
// not check mutable peer/state conditions until the returned Prepare runs,
// after SQL has authorized the caller and recovered any accepted receipt.
func PrepareEnterpriseOperation(snapshot EnterpriseRoomSnapshot, caller EnterpriseOperationCaller, intent EnterpriseOperationIntent, ids EnterpriseOperationIDs) (EnterprisePrepare, error) {
	return PrepareEnterpriseOperationWithAttachments(snapshot, caller, intent, ids, nil)
}

// PrepareEnterpriseOperationWithAttachments accepts metadata resolved from
// the caller's live, fenced Room manifest. It does no filesystem/network work
// and captures an isolated copy so the candidate cannot change after admission.
func PrepareEnterpriseOperationWithAttachments(snapshot EnterpriseRoomSnapshot, caller EnterpriseOperationCaller, intent EnterpriseOperationIntent, ids EnterpriseOperationIDs, attachments []model.Attachment) (EnterprisePrepare, error) {
	if err := ValidateEnterpriseOperationIntent(intent); err != nil {
		return nil, err
	}
	if err := matchEnterpriseAttachmentSelection(intent.AttachmentIDs, attachments); err != nil {
		return nil, err
	}
	media := append([]model.Attachment(nil), attachments...)
	if !validEnterpriseOperationID(caller.ClientID) || !validEnterpriseOperationID(caller.UserID) || caller.Actor != model.ActorUser && !caller.Actor.ValidParticipant() {
		return nil, errors.New("invalid enterprise operation caller")
	}
	if caller.Actor.ValidParticipant() {
		if !validEnterpriseOperationID(caller.BindingID) || caller.Generation == 0 {
			return nil, ErrAuth
		}
	} else if caller.BindingID != "" || caller.Generation != 0 || intent.Kind != EnterpriseOperationSend && intent.Kind != EnterpriseOperationRetry && intent.Kind != EnterpriseOperationCancel {
		return nil, ErrAuth
	}
	copy := cloneEnterpriseOperationSnapshot(snapshot)
	return func(at time.Time) (EnterpriseOperationPlan, error) {
		if at.IsZero() {
			return EnterpriseOperationPlan{}, errors.New("enterprise operation requires a locked timestamp")
		}
		view, err := enterpriseOperationView(copy)
		if err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if caller.Actor.ValidParticipant() {
			binding := view.bindings[caller.Actor]
			if !binding.Active || binding.enterprise == nil || binding.BindID != caller.BindingID || binding.Generation != caller.Generation {
				return EnterpriseOperationPlan{}, ErrAuth
			}
		}
		return view.prepareEnterpriseOperation(caller, intent, ids, media, at)
	}, nil
}

// The view is an isolated replay through the existing apply implementation,
// never an Engine with a recorder Store or a call into a legacy mutator. Only
// the small expiry overlay below is composed; it substitutes candidates made
// by the shared expiry planner and does not reimplement event application.
func (view *Engine) prepareEnterpriseOperation(caller EnterpriseOperationCaller, intent EnterpriseOperationIntent, ids EnterpriseOperationIDs, media []model.Attachment, at time.Time) (EnterpriseOperationPlan, error) {
	plan := EnterpriseOperationPlan{Result: EnterpriseOperationResult{Version: 1, Kind: intent.Kind}}
	appendFact := func(kind string, actor model.ActorID, payload any) error {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		plan.Facts = append(plan.Facts, model.Event{RoomID: view.cfg.RoomID, Kind: kind, Actor: actor, CreatedAt: at, Data: data})
		return nil
	}
	finish := func() (EnterpriseOperationPlan, error) {
		if err := ValidateEnterpriseOperationResult(plan.Result); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		return plan, nil
	}
	if intent.Kind == EnterpriseOperationClaim && intent.ParkEnabled && !view.bindings[caller.Actor].ParkEnabled {
		plan.Result.Empty = true
		return finish()
	}
	updates := map[string]Message{}
	if intent.Kind == EnterpriseOperationClaim || intent.Kind == EnterpriseOperationAck || intent.Kind == EnterpriseOperationRetry {
		inFlight := make([]Message, 0, len(view.inFlight))
		for id := range view.inFlight {
			inFlight = append(inFlight, view.messages[id])
		}
		for _, fact := range planExpiryFacts(inFlight, at, view.cfg.Lease, false) {
			updates[fact.ID] = fact.Message
			if err := appendFact(EventMessage, model.ActorSystem, fact); err != nil {
				return EnterpriseOperationPlan{}, err
			}
		}
	}
	messageAt := func(id string) (Message, bool) {
		if message, ok := updates[id]; ok {
			return message, true
		}
		message, ok := view.messages[id]
		return message, ok
	}
	pendingRetry := func(id string) bool {
		for _, key := range view.unresolved {
			message, _ := messageAt(key)
			if isPendingRetry(message, id) {
				return true
			}
		}
		return false
	}
	switch intent.Kind {
	case EnterpriseOperationSend:
		to := intent.To
		if caller.Actor.ValidParticipant() {
			if to == "" {
				to = model.OtherParticipant(caller.Actor)
			}
			if to != model.ActorUser && to != model.OtherParticipant(caller.Actor) {
				return EnterpriseOperationPlan{}, errors.New("explicit relay targets only the peer or @user")
			}
		}
		if caller.Actor == model.ActorUser && !to.ValidParticipant() {
			return EnterpriseOperationPlan{}, errors.New("select a native target slot")
		}
		guardPeer := caller.Actor.ValidParticipant() && to.ValidParticipant()
		if guardPeer && (intent.ExpectedPeerGeneration == 0 || intent.ExpectedPeerGeneration != view.generation(to)) {
			return EnterpriseOperationPlan{}, ErrPeerChanged
		}
		if !validEnterpriseOperationID(ids.MessageID) {
			return EnterpriseOperationPlan{}, errors.New("enterprise send requires its fixed message ID")
		}
		message := planMessage(view.messagePlanSource(caller.Actor, to), ids.MessageID, at, intent.Text, "send")
		if caller.Actor == model.ActorUser {
			message.Author = EnterpriseHumanAuthor(caller.UserID)
		}
		message.Attachments = append([]model.Attachment(nil), media...)
		if intent.Review != (review.Anchor{}) {
			anchor := intent.Review
			message.Review = &anchor
		}
		if err := view.mergeQuoteAndLimitsLocked(&message, SendRequest{QuoteID: intent.QuoteID}); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		key := enterpriseClientKey(caller.UserID, caller.ClientID)
		if caller.Actor.ValidParticipant() {
			key = bindingKey(caller.BindingID, caller.Generation) + "/send/" + caller.ClientID
		}
		if err := view.validateEnterpriseEnvelope(message); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if err := view.admitEnterpriseQueuedWork(message); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if err := appendFact(EventMessage, caller.Actor, messageFact{Message: message, ClientKey: key}); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		public := cloneMessage(message)
		plan.Result.Message = &public
	case EnterpriseOperationReport:
		decision, err := decidePublication(view.publicationPlanState(caller.Actor), intent.ReportSeq, intent.Text, []uint64{intent.ExpectedPeerGeneration})
		if err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if decision.to != "" && decision.publication.Outcome == "" {
			if !validEnterpriseOperationID(ids.MessageID) {
				return EnterpriseOperationPlan{}, errors.New("enterprise publication requires its fixed message ID")
			}
			message := planMessage(view.messagePlanSource(caller.Actor, decision.to), ids.MessageID, at, intent.Text, "stop")
			if err := view.admitEnterpriseQueuedWork(message); err != nil {
				return EnterpriseOperationPlan{}, err
			}
			decision.publication.Message = &message
		}
		if err := appendFact(decision.kind, caller.Actor, decision.publication); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		public := clonePublication(decision.publication)
		plan.Result.Publication = &public
	case EnterpriseOperationClaim:
		busy := false
		for id := range view.inFlight {
			message, _ := messageAt(id)
			if message.To == caller.Actor && message.State == "delivering" {
				busy = true
			}
		}
		var head *Message
		if queued := view.queued[caller.Actor]; len(queued) > 0 {
			message, _ := messageAt(queued[0])
			head = &message
		}
		candidate := selectClaimMessage(head, busy, caller.Generation)
		if intent.MessageID != "" && (candidate == nil || candidate.ID != intent.MessageID || candidate.TargetGeneration != intent.ExpectedTargetGeneration) {
			return EnterpriseOperationPlan{}, ErrPreparedClaimChanged
		}
		if candidate == nil {
			plan.Result.Empty = true
			return finish()
		}
		if err := view.validateEnterpriseEnvelope(*candidate); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if len(candidate.Attachments) > 0 && intent.MessageID == "" {
			return EnterpriseOperationPlan{}, errors.New("enterprise attachments require a prepared claim")
		}
		if !validEnterpriseOperationID(ids.Receipt) {
			return EnterpriseOperationPlan{}, errors.New("enterprise claim requires its fixed private receipt")
		}
		fact := planClaimFact(*candidate, ids.Receipt, at)
		if err := appendFact(EventMessage, caller.Actor, fact); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if len(candidate.Attachments) > 0 {
			plan.Result.StructuredClaim = &StructuredClaim{ID: candidate.ID, Receipt: ids.Receipt, Message: cloneMessage(*candidate), PeerGeneration: view.peerGenerationLocked(caller.Actor)}
		} else {
			envelope, err := view.envelope(*candidate)
			if err != nil {
				return EnterpriseOperationPlan{}, err
			}
			plan.Result.Claim = &Claim{ID: candidate.ID, Receipt: ids.Receipt, Envelope: envelope, PeerGeneration: view.peerGenerationLocked(caller.Actor)}
		}
	case EnterpriseOperationAck:
		original, exists := messageAt(intent.MessageID)
		decision, err := decideAcknowledgement(original, exists, caller.Actor, caller.Generation, intent.Receipt, pendingRetry(intent.MessageID))
		if err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if decision == acknowledgementHandoff {
			if err := appendFact(EventMessage, caller.Actor, planAcknowledgementFact(original, at)); err != nil {
				return EnterpriseOperationPlan{}, err
			}
		}
		plan.Result.HandedOff = true
	case EnterpriseOperationRetry:
		original, exists := messageAt(intent.MessageID)
		if err := decideRetry(original, exists, true, view.generation(original.To), pendingRetry(intent.MessageID)); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if !validEnterpriseOperationID(ids.MessageID) {
			return EnterpriseOperationPlan{}, errors.New("enterprise retry requires its fixed message ID")
		}
		fact := planRetryFact(original, true, view.generation(original.To), ids.MessageID, at)
		if err := view.validateEnterpriseEnvelope(fact.Message); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if err := view.admitEnterpriseQueuedWork(fact.Message); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if err := appendFact(EventMessage, model.ActorUser, fact); err != nil {
			return EnterpriseOperationPlan{}, err
		}
		public := cloneMessage(fact.Message)
		plan.Result.Message = &public
	case EnterpriseOperationCancel:
		original, exists := messageAt(intent.MessageID)
		changed, err := decideCancellation(original, exists)
		if err != nil {
			return EnterpriseOperationPlan{}, err
		}
		if changed {
			fact := planCancellationFact(original, at)
			original = fact.Message
			if err := appendFact(EventMessage, model.ActorUser, fact); err != nil {
				return EnterpriseOperationPlan{}, err
			}
		}
		public := cloneMessage(original)
		plan.Result.Message = &public
	case EnterpriseOperationPark:
		old, changed := planParkBinding(view.bindings[caller.Actor], intent.ParkEnabled)
		binding := model.EnterpriseBinding{Version: 1, RoomID: view.cfg.RoomID, Slot: old.Slot, BindID: old.BindID, Generation: old.Generation, AgentID: old.enterprise.agentID, Runtime: old.Runtime, Active: true, ParkEnabled: intent.ParkEnabled}
		if changed {
			if err := appendFact(model.EnterpriseBindingUpdatedKind, caller.Actor, binding); err != nil {
				return EnterpriseOperationPlan{}, err
			}
		}
		plan.Result.Binding = &binding
	}
	return finish()
}
