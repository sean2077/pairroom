package relay

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func enterpriseTestAttachment() model.Attachment {
	return model.Attachment{ID: "att-111111111111111111111111", Name: "report.txt", Kind: "file", MediaType: "text/plain", Size: 9, SHA256: strings.Repeat("1", 64), Source: "enterprise-upload", CreatedAt: time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)}
}

func TestEnterpriseAttachmentSendFreezesVerifiedMetadataAndOriginalHumanKey(t *testing.T) {
	meta := enterpriseTestAttachment()
	selected, err := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	if err != nil {
		t.Fatal(err)
	}
	intent := EnterpriseOperationIntent{Kind: EnterpriseOperationSend, To: model.ActorSlot1, AttachmentIDs: selected}
	media := []model.Attachment{meta}
	snapshot := operationSnapshot(t)
	prepare, err := PrepareEnterpriseOperationWithAttachments(snapshot, operationCaller(model.ActorUser), intent, EnterpriseOperationIDs{MessageID: "human-media"}, media)
	if err != nil {
		t.Fatal(err)
	}
	media[0].Name = "changed-after-admission.txt"
	plan, err := prepare(meta.CreatedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	fact := operationMessageFact(t, plan.Facts[0])
	if fact.ClientKey != "user/u-owner/operation-1" || len(fact.Attachments) != 1 || fact.Attachments[0] != meta || fact.Text != "" {
		t.Fatal("metadata-only human send lost canonical identity")
	}
	accepted := plan.Facts[0]
	accepted.ID = "accepted-fact"
	accepted.Seq = 4
	// Existing human_send producers assigned the event timestamp separately
	// from the message. Original accepted receipts retain both timestamps.
	accepted.CreatedAt = accepted.CreatedAt.Add(time.Nanosecond)
	public, err := EnterpriseHumanSendMessage(accepted, "u-owner", "operation-1", intent)
	if err != nil || public.ID != "human-media" || public.Attachments[0] != meta {
		t.Fatalf("original human receipt could not be rendered: %v", err)
	}
	plan.Result.Message.Attachments[0].SHA256 = strings.Repeat("2", 64)
	again, err := prepare(meta.CreatedAt.Add(time.Hour))
	if err != nil || again.Result.Message.Attachments[0] != meta {
		t.Fatal("returned message aliases a future preparation")
	}
	if _, err := PrepareEnterpriseOperationWithAttachments(snapshot, operationCaller(model.ActorUser), intent, EnterpriseOperationIDs{MessageID: "human-media"}, nil); err == nil {
		t.Fatal("ID without verified metadata entered a send plan")
	}
	other := meta
	other.ID = "att-222222222222222222222222"
	if _, err := PrepareEnterpriseOperationWithAttachments(snapshot, operationCaller(model.ActorUser), intent, EnterpriseOperationIDs{MessageID: "human-media"}, []model.Attachment{other}); err == nil {
		t.Fatal("metadata did not match the selected ordered IDs")
	}
}

func TestEnterpriseAttachmentClaimRequiresThePrefetchedHeadAndNoServerPath(t *testing.T) {
	message := operationMessage("media-message", "queued")
	message.Attachments = []model.Attachment{enterpriseTestAttachment()}
	snapshot := operationSnapshot(t, message)
	at := message.CreatedAt.Add(time.Second)
	view, err := DescribeEnterpriseSnapshot(snapshot, model.ActorSlot2, "bind-2", 1, at)
	if err != nil || view.Next == nil || view.Next.MessageID != message.ID || view.Next.TargetGeneration != 1 || view.Next.Attachments[0] != message.Attachments[0] {
		t.Fatalf("missing authorized head manifest: %v", err)
	}
	encoded, _ := json.Marshal(view)
	if strings.Contains(string(encoded), message.Text) || strings.Contains(string(encoded), `"path"`) {
		t.Fatal("manifest peek released body or server path")
	}
	for _, intent := range []EnterpriseOperationIntent{
		{Kind: EnterpriseOperationClaim},
		{Kind: EnterpriseOperationClaim, MessageID: "different-head", ExpectedTargetGeneration: 1},
		{Kind: EnterpriseOperationClaim, MessageID: message.ID, ExpectedTargetGeneration: 2},
	} {
		prepare, err := PrepareEnterpriseOperation(snapshot, operationCaller(model.ActorSlot2), intent, EnterpriseOperationIDs{Receipt: "prepared-receipt"})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := prepare(at)
		if err == nil || len(plan.Facts) != 0 {
			t.Fatal("unprepared or changed head produced a delivering fact")
		}
		if intent.MessageID != "" && !errors.Is(err, ErrPreparedClaimChanged) {
			t.Fatal("changed head was not a definite preparation refusal")
		}
	}
	plan := runOperation(t, snapshot, operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationClaim, MessageID: message.ID, ExpectedTargetGeneration: 1}, at)
	if plan.Result.Claim != nil || plan.Result.StructuredClaim == nil || plan.Result.StructuredClaim.Message.ID != message.ID || plan.Result.StructuredClaim.Message.Attachments[0] != message.Attachments[0] || plan.Result.StructuredClaim.Message.State != "queued" {
		t.Fatal("claim lost its immutable structured envelope")
	}
	fact := operationMessageFact(t, plan.Facts[0])
	if fact.State != "delivering" || fact.Receipt != "new-receipt" || fact.Attachments[0] != message.Attachments[0] {
		t.Fatal("claim fact differs from the prepared message")
	}
	claimed := appendOperationPlan(snapshot, plan)
	ack := runOperation(t, claimed, operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationAck, MessageID: message.ID, Receipt: "new-receipt"}, at.Add(time.Second))
	if !ack.Result.HandedOff || operationMessageFact(t, ack.Facts[0]).Attachments[0] != message.Attachments[0] {
		t.Fatal("ACK changed original attachment reference")
	}
}
