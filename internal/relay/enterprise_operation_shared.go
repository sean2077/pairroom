package relay

import (
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
)

type messagePlanSource struct {
	from, to                         model.ActorID
	dynamic                          bool
	runtime                          model.RuntimeKind
	fromGeneration, targetGeneration uint64
}

func (e *Engine) messagePlanSource(from, to model.ActorID) messagePlanSource {
	source := messagePlanSource{from: from, to: to, dynamic: e.cfg.DynamicBindings}
	if to != model.ActorUser {
		source.targetGeneration = e.generation(to)
	}
	if source.dynamic && from.ValidParticipant() {
		source.runtime = e.bindings[from].Runtime
		source.fromGeneration = e.bindings[from].Generation
	}
	return source
}

func planMessage(source messagePlanSource, id string, at time.Time, text, kind string) Message {
	message := Message{ID: id, From: source.from, To: source.to, Text: text, State: "queued", TargetGeneration: source.targetGeneration, CreatedAt: at, UpdatedAt: at, Source: kind}
	if source.to == model.ActorUser {
		message.State = "human"
		message.TargetGeneration = 0
	}
	if source.dynamic && source.from.ValidParticipant() {
		message.FromRuntime = source.runtime
		message.FromGeneration = source.fromGeneration
	}
	return message
}

type publicationPlanState struct {
	actor                                  model.ActorID
	bindID                                 string
	generation, lastReport, peerGeneration uint64
	dynamic                                bool
	runtimes                               map[model.ActorID]model.RuntimeKind
}

type publicationDecision struct {
	kind        string
	to          model.ActorID
	publication Publication
}

func (e *Engine) publicationPlanState(actor model.ActorID) publicationPlanState {
	b := e.bindings[actor]
	return publicationPlanState{actor: actor, bindID: b.BindID, generation: b.Generation, lastReport: e.lastReport[bindingKey(b.BindID, b.Generation)], peerGeneration: e.generation(model.OtherParticipant(actor)), dynamic: e.cfg.DynamicBindings, runtimes: e.cfg.Runtimes}
}

// The accepted-operation receipt is handled by the caller before this new
// publication decision. Private, unaddressed text never enters its result.
func decidePublication(state publicationPlanState, seq uint64, text string, expectedPeer []uint64) (publicationDecision, error) {
	if len(expectedPeer) > 1 {
		return publicationDecision{}, errors.New("report accepts at most one expected peer generation")
	}
	if seq <= state.lastReport {
		return publicationDecision{}, errors.New("report_seq regressed; reconcile the original publication")
	}
	decision := publicationDecision{kind: EventPublication, publication: Publication{BindID: state.bindID, Generation: state.generation, ReportSeq: seq}}
	parsed := prompt.ParseMentions(text, state.actor, state.runtimes)
	if state.dynamic {
		if len(expectedPeer) == 1 {
			decision.publication.ExpectedPeerGeneration = expectedPeer[0]
		}
		parsed = prompt.ParseNativeMentions(text, state.actor)
	}
	if len(parsed.Targets) > 0 {
		decision.to = model.OtherParticipant(state.actor)
	} else if parsed.Human {
		decision.to = model.ActorUser
	}
	if state.dynamic && decision.to.ValidParticipant() && (decision.publication.ExpectedPeerGeneration == 0 || decision.publication.ExpectedPeerGeneration != state.peerGeneration) {
		decision.publication.Outcome = PeerChangedCode
	}
	if seq > state.lastReport+1 {
		decision.publication.GapFrom = state.lastReport + 1
		decision.publication.GapTo = seq - 1
		decision.kind = EventPublicationGap
	}
	return decision, nil
}

func selectClaimMessage(head *Message, busy bool, generation uint64) *Message {
	if busy || head == nil || head.TargetGeneration != generation {
		return nil
	}
	copy := cloneDeliveryMessage(*head)
	return &copy
}

func planClaimFact(original Message, receipt string, at time.Time) messageFact {
	message := cloneDeliveryMessage(original)
	message.State, message.Receipt, message.ClaimedAt, message.UpdatedAt = "delivering", receipt, at, at
	return messageFact{Message: message, Receipt: receipt}
}

func decideCancellation(message Message, exists bool) (bool, error) {
	if !exists {
		return false, errors.New("message not found")
	}
	if message.State == "cancelled" {
		return false, nil
	}
	if message.State != "queued" {
		return false, errors.New("Cancel only removes queued native messages; it cannot stop native work")
	}
	return true, nil
}

func planCancellationFact(original Message, at time.Time) messageFact {
	message := cloneDeliveryMessage(original)
	message.State, message.UpdatedAt = "cancelled", at
	return messageFact{Message: message}
}

func planParkBinding(binding bindingFact, enabled bool) (bindingFact, bool) {
	if binding.ParkEnabled == enabled {
		return binding, false
	}
	binding.ParkEnabled = enabled
	return binding, true
}

func planLocalRetirement(binding bindingFact, at time.Time) bindingFact {
	binding.Active = false
	binding.CredentialHash, binding.SessionID, binding.TranscriptPath = "", "", ""
	binding.LastActivity = at
	return binding
}
