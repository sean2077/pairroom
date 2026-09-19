package relay

import "reflect"

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
