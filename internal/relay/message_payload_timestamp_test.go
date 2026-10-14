package relay

import (
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestSameMessagePayloadAttachmentTimestampUsesInstant(t *testing.T) {
	at := time.Date(2026, 10, 13, 1, 2, 3, 456000000, time.UTC)
	first := Message{From: model.ActorUser, To: model.ActorSlot1, Text: "same accepted request", Attachments: []model.Attachment{{ID: "object", Name: "evidence.txt", CreatedAt: at, Source: "upload"}}}
	second := first
	second.Attachments = append([]model.Attachment(nil), first.Attachments...)
	second.Attachments[0].CreatedAt = at.In(time.FixedZone("SQL UTC representation", 0))
	if !sameMessagePayload(first, second) {
		t.Fatal("same immutable timestamp made original receipt recovery conflict")
	}
	second.Attachments[0].CreatedAt = at.Add(time.Nanosecond)
	if sameMessagePayload(first, second) {
		t.Fatal("changed immutable timestamp recovered the original receipt")
	}
	second.Attachments[0] = first.Attachments[0]
	second.Attachments[0].Source = "changed"
	if sameMessagePayload(first, second) {
		t.Fatal("changed attachment source recovered the original receipt")
	}
}
