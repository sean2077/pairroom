package room

import (
	"fmt"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// nativeMessageContent expands only an explicit user quote. Agent ReplyTo
// values correlate native responses, so expanding them would replay history on
// every relay. Keep the durable body untouched and resolve again on dispatch so
// queued, steered, retried, and restored messages all use the same Room source.
func (e *Engine) nativeMessageContent(message model.Message) (string, []model.Attachment, error) {
	if message.From != model.ActorUser || message.ReplyTo == "" {
		return message.Text, message.Attachments, nil
	}

	e.mu.RLock()
	quoted, found := e.findMessageLocked(message.ReplyTo)
	if found {
		quoted = cloneMessage(quoted)
	}
	e.mu.RUnlock()
	if !found {
		return "", nil, fmt.Errorf("quoted message %q is unavailable in this Room", message.ReplyTo)
	}

	from := "@user"
	if quoted.From.ValidParticipant() {
		from = model.ParticipantIdentityFor(quoted.From, e.runtimeKinds()).MentionHandle
	} else if quoted.From == model.ActorSystem {
		from = "PairRoom"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[Quoted message from %s]\n> %s", from, strings.ReplaceAll(quoted.Text, "\n", "\n> "))
	if len(quoted.Attachments) > 0 {
		fmt.Fprint(&b, "\n> attachments:")
		for _, attachment := range quoted.Attachments {
			fmt.Fprintf(&b, "\n> - name: %q; type: %q", attachment.Name, attachment.MediaType)
		}
	}
	fmt.Fprintf(&b, "\n\n[Current message]\n%s", message.Text)
	// Reuse the usual native attachment integrity checks and keep local paths
	// outside the Event Log/API. A shared image is sent only once, retaining the
	// quoted message's accepted content identity rather than substituting bytes.
	return b.String(), mergeAttachments(quoted.Attachments, message.Attachments), nil
}
