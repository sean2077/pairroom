package room

import (
	"encoding/json"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func attentionEvent(t *testing.T, kind string, payload any) model.Event {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return model.Event{ID: "ev", Kind: kind, Data: data}
}

// Only facts where relay has stopped for a human, an approval is pending, a
// turn failed or a stall warning fired raise attention; routine relay does not.
func TestEmbeddedAttentionClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		ev   model.Event
		kind string
	}{
		{"answer for the human only", attentionEvent(t, EventMessageCreated, model.Message{ID: "m1", From: model.ActorSlot1, To: []model.ActorID{model.ActorUser}}), AttentionHumanTurn},
		{"answer relayed to the peer", attentionEvent(t, EventMessageCreated, model.Message{ID: "m2", From: model.ActorSlot1, To: []model.ActorID{model.ActorUser, model.ActorSlot2}}), ""},
		{"human input", attentionEvent(t, EventMessageCreated, model.Message{ID: "m3", From: model.ActorUser, To: []model.ActorID{model.ActorSlot1}}), ""},
		{"pending approval", attentionEvent(t, EventApprovalUpdated, model.Approval{ID: "a1", Agent: model.ActorSlot2, Status: "pending"}), AttentionApproval},
		{"resolved approval", attentionEvent(t, EventApprovalUpdated, model.Approval{ID: "a1", Agent: model.ActorSlot2, Status: "approved"}), ""},
		{"failed processing", attentionEvent(t, EventProcessingUpdated, model.ProcessingUpdate{MessageID: "m1", Target: model.ActorSlot2, State: model.ProcessingFailed}), AttentionAgentFailed},
		{"completed processing", attentionEvent(t, EventProcessingUpdated, model.ProcessingUpdate{MessageID: "m1", Target: model.ActorSlot2, State: model.ProcessingCompleted}), ""},
		{"stall warning", attentionEvent(t, EventSystemNotice, model.SystemNotice{Level: "warning", Text: "Codex has produced no runtime event for 5m0s"}), AttentionStallWarning},
		{"other notice", attentionEvent(t, EventSystemNotice, model.SystemNotice{Level: "info", Text: "restored"}), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AttentionFromEvent(tc.ev)
			if ok != (tc.kind != "") || got.Kind != tc.kind {
				t.Fatalf("attention = %+v, %v; want %q", got, ok, tc.kind)
			}
		})
	}
}
