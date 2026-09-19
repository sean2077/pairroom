package relay

import (
	"errors"
	"reflect"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
)

var errSendPayloadConflict = errors.New("client message ID already refers to a different body, target, attachments, or quote; retry the original request unchanged")

// Same-ID recovery may return the original receipt only for the same delivered
// payload. State, timestamps, generation and receipt are transport facts, not
// request content. A new ID still intentionally publishes identical content.
func sameMessagePayload(a, b Message) bool {
	if a.From != b.From || a.To != b.To || a.Text != b.Text || len(a.Attachments) != len(b.Attachments) || !reflect.DeepEqual(a.Quote, b.Quote) {
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
func acceptedAttachments(original Message, ids []string) ([]model.Attachment, error) {
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
			return nil, errSendPayloadConflict
		}
		result = append(result, a)
	}
	return result, nil
}
