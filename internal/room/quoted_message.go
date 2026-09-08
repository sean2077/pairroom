package room

import (
	"fmt"

	"github.com/sean2077/pairroom/internal/model"
)

// messageForUserQuote reads the complete durable transcript, not the browser's
// paginated preview. The returned copy must not alias mutable snapshot data.
func (e *Engine) messageForUserQuote(id string) (model.Message, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	message, found := e.findMessageLocked(id)
	if !found {
		return model.Message{}, fmt.Errorf("unknown quoted message %q in this Room", id)
	}
	return cloneMessage(message), nil
}

// deliveryQuote runs at the shared StartTurn/Steer boundary, including queued,
// restored, and retried inputs. Agent ReplyTo values are correlation links, not
// user-selected quotes: expanding those would repeatedly resend the transcript.
func (e *Engine) deliveryQuote(message model.Message) (*model.AgentQuote, []model.Attachment, error) {
	attachments := append([]model.Attachment(nil), message.Attachments...)
	if message.From != model.ActorUser || message.ReplyTo == "" {
		return nil, attachments, nil
	}
	source, err := e.messageForUserQuote(message.ReplyTo)
	if err != nil {
		return nil, nil, err
	}
	from := "@" + string(source.From)
	if source.From.ValidParticipant() {
		from = model.ParticipantIdentityFor(source.From, e.runtimeKinds()).MentionHandle
	}
	quote := &model.AgentQuote{FromHandle: from, Text: source.Text}

	// Quoted images need the same native media path as newly attached images.
	// Resolve their canonical IDs through agentAttachments in deliver(); never
	// accept a browser-supplied path or duplicate an image already on the input.
	seen := make(map[string]bool, len(attachments)+len(source.Attachments))
	for _, attachment := range attachments {
		seen[attachment.ID] = true
	}
	for _, attachment := range source.Attachments {
		if !seen[attachment.ID] {
			attachments = append(attachments, attachment)
			seen[attachment.ID] = true
		}
	}
	return quote, attachments, nil
}
