package relay

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestEnterpriseResourceReserveUsesFullSharedCompletionFactsWithoutEffects(t *testing.T) {
	queued, delivering, unknown, settled := operationMessage("queued", "queued"), operationMessage("delivering", "delivering"), operationMessage("unknown", "unknown"), operationMessage("settled", "handed_off")
	queued.Text = strings.Repeat("<", MaxBodyBytes)
	queued.Quote = &model.AgentQuote{FromHandle: "@user", Author: EnterpriseHumanAuthor("original-user"), Text: strings.Repeat("&", MaxBodyBytes)}
	anchor := enterpriseReviewAnchor()
	queued.Review, queued.Attachments = &anchor, []model.Attachment{enterpriseTestAttachment()}
	snapshot := operationSnapshot(t, queued, delivering, unknown, settled)
	before, _ := json.Marshal(snapshot)
	reserve, err := EnterpriseResourceReserve(snapshot)
	if err != nil || reserve.Pending != 3 || len(reserve.TerminalFacts) != 3+2+1+2 {
		t.Fatalf("completion liabilities: pending=%d facts=%d err=%v", reserve.Pending, len(reserve.TerminalFacts), err)
	}
	counts := map[string][]string{}
	retirement := 0
	for _, fact := range reserve.TerminalFacts {
		if fact.Kind == model.EnterpriseBindingUpdatedKind {
			binding, err := model.DecodeEnterpriseBindingUpdate(fact)
			if err != nil || binding.Active || len(binding.RetiredReason) != 256 {
				t.Fatal("retirement allowance lost its existing strict constructor")
			}
			retirement++
			continue
		}
		var payload messageFact
		if json.Unmarshal(fact.Data, &payload) != nil || payload.ID == "settled" {
			t.Fatal("terminal history acquired a completion liability")
		}
		counts[payload.ID] = append(counts[payload.ID], payload.State)
		if payload.ID == "queued" && (payload.Text != queued.Text || payload.Quote == nil || *payload.Quote != *queued.Quote || payload.Review == nil || *payload.Review != anchor || !reflect.DeepEqual(payload.Attachments, queued.Attachments) || payload.Receipt != strings.Repeat("<", 256)) {
			t.Fatal("completion reserve dropped full body, quote, review, media or receipt domain")
		}
	}
	if retirement != 2 || !reflect.DeepEqual(counts["queued"], []string{"delivering", "unknown", "handed_off"}) || !reflect.DeepEqual(counts["delivering"], []string{"unknown", "handed_off"}) || !reflect.DeepEqual(counts["unknown"], []string{"handed_off"}) {
		t.Fatal("reserve did not preserve the shared completion sequence")
	}
	after, _ := json.Marshal(snapshot)
	if !bytes.Equal(before, after) {
		t.Fatal("reserve calculator changed the source snapshot")
	}
	snapshot.Events[0].Data = []byte(`{"corrupt":true}`)
	if _, err := EnterpriseResourceReserve(snapshot); err == nil {
		t.Fatal("resource calculator accepted invalid replay")
	}
}

func TestEnterpriseQueueLimitRefusesOnlyNewQueuedWork(t *testing.T) {
	messages := make([]Message, EnterpriseQueuePendingLimit)
	messages[0] = operationMessage("old-unknown", "unknown")
	for i := 1; i < len(messages); i++ {
		messages[i] = operationMessage(fmt.Sprintf("queued-%03d", i), "queued")
	}
	snapshot := operationSnapshot(t, messages...)
	at := messages[0].CreatedAt.Add(time.Second)
	for _, tc := range []struct {
		caller EnterpriseOperationCaller
		intent EnterpriseOperationIntent
	}{{operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationSend, Text: "new", ExpectedPeerGeneration: 1}}, {operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationReport, Text: "@agent2 new", ReportSeq: 1, ExpectedPeerGeneration: 1}}, {operationCaller(model.ActorUser), EnterpriseOperationIntent{Kind: EnterpriseOperationRetry, MessageID: "old-unknown"}}} {
		prepare, err := PrepareEnterpriseOperation(snapshot, tc.caller, tc.intent, EnterpriseOperationIDs{MessageID: "new"})
		if err != nil {
			t.Fatal(err)
		}
		plan, err := prepare(at)
		if !errors.Is(err, ErrEnterpriseQueueLimit) || len(plan.Facts) != 0 {
			t.Fatalf("queue overflow created work: %s %+v %v", tc.intent.Kind, plan, err)
		}
	}
	for _, tc := range []struct {
		caller EnterpriseOperationCaller
		intent EnterpriseOperationIntent
	}{{operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationReport, Text: "unaddressed receipt only", ReportSeq: 1, ExpectedPeerGeneration: 1}}, {operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationSend, To: model.ActorUser, Text: "human evidence"}}, {operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationClaim}}, {operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationAck, MessageID: "old-unknown", Receipt: "receipt-old-unknown"}}, {operationCaller(model.ActorUser), EnterpriseOperationIntent{Kind: EnterpriseOperationCancel, MessageID: "queued-001"}}} {
		runOperation(t, snapshot, tc.caller, tc.intent, at)
	}
	prepare, err := PrepareEnterpriseBindingRetirement(snapshot, "bind-2", "device.revoked", "retire")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(at); err != nil {
		t.Fatal("queue limit prevented required retirement")
	}
}

func TestProjectEnterpriseSnapshotTailUsesExistingCountAndBodyBudget(t *testing.T) {
	messages := make([]Message, SnapshotMessageLimit+20)
	for i := range messages {
		messages[i] = operationMessage(fmt.Sprintf("message-%03d", i), "handed_off")
		messages[i].Text = strings.Repeat("x", 4096)
	}
	snapshot := operationSnapshot(t, messages...)
	before, _ := json.Marshal(snapshot)
	tail, err := ProjectEnterpriseSnapshotTail(snapshot)
	if err != nil || tail.TotalMessages != len(messages) || len(tail.Messages) >= SnapshotMessageLimit || tail.Messages[len(tail.Messages)-1].ID != messages[len(messages)-1].ID {
		t.Fatalf("bounded enterprise tail: total=%d loaded=%d err=%v", tail.TotalMessages, len(tail.Messages), err)
	}
	var bodyBytes int
	for _, message := range tail.Messages {
		bodyBytes += len(message.Text)
		if message.Receipt != "" || len(message.Text) != 4096 {
			t.Fatal("tail disclosed private receipt or clipped body")
		}
	}
	if bodyBytes > SnapshotTextBudget {
		t.Fatal("tail exceeded shared body budget")
	}
	after, _ := json.Marshal(snapshot)
	if !bytes.Equal(before, after) {
		t.Fatal("tail projection changed stored history")
	}
}
