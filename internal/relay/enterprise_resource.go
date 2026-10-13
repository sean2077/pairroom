package relay

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

const EnterpriseQueuePendingLimit = 256

var ErrEnterpriseQueueLimit = errors.New("enterprise Room unsettled queue limit reached")

// EnterpriseResourceReservation is a conservative completion allowance,
// not facts to append. SQL supplies its bounded event/index/provenance wrapper
// and measures canonical bytes; no fixed per-message byte estimate is used.
type EnterpriseResourceReservation struct {
	Pending       int
	TerminalFacts []model.Event
}

// EnterpriseResourceReserve replays an authorized immutable snapshot through
// the shared state machine and builds full-content completion candidates with
// the shared Claim, expiry, ACK and Binding retirement constructors. Queued
// work reserves claim + expiry + ACK; delivering work reserves expiry + ACK;
// unknown work reserves ACK. Each current Binding separately reserves one
// retirement fact whose replay derives queued cancellation/delivery unknown.
// These combined alternatives deliberately overreserve a mutually exclusive
// retirement and ACK path so either original settlement remains possible.
func EnterpriseResourceReserve(snapshot EnterpriseRoomSnapshot) (EnterpriseResourceReservation, error) {
	view, err := enterpriseOperationView(snapshot)
	if err != nil {
		return EnterpriseResourceReservation{}, err
	}
	reservation := EnterpriseResourceReservation{}
	// Future mutable timestamps use the largest UTC RFC3339Nano encoding.
	// HTML escaping is the largest JSON expansion of an allowed ASCII byte
	// in receipt/reason IDs, so retain the entire valid 256-byte domain.
	at := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	receipt, reason := strings.Repeat("<", 256), strings.Repeat("<", 256)
	appendCandidate := func(actor model.ActorID, fact messageFact) error {
		data, err := json.Marshal(fact)
		if err != nil {
			return err
		}
		reservation.TerminalFacts = append(reservation.TerminalFacts, model.Event{RoomID: snapshot.RoomID, Kind: EventMessage, Actor: actor, CreatedAt: at, Data: data})
		return nil
	}
	for _, id := range view.unresolved {
		message := cloneDeliveryMessage(view.messages[id])
		switch message.State {
		case "queued":
			reservation.Pending++
			claim := planClaimFact(message, receipt, at)
			if err := appendCandidate(message.To, claim); err != nil {
				return EnterpriseResourceReservation{}, err
			}
			message = claim.Message
		case "delivering", "unknown":
			reservation.Pending++
		default:
			return EnterpriseResourceReservation{}, errors.New("unsettled enterprise index contains terminal work")
		}
		if message.State == "delivering" {
			for _, expiry := range planExpiryFacts([]Message{message}, at, view.cfg.Lease, true) {
				if err := appendCandidate(model.ActorSystem, expiry); err != nil {
					return EnterpriseResourceReservation{}, err
				}
				message = expiry.Message
			}
		}
		if err := appendCandidate(message.To, planAcknowledgementFact(message, at)); err != nil {
			return EnterpriseResourceReservation{}, err
		}
	}
	for _, slot := range model.SlotActors() {
		binding := view.bindings[slot]
		if !binding.Active || binding.enterprise == nil {
			continue
		}
		fact, err := view.enterpriseBindingRetirementFact(binding.BindID, reason, "resource-reserve", at)
		if err != nil {
			return EnterpriseResourceReservation{}, err
		}
		reservation.TerminalFacts = append(reservation.TerminalFacts, fact)
	}
	return reservation, nil
}

func (view *Engine) admitEnterpriseQueuedWork(message Message) error {
	if message.State == "queued" && len(view.unresolved) >= EnterpriseQueuePendingLimit {
		return ErrEnterpriseQueueLimit
	}
	return nil
}
