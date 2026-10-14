package relay

import (
	"errors"
	"sort"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// These planners operate only on supplied values. They do not authorize a
// caller, read a clock, allocate an ID, inspect media, append/apply facts or
// change transient collector/wake state. Local retains its existing command
// ordering; a future SQL planner can use the same decisions with its guarded
// projection and atomic persistence boundary.

// cloneDeliveryMessage keeps the private receipt while detaching mutable
// message metadata. cloneMessage intentionally redacts receipts for public
// results, so a transition candidate must restore it explicitly.
func cloneDeliveryMessage(message Message) Message {
	copy := cloneMessage(message)
	copy.Receipt = message.Receipt
	return copy
}

// planExpiryFacts selects the supplied in-flight messages at the one fixed
// observation time. Returned facts follow stable ID order; neither the input
// slice nor its nested metadata is modified or shared with the result.
func planExpiryFacts(inFlight []Message, now time.Time, lease time.Duration, all bool) []messageFact {
	ordered := append([]Message(nil), inFlight...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	var facts []messageFact
	for _, original := range ordered {
		if original.State != "delivering" || !all && now.Before(original.ClaimedAt.Add(lease)) {
			continue
		}
		message := cloneDeliveryMessage(original)
		message.State, message.UpdatedAt = "unknown", now
		facts = append(facts, messageFact{Message: message, Receipt: message.Receipt})
	}
	return facts
}

type acknowledgementDecision uint8

const (
	acknowledgementNoChange acknowledgementDecision = iota
	acknowledgementHandoff
)

// decideAcknowledgement deliberately precedes timestamp materialization: an
// invalid or already-settled receipt must not consume a new clock observation.
// A pending explicit Retry wins only over a late acknowledgement of unknown
// work, matching the existing Local receipt semantics.
func decideAcknowledgement(message Message, exists bool, slot model.ActorID, generation uint64, receipt string, pendingRetry bool) (acknowledgementDecision, error) {
	if !exists || message.To != slot || message.TargetGeneration != generation || receipt == "" || !same(message.Receipt, receipt) {
		return acknowledgementNoChange, ErrAuth
	}
	if message.State == "handed_off" {
		return acknowledgementNoChange, nil
	}
	if message.State == "unknown" {
		if pendingRetry {
			return acknowledgementNoChange, errors.New("an explicit Retry is pending; resolve it before acknowledging the original delivery")
		}
		return acknowledgementHandoff, nil
	}
	if message.State != "delivering" {
		return acknowledgementNoChange, errors.New("delivery outcome is unknown; inspect before explicit Retry")
	}
	return acknowledgementHandoff, nil
}

// planAcknowledgementFact materializes a successful handoff decision using
// the caller's fixed time. The caller must keep the validated message stable
// between decision and materialization.
func planAcknowledgementFact(original Message, now time.Time) messageFact {
	message := cloneDeliveryMessage(original)
	message.State, message.UpdatedAt = "handed_off", now
	return messageFact{Message: message, Receipt: message.Receipt}
}

func isPendingRetry(message Message, originalID string) bool {
	return message.RetryOf == originalID && (message.State == "queued" || message.State == "delivering")
}

// decideRetry checks the original work before the caller allocates the new
// message identity or observes its creation time. Authorization and expiry
// processing remain the caller's responsibility.
func decideRetry(original Message, exists, dynamic bool, currentTargetGeneration uint64, pendingRetry bool) error {
	if !exists || original.State != "unknown" {
		return errors.New("only unknown messages can be explicitly retried")
	}
	if dynamic && original.To.ValidParticipant() && original.TargetGeneration != currentTargetGeneration {
		return ErrPeerChanged
	}
	if pendingRetry {
		return errors.New("a retry is already pending")
	}
	return nil
}

// planRetryFact materializes an accepted retry decision. The caller supplies
// a fresh stable ID and time; neither is generated here. Dynamic retries keep
// their original source/target generations, while legacy routing retains its
// existing current-target rule. A retry is new queued work, never an ACK or a
// continuation of the old claim receipt.
func planRetryFact(original Message, dynamic bool, currentTargetGeneration uint64, id string, now time.Time) messageFact {
	content := cloneDeliveryMessage(original)
	message := Message{ID: id, From: original.From, To: original.To, Text: original.Text,
		State: "queued", TargetGeneration: currentTargetGeneration, CreatedAt: now, UpdatedAt: now, Source: "retry",
		Author: content.Author, Attachments: content.Attachments, Quote: content.Quote, Review: content.Review, RetryOf: original.ID}
	if original.To == model.ActorUser {
		message.State, message.TargetGeneration = "human", 0
	}
	if dynamic {
		message.FromRuntime, message.FromGeneration, message.TargetGeneration = original.FromRuntime, original.FromGeneration, original.TargetGeneration
	}
	return messageFact{Message: message}
}
