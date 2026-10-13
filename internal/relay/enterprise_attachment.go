package relay

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/prompt"
	"github.com/sean2077/pairroom/internal/review"
)

var ErrPreparedClaimChanged = errors.New("prepared enterprise inbox head changed")

const enterpriseEnvelopeBytes = 2*MaxBodyBytes + (16 << 10)

// EnterpriseHumanAuthor records the authenticated immutable User identity,
// independent of the User's later display name, Room role or ownership.
func EnterpriseHumanAuthor(userID string) string { return "enterprise:" + userID }

func ValidEnterpriseHumanAuthor(author string) bool {
	return strings.HasPrefix(author, "enterprise:") && validEnterpriseOperationID(strings.TrimPrefix(author, "enterprise:"))
}

// ValidateEnterpriseOriginalAuthor pairs new human publication attribution
// with its immutable SQL origin. Delivery updates keep the original author
// even when a different User or operator caused that later transition.
func ValidateEnterpriseOriginalAuthor(fact model.Event, originUser string) error {
	if fact.Kind != EventMessage {
		return nil
	}
	// This provenance hook owns only a present nonempty author. Historical
	// fact validation remains with its existing codec and replay owner.
	var fields map[string]json.RawMessage
	if json.Unmarshal(fact.Data, &fields) != nil || fields["author"] == nil {
		return nil
	}
	var author string
	if json.Unmarshal(fields["author"], &author) != nil {
		return errors.New("invalid enterprise human author")
	}
	if author == "" {
		return nil
	}
	var payload messageFact
	decoder := json.NewDecoder(bytes.NewReader(fact.Data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid enterprise message fact")
	}
	if payload.From != model.ActorUser || payload.Source != "send" || payload.State != "queued" || payload.Receipt != "" || !payload.ClaimedAt.IsZero() || payload.RetryOf != "" || payload.Author == "" {
		return nil
	}
	if fact.Actor != model.ActorUser || !validEnterpriseOperationID(originUser) || payload.Author != EnterpriseHumanAuthor(originUser) {
		return errors.New("original enterprise human author differs from immutable origin")
	}
	return nil
}

// EnterpriseHumanSendMessage returns only the original public message after
// SQL has authenticated and recovered its exact human_send operation. Receipt
// capabilities and unknown payload fields are never copied into the response.
func EnterpriseHumanSendMessage(fact model.Event, userID, clientID string, intent EnterpriseOperationIntent) (Message, error) {
	if fact.Kind != EventMessage || fact.Actor != model.ActorUser || fact.Seq == 0 || !validEnterpriseOperationID(fact.ID) || intent.Kind != EnterpriseOperationSend {
		return Message{}, errors.New("invalid human message result")
	}
	var payload messageFact
	decoder := json.NewDecoder(bytes.NewReader(fact.Data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(new(any)) != io.EOF || payload.Receipt != "" || payload.ClientKey != enterpriseClientKey(userID, clientID) || payload.Author != "" && payload.Author != EnterpriseHumanAuthor(userID) || payload.From != model.ActorUser || payload.To != intent.To || payload.Text != intent.Text || !payload.UpdatedAt.Equal(payload.CreatedAt) {
		return Message{}, errors.New("human message result disagrees with original intent")
	}
	if err := MatchEnterpriseSendEvidence(intent, payload.Message); err != nil {
		return Message{}, err
	}
	message := cloneMessage(payload.Message)
	if err := ValidateEnterpriseOperationResult(EnterpriseOperationResult{Version: 1, Kind: EnterpriseOperationSend, Message: &message}); err != nil {
		return Message{}, err
	}
	return message, nil
}

// MatchEnterpriseSendEvidence checks the frozen explicit portion of a send
// result. SQL separately validates quoted source content and inherited object
// references against the same Room before accepting or replaying the result.
func MatchEnterpriseSendEvidence(intent EnterpriseOperationIntent, message Message) error {
	if !validEnterpriseQuote(message.Quote) || (intent.QuoteID == "") != (message.Quote == nil) || intent.Review == (review.Anchor{}) && message.Review != nil || intent.Review != (review.Anchor{}) && (message.Review == nil || *message.Review != intent.Review) {
		return errors.New("enterprise message quote or review differs from intent")
	}
	if intent.QuoteID == "" {
		return matchEnterpriseAttachmentSelection(intent.AttachmentIDs, message.Attachments)
	}
	ids := intent.AttachmentIDs.Slice()
	if validateEnterpriseAttachments(message.Attachments) != nil || len(message.Attachments) < len(ids) {
		return errors.New("invalid quoted enterprise attachment selection")
	}
	for i, id := range ids {
		if message.Attachments[i].ID != id {
			return errors.New("explicit attachment order differs from intent")
		}
	}
	return nil
}

// EnterpriseIncoming exposes only the next candidate's immutable manifest,
// before any delivery lease starts. Content paths belong to the endpoint.
type EnterpriseIncoming struct {
	MessageID        string             `json:"message_id"`
	TargetGeneration uint64             `json:"target_generation"`
	Attachments      []model.Attachment `json:"attachments"`
}

func validateEnterpriseAttachments(values []model.Attachment) error {
	if len(values) > attachment.MaxImagesPerMessage {
		return errors.New("enterprise attachment count exceeds limit")
	}
	var total int64
	for i, value := range values {
		hash, err := hex.DecodeString(value.SHA256)
		if !model.ValidEnterpriseAttachmentID(value.ID) || value.Name == "" || len(value.Name) > 255 || !utf8.ValidString(value.Name) || strings.TrimSpace(value.Name) != value.Name || strings.ContainsAny(value.Name, "/\\") || strings.ContainsFunc(value.Name, unicode.IsControl) || value.Size <= 0 || value.Size > attachment.MaxImageBytes || err != nil || len(hash) != 32 || hex.EncodeToString(hash) != value.SHA256 || value.CreatedAt.IsZero() || len(value.Source) > 128 || !utf8.ValidString(value.Source) || strings.ContainsFunc(value.Source, unicode.IsControl) {
			return errors.New("invalid enterprise attachment manifest")
		}
		for _, prior := range values[:i] {
			if prior.ID == value.ID {
				return errors.New("duplicate enterprise attachment manifest")
			}
		}
		switch value.Kind {
		case "file":
			if value.MediaType != "text/plain" || value.Width != 0 || value.Height != 0 {
				return errors.New("invalid enterprise evidence manifest")
			}
		case "image":
			if value.MediaType != "image/png" && value.MediaType != "image/jpeg" && value.MediaType != "image/gif" && value.MediaType != "image/webp" || value.Width <= 0 || value.Height <= 0 || value.Width > attachment.MaxImageDimension || value.Height > attachment.MaxImageDimension || int64(value.Width)*int64(value.Height) > attachment.MaxImagePixels {
				return errors.New("invalid enterprise image manifest")
			}
		default:
			return errors.New("unsupported enterprise attachment kind")
		}
		total += value.Size
		if total > attachment.MaxTotalImageBytes {
			return errors.New("enterprise attachment total exceeds limit")
		}
	}
	return nil
}

func matchEnterpriseAttachmentSelection(ids model.EnterpriseAttachmentIDs, values []model.Attachment) error {
	if ids.Validate() != nil || validateEnterpriseAttachments(values) != nil {
		return errors.New("invalid enterprise attachment selection")
	}
	selected := ids.Slice()
	if len(selected) != len(values) {
		return errors.New("enterprise attachment metadata is incomplete")
	}
	for i, id := range selected {
		if values[i].ID != id {
			return errors.New("enterprise attachment metadata order changed")
		}
	}
	return nil
}

func (e *Engine) validateEnterpriseEnvelope(message Message) error {
	if err := validateEnterpriseAttachments(message.Attachments); err != nil {
		return err
	}
	input := e.envelopeInput(message)
	for _, meta := range message.Attachments {
		// This is only a budget check of the shared formatter. The opaque ID
		// stands in for a path; the server never releases this draft envelope.
		input.Attachments = append(input.Attachments, model.AgentAttachment{Attachment: meta, Path: meta.ID})
	}
	if len(prompt.Envelope(input)) > enterpriseEnvelopeBytes {
		return errors.New("enterprise envelope exceeds limit")
	}
	return nil
}
