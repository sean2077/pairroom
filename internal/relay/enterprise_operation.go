package relay

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

type EnterpriseOperationKind string

const (
	EnterpriseOperationSend   EnterpriseOperationKind = "send"
	EnterpriseOperationReport EnterpriseOperationKind = "report"
	EnterpriseOperationClaim  EnterpriseOperationKind = "claim"
	EnterpriseOperationAck    EnterpriseOperationKind = "ack"
	EnterpriseOperationRetry  EnterpriseOperationKind = "retry"
	EnterpriseOperationCancel EnterpriseOperationKind = "cancel"
	EnterpriseOperationPark   EnterpriseOperationKind = "park"
)

// EnterpriseOperationIntent is the closed text-only Relay operation intent.
// Room, operation identity and authenticated scope belong to the SQL command,
// not this value. Neither this type nor a Plan is a raw-event upload surface.
type EnterpriseOperationIntent struct {
	Kind                   EnterpriseOperationKind `json:"kind"`
	To                     model.ActorID           `json:"to,omitempty"`
	Text                   string                  `json:"text,omitempty"`
	ReportSeq              uint64                  `json:"report_seq,omitempty"`
	ExpectedPeerGeneration uint64                  `json:"expected_peer_generation,omitempty"`
	MessageID              string                  `json:"message_id,omitempty"`
	Receipt                string                  `json:"receipt,omitempty"`
	ParkEnabled            bool                    `json:"park_enabled,omitempty"`
}

// EnterpriseOperationCaller is built only from the SQL runner's fenced and
// authenticated scope. It is never reconstructed from request-body authority.
type EnterpriseOperationCaller struct {
	Actor      model.ActorID
	UserID     string
	BindingID  string
	Generation uint64
	ClientID   string
}

type EnterpriseOperationIDs struct {
	MessageID string
	Receipt   string
}

// EnterpriseOperationResult is the complete original accepted result. Claim
// intentionally contains a private receipt and envelope: only the authorized
// collector may receive it. Browser mutation replies must not serialize this
// result indiscriminately. In particular it is not a ControlReceipt.
type EnterpriseOperationResult struct {
	Version     int                      `json:"version"`
	Kind        EnterpriseOperationKind  `json:"kind"`
	Message     *Message                 `json:"message,omitempty"`
	Publication *Publication             `json:"publication,omitempty"`
	Claim       *Claim                   `json:"claim,omitempty"`
	Binding     *model.EnterpriseBinding `json:"binding,omitempty"`
	HandedOff   bool                     `json:"handed_off,omitempty"`
	Empty       bool                     `json:"empty,omitempty"`
}

// EnterpriseOperationPlan has zero or more ordered candidates. Candidate Seq
// is zero; SQL assigns the contiguous range and generates an Event ID if empty.
// A zero-fact result still needs its own accepted operation receipt.
type EnterpriseOperationPlan struct {
	Facts  []model.Event
	Result EnterpriseOperationResult
}

// EnterprisePrepare may run only after live authorization, accepted-operation
// lookup and revision CAS. Its fixed time is observed after transaction locks;
// the function performs no effects, clock reads or random identity generation.
type EnterprisePrepare func(lockedAt time.Time) (EnterpriseOperationPlan, error)

func ValidateEnterpriseOperationIntent(intent EnterpriseOperationIntent) error {
	rest := intent
	rest.Kind = ""
	switch intent.Kind {
	case EnterpriseOperationSend:
		if intent.To != "" && intent.To != model.ActorUser && !intent.To.ValidParticipant() || validateBody(intent.Text) != nil || strings.TrimSpace(intent.Text) == "" {
			return errors.New("invalid enterprise send intent")
		}
		rest.To, rest.Text, rest.ExpectedPeerGeneration = "", "", 0
	case EnterpriseOperationReport:
		if intent.ReportSeq == 0 || validateBody(intent.Text) != nil {
			return errors.New("invalid enterprise report intent")
		}
		rest.ReportSeq, rest.Text, rest.ExpectedPeerGeneration = 0, "", 0
	case EnterpriseOperationClaim:
		rest.ParkEnabled = false
	case EnterpriseOperationAck:
		if !validEnterpriseOperationID(intent.MessageID) || !validEnterpriseOperationID(intent.Receipt) {
			return errors.New("invalid enterprise acknowledgement intent")
		}
		rest.MessageID, rest.Receipt = "", ""
	case EnterpriseOperationRetry, EnterpriseOperationCancel:
		if !validEnterpriseOperationID(intent.MessageID) {
			return errors.New("invalid enterprise message control intent")
		}
		rest.MessageID = ""
	case EnterpriseOperationPark:
		rest.ParkEnabled = false
	default:
		return errors.New("unsupported enterprise Relay operation")
	}
	if rest != (EnterpriseOperationIntent{}) {
		return errors.New("enterprise Relay intent carries unrelated fields")
	}
	return nil
}

func ValidateEnterpriseOperationResult(result EnterpriseOperationResult) error {
	if result.Version != 1 {
		return errors.New("unsupported enterprise Relay result version")
	}
	rest := result
	rest.Version, rest.Kind = 0, ""
	switch result.Kind {
	case EnterpriseOperationSend, EnterpriseOperationRetry, EnterpriseOperationCancel:
		if result.Message == nil || !validOperationMessage(*result.Message) {
			return errors.New("invalid enterprise message result")
		}
		message := result.Message
		if result.Kind == EnterpriseOperationSend && (message.Source != "send" || message.State != "queued" && message.State != "human" || message.RetryOf != "") {
			return errors.New("invalid enterprise send result")
		}
		if result.Kind == EnterpriseOperationRetry && (message.Source != "retry" || message.State != "queued" && message.State != "human" || !validEnterpriseOperationID(message.RetryOf)) {
			return errors.New("invalid enterprise retry result")
		}
		if result.Kind == EnterpriseOperationCancel && message.State != "cancelled" {
			return errors.New("invalid enterprise cancellation result")
		}
		rest.Message = nil
	case EnterpriseOperationReport:
		p := result.Publication
		if p == nil || !validEnterpriseOperationID(p.BindID) || p.Generation == 0 || p.ReportSeq == 0 || p.Outcome != "" && p.Outcome != PeerChangedCode || p.Outcome == PeerChangedCode && p.Message != nil || p.GapFrom == 0 && p.GapTo != 0 || p.GapFrom > 0 && (p.GapTo != p.ReportSeq-1 || p.GapFrom > p.GapTo) {
			return errors.New("invalid enterprise publication result")
		}
		if p.Message != nil && (!validOperationMessage(*p.Message) || p.Message.Source != "stop" || p.Message.RetryOf != "" || p.Message.State != "queued" && p.Message.State != "human" || !p.Message.From.ValidParticipant() || p.Message.FromGeneration != p.Generation || p.Message.To.ValidParticipant() && p.Message.TargetGeneration != p.ExpectedPeerGeneration) {
			return errors.New("invalid enterprise publication message")
		}
		rest.Publication = nil
	case EnterpriseOperationClaim:
		if result.Empty {
			if result.Claim != nil {
				return errors.New("empty enterprise claim contains a delivery")
			}
		} else if result.Claim == nil || !validEnterpriseOperationID(result.Claim.ID) || !validEnterpriseOperationID(result.Claim.Receipt) || result.Claim.PeerGeneration == 0 || result.Claim.Envelope == "" || !utf8.ValidString(result.Claim.Envelope) || len(result.Claim.Envelope) > MaxBodyBytes+(16<<10) {
			return errors.New("invalid enterprise claim result")
		}
		rest.Claim, rest.Empty = nil, false
	case EnterpriseOperationAck:
		if !result.HandedOff {
			return errors.New("unsettled enterprise acknowledgement result")
		}
		rest.HandedOff = false
	case EnterpriseOperationPark:
		if result.Binding == nil || !result.Binding.Active {
			return errors.New("invalid enterprise park result")
		}
		// Reuse the strict model codec without manufacturing a credential or
		// invoking its ID-allocating event constructor. These envelope fields
		// are validation-only and are never returned or persisted.
		data, err := json.Marshal(result.Binding)
		if err != nil {
			return err
		}
		if _, err := model.DecodeEnterpriseBindingUpdate(model.Event{ID: "validate-result", RoomID: result.Binding.RoomID, Kind: model.EnterpriseBindingUpdatedKind, Actor: result.Binding.Slot, CreatedAt: time.Unix(1, 0).UTC(), Data: data}); err != nil {
			return err
		}
		rest.Binding = nil
	default:
		return errors.New("unsupported enterprise Relay result kind")
	}
	if rest != (EnterpriseOperationResult{}) {
		return errors.New("enterprise Relay result carries unrelated fields")
	}
	return nil
}

func validOperationMessage(message Message) bool {
	if !validEnterpriseOperationID(message.ID) || message.From != model.ActorUser && !message.From.ValidParticipant() || message.To != model.ActorUser && !message.To.ValidParticipant() || validateBody(message.Text) != nil || !utf8.ValidString(message.Author) || message.Receipt != "" || len(message.Attachments) != 0 || message.Quote != nil || message.Review != nil || message.CreatedAt.IsZero() || message.UpdatedAt.IsZero() || !message.ClaimedAt.IsZero() {
		return false
	}
	if message.From.ValidParticipant() && (!message.FromRuntime.Valid() || message.FromRuntime != message.FromRuntime.Canonical() || message.FromGeneration == 0 || message.To.ValidParticipant() && message.To != model.OtherParticipant(message.From)) {
		return false
	}
	if message.From == model.ActorUser && (message.FromRuntime != "" || message.FromGeneration != 0 || !message.To.ValidParticipant()) {
		return false
	}
	if message.State == "human" && message.To != model.ActorUser || message.State == "queued" && !message.To.ValidParticipant() {
		return false
	}
	if message.Source != "send" && message.Source != "stop" && message.Source != "retry" || message.RetryOf != "" && !validEnterpriseOperationID(message.RetryOf) {
		return false
	}
	return message.To == model.ActorUser && message.TargetGeneration == 0 || message.To.ValidParticipant() && message.TargetGeneration > 0
}

func validEnterpriseOperationID(value string) bool {
	return validID(value) && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}
