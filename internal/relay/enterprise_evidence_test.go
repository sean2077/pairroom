package relay

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/review"
)

func enterpriseReviewAnchor() review.Anchor {
	return review.Anchor{Schema: 1, Workspace: "endpoint-owned-review-observation", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), DirtySHA256: strings.Repeat("c", 64)}
}

func TestEnterpriseEvidenceIntentKeepsEmptyEncodingAndComparableReview(t *testing.T) {
	old := EnterpriseOperationIntent{Kind: EnterpriseOperationSend, Text: "unchanged", ExpectedPeerGeneration: 1}
	encoded, err := json.Marshal(old)
	if err != nil || string(encoded) != `{"kind":"send","text":"unchanged","expected_peer_generation":1}` {
		t.Fatalf("empty quote/review changed old intent bytes: %s %v", encoded, err)
	}
	intent := old
	intent.QuoteID = "original"
	intent.Review = enterpriseReviewAnchor()
	encoded, _ = json.Marshal(intent)
	var decoded EnterpriseOperationIntent
	if json.Unmarshal(encoded, &decoded) != nil || decoded != intent || ValidateEnterpriseOperationIntent(decoded) != nil {
		t.Fatal("review intent not frozen/comparable")
	}
	for _, change := range []func(*EnterpriseOperationIntent){func(i *EnterpriseOperationIntent) { i.QuoteID = "other/room" }, func(i *EnterpriseOperationIntent) { i.Review.Schema = 2 }, func(i *EnterpriseOperationIntent) {
		i.Kind = EnterpriseOperationClaim
		i.Text = ""
		i.ExpectedPeerGeneration = 0
	}} {
		bad := intent
		change(&bad)
		if ValidateEnterpriseOperationIntent(bad) == nil {
			t.Fatal("invalid/unrelated evidence accepted")
		}
	}
}

func TestEnterpriseQuoteUsesSharedSourceAndDeduplicatesAttachmentsWithoutRecursion(t *testing.T) {
	source := operationMessage("source", "queued")
	source.Quote = &model.AgentQuote{FromHandle: "@user", Text: "nested history must not expand"}
	oldReview := enterpriseReviewAnchor()
	source.Review = &oldReview
	source.Attachments = []model.Attachment{enterpriseTestAttachment()}
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{source.Attachments[0].ID})
	intent := EnterpriseOperationIntent{Kind: EnterpriseOperationSend, Text: "new review", QuoteID: source.ID, Review: enterpriseReviewAnchor(), AttachmentIDs: ids, ExpectedPeerGeneration: 1}
	prepare, err := PrepareEnterpriseOperationWithAttachments(operationSnapshot(t, source), operationCaller(model.ActorSlot2), intent, EnterpriseOperationIDs{MessageID: "quoted"}, source.Attachments)
	if err != nil {
		t.Fatal(err)
	}
	intent.Review.Workspace = "changed caller copy"
	plan, err := prepare(source.CreatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	message := plan.Result.Message
	if message.Quote == nil || message.Quote.Text != source.Text || message.Quote.FromHandle != "@agent1" || len(message.Attachments) != 1 || message.Attachments[0] != source.Attachments[0] || message.Review == nil || *message.Review != oldReview {
		t.Fatal("quote/review producer changed frozen evidence")
	}
	encoded, _ := json.Marshal(message)
	if strings.Contains(string(encoded), "nested history must not expand") || strings.Contains(string(encoded), "changed caller copy") {
		t.Fatal("quote recursively expanded or aliased input")
	}
	bad := EnterpriseOperationIntent{Kind: EnterpriseOperationSend, Text: "reply", QuoteID: "foreign", ExpectedPeerGeneration: 1}
	prepare, err = PrepareEnterpriseOperation(operationSnapshot(t, source), operationCaller(model.ActorSlot2), bad, EnterpriseOperationIDs{MessageID: "bad"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err = prepare(source.CreatedAt)
	if err == nil || len(plan.Facts) != 0 {
		t.Fatal("foreign quote produced effects")
	}
}

func TestEnterpriseFullQuoteEnvelopeRetainsMaximumBodies(t *testing.T) {
	source := operationMessage("source", "queued")
	source.Text = strings.Repeat("s", MaxBodyBytes)
	snapshot := operationSnapshot(t, source)
	send := runOperation(t, snapshot, operationCaller(model.ActorSlot2), EnterpriseOperationIntent{Kind: EnterpriseOperationSend, Text: strings.Repeat("n", MaxBodyBytes), QuoteID: source.ID, ExpectedPeerGeneration: 1}, source.CreatedAt.Add(time.Second))
	claim := runOperation(t, appendOperationPlan(snapshot, send), operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationClaim}, source.CreatedAt.Add(2*time.Second))
	if claim.Result.Claim == nil || !strings.Contains(claim.Result.Claim.Envelope, source.Text) || !strings.Contains(claim.Result.Claim.Envelope, send.Result.Message.Text) {
		t.Fatal("full quote was truncated")
	}
}

func TestEnterpriseHistoryUsesStablePagesAndHidesPrivateReceipts(t *testing.T) {
	first := operationMessage("first", "unknown")
	first.Quote = &model.AgentQuote{FromHandle: "@user", Text: "full explicit quote"}
	anchor := enterpriseReviewAnchor()
	first.Review = &anchor
	second := operationMessage("second", "queued")
	snapshot := operationSnapshot(t, first, second)
	page, err := ProjectEnterpriseHistory(snapshot, HistoryQuery{Limit: 1})
	if err != nil || page.Total != 2 || len(page.Messages) != 1 || page.Messages[0].ID != "second" || !page.HasMore {
		t.Fatalf("newest page: %+v %v", page, err)
	}
	page, err = ProjectEnterpriseHistory(snapshot, HistoryQuery{Limit: 1, Cursor: page.NextCursor})
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != "first" || page.Messages[0].Quote.Text != first.Quote.Text || *page.Messages[0].Review != anchor {
		t.Fatal("history cursor/evidence drift")
	}
	encoded, _ := json.Marshal(page)
	if strings.Contains(string(encoded), "receipt-first") || strings.Contains(string(encoded), "session_id") || strings.Contains(string(encoded), "event_encoding") {
		t.Fatal("history leaked private protocol fields")
	}
	pending, err := ProjectEnterpriseHistory(snapshot, HistoryQuery{Pending: true, Limit: 1})
	if err != nil || pending.Messages[0].ID != "first" || pending.Messages[0].State != "unknown" {
		t.Fatal("history observation mutated unknown delivery")
	}
	if _, err := ProjectEnterpriseHistory(snapshot, HistoryQuery{Cursor: "pending:0"}); err == nil {
		t.Fatal("cross-view cursor accepted")
	}
}

func TestEnterpriseHumanAuthorsRemainDistinctAcrossQuotesAndReceiptUpdates(t *testing.T) {
	snapshot := operationSnapshot(t)
	firstCaller := operationCaller(model.ActorUser)
	firstCaller.UserID, firstCaller.ClientID = "user-first", "first-human"
	first := runOperation(t, snapshot, firstCaller, EnterpriseOperationIntent{Kind: EnterpriseOperationSend, To: model.ActorSlot1, Text: "original human body"}, time.Unix(10, 0).UTC())
	secondCaller := firstCaller
	secondCaller.UserID, secondCaller.ClientID = "user-second", "second-human"
	second := runOperation(t, appendOperationPlan(snapshot, first), secondCaller, EnterpriseOperationIntent{Kind: EnterpriseOperationSend, To: model.ActorSlot1, Text: "response", QuoteID: first.Result.Message.ID}, time.Unix(11, 0).UTC())
	message := second.Result.Message
	if first.Result.Message.Author != EnterpriseHumanAuthor("user-first") || message.Author != EnterpriseHumanAuthor("user-second") || message.Quote == nil || message.Quote.FromHandle != "@user" || message.Quote.Author != first.Result.Message.Author || message.Quote.Text != "original human body" {
		t.Fatal("new human or quoted attribution changed identities")
	}
	for _, origin := range []string{"", "user-second"} {
		if ValidateEnterpriseOriginalAuthor(first.Facts[0], origin) == nil {
			t.Fatal("nonempty author accepted a different original origin")
		}
	}
	if ValidateEnterpriseOriginalAuthor(first.Facts[0], "user-first") != nil {
		t.Fatal("exact authenticated author refused")
	}
	legacy := first.Facts[0]
	var payload messageFact
	_ = json.Unmarshal(legacy.Data, &payload)
	payload.Author = ""
	legacy.Data, _ = json.Marshal(payload)
	if ValidateEnterpriseOriginalAuthor(legacy, "user-second") != nil {
		t.Fatal("legacy empty author lost compatibility")
	}
	if ValidateEnterpriseOriginalAuthor(model.Event{Kind: EventMessage, Data: json.RawMessage(`{"note":"legacy owner validates this payload"}`)}, "") != nil {
		t.Fatal("author hook redefined unrelated legacy fact admission")
	}
	payload.Author, payload.State, payload.Receipt = EnterpriseHumanAuthor("user-first"), "handed_off", "private-receipt"
	legacy.Actor = model.ActorSlot1
	legacy.Data, _ = json.Marshal(payload)
	if ValidateEnterpriseOriginalAuthor(legacy, "user-second") != nil {
		t.Fatal("delivery causation relabeled original human author")
	}
	claim := runOperation(t, appendOperationPlan(appendOperationPlan(snapshot, first), second), operationCaller(model.ActorSlot1), EnterpriseOperationIntent{Kind: EnterpriseOperationClaim}, time.Unix(12, 0).UTC())
	if claim.Result.Claim == nil || !strings.Contains(claim.Result.Claim.Envelope, "from: @user\n") || !strings.Contains(claim.Result.Claim.Envelope, `author: "enterprise:user-first"`) || strings.Contains(claim.Result.Claim.Envelope, "@owner") {
		t.Fatal("new enterprise human attribution used an Owner handle")
	}
	encoded, _ := json.Marshal(model.AgentQuote{FromHandle: "@user", Text: "legacy"})
	if string(encoded) != `{"from_handle":"@user","text":"legacy"}` {
		t.Fatal("optional author changed legacy quote bytes")
	}
}
