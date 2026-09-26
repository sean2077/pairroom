package relay

import (
	"errors"
	"reflect"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

// ErrSendPayloadConflict means the client message ID already names an accepted
// publication with different delivered content. Nothing new was published.
// SendPayloadConflictCode is its stable wire code on relay error responses.
var ErrSendPayloadConflict = errors.New("client message ID already refers to a different body, target, attachments, quote, or review evidence; retry the original request unchanged")

const SendPayloadConflictCode = "send_payload_conflict"

// Same-ID recovery may return the original receipt only for the same delivered
// payload. State, timestamps, generation and receipt are transport facts, not
// request content. A new ID still intentionally publishes identical content.
func sameMessagePayload(a, b Message) bool {
	if a.From != b.From || a.To != b.To || a.Text != b.Text || len(a.Attachments) != len(b.Attachments) || !reflect.DeepEqual(a.Quote, b.Quote) || !reflect.DeepEqual(a.Review, b.Review) {
		return false
	}
	for i := range a.Attachments {
		if !reflect.DeepEqual(a.Attachments[i], b.Attachments[i]) {
			return false
		}
	}
	return true
}

// A receipt retry must not re-read uploaded files: their later loss/tampering
// cannot change whether the original publication was accepted. Collection still
// verifies the actual bytes in envelope. Match ResolveMany's ID normalization.
//
// A CLI retry re-uploads each image and so carries a fresh attachment ID. Only
// such an unknown ID is resolved, and it stands for the original attachment at
// the same position when the delivered content is identical: bytes (SHA-256 and
// size), media type and display name. Anything else remains a conflict.
func acceptedAttachments(original Message, ids []string, resolve func(string) (model.Attachment, error)) ([]model.Attachment, error) {
	known := make(map[string]model.Attachment, len(original.Attachments))
	for _, a := range original.Attachments {
		known[a.ID] = a
	}
	seen := make(map[string]bool, len(ids))
	var result []model.Attachment
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		a, ok := known[id]
		if !ok {
			position := len(result)
			if resolve == nil || position >= len(original.Attachments) {
				return nil, ErrSendPayloadConflict
			}
			fresh, err := resolve(id)
			if err != nil || !sameAttachmentContent(original.Attachments[position], fresh) {
				return nil, ErrSendPayloadConflict
			}
			a = original.Attachments[position]
		}
		result = append(result, a)
	}
	return result, nil
}

func sameAttachmentContent(a, b model.Attachment) bool {
	return a.SHA256 != "" && strings.EqualFold(a.SHA256, b.SHA256) && a.Size == b.Size && a.MediaType == b.MediaType && a.Kind == b.Kind && a.Name == b.Name
}
