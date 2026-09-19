package relay

import (
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestNativeReceiptRetryDoesNotReopenAttachmentFiles(t *testing.T) {
	e, auth, _ := testEngine(t)
	a := auth[model.ActorSlot1]
	original := e.makeMessage(a.Slot, model.ActorSlot2, "accepted image", "send")
	original.Attachments = []model.Attachment{{ID: "saved-image", SHA256: "original-digest", Size: 10}}
	key := bindingKey(a.BindID, a.Generation) + "/send/receipt"
	if err := e.append(EventMessage, a.Slot, messageFact{Message: original, ClientKey: key}); err != nil {
		t.Fatal(err)
	}
	before := e.Sequence()
	got, err := e.Send(a, SendRequest{ID: "receipt", Text: original.Text, AttachmentIDs: []string{" saved-image ", "saved-image", ""}})
	if err != nil || got.ID != original.ID || e.Sequence() != before {
		t.Fatalf("accepted receipt depended on attachment availability: %v", err)
	}
	if _, err := e.Send(a, SendRequest{ID: "receipt", Text: original.Text, AttachmentIDs: []string{"different-image"}}); err == nil {
		t.Fatal("receipt retry substituted an attachment")
	}
}
