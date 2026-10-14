package enterpriseservice

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
)

func TestHistoryHTTPPGQuoteReviewReferencesAndPrivateClaimObservation(t *testing.T) {
	f := newObjectHTTPFixture(t)
	owner := f.browser("member-one")
	f.login(owner)
	o := f.session(owner)
	const room = "evidence-history-room"
	f.createRoom(owner, o.CSRF, room)
	f.createRoom(owner, o.CSRF, "foreign-history-room")
	native := grantsPGNative(t, f)
	device := grantsPGEnroll(t, f, native, owner, o.CSRF, "history-native")
	one := grantsPGBind(t, f, native, owner, o.CSRF, room, device, model.ActorSlot1, "history-one")
	two := grantsPGBind(t, f, native, owner, o.CSRF, room, device, model.ActorSlot2, "history-two")
	data := []byte("original immutable evidence")
	meta := objectHTTPFinish(t, f, native, room, "history-verify", objectHTTPReserve(t, f, native, room, "history-reserve", data, one.token, ""), data, one.token, "")
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	anchor := review.Anchor{Schema: 1, Workspace: "endpoint review observation", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), DirtySHA256: strings.Repeat("c", 64)}
	base := "/enterprise/device/v1/rooms/" + room + "/relay"
	send := grantsPGRelayRequest{ClientID: "history-source", BindingID: one.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "full original body", AttachmentIDs: ids, Review: anchor, ExpectedPeerGeneration: 1}}
	status, body := grantsPGCall(t, f, native, http.MethodPost, base, one.token, send)
	var source grantsPGRelayReply
	if status != 200 || json.Unmarshal(body, &source) != nil || source.Result.Message == nil {
		t.Fatalf("original evidence send: %d %s", status, body)
	}
	human := map[string]any{"client_id": "human-history-quote", "to": "slot1", "text": "human quoted response", "quote_id": source.Result.Message.ID, "review": anchor}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", human, o.CSRF)
	if status != 200 || !bytes.Contains(body, []byte(meta.ID)) || !bytes.Contains(body, []byte("full original body")) || !bytes.Contains(body, []byte(anchor.Head)) {
		t.Fatalf("HTTP human quote/reference producer: %d %s", status, body)
	}
	quoted := grantsPGRelayRequest{ClientID: "native-history-quote", BindingID: two.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "native quoted response", QuoteID: source.Result.Message.ID, Review: anchor, ExpectedPeerGeneration: 1}}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base, two.token, quoted)
	var accepted grantsPGRelayReply
	if status != 200 || json.Unmarshal(body, &accepted) != nil || accepted.Result.Message == nil || accepted.Result.Message.Quote == nil || accepted.Result.Message.Quote.Text != source.Result.Message.Text || accepted.Result.Message.Review == nil || *accepted.Result.Message.Review != anchor || len(accepted.Result.Message.Attachments) != 1 || accepted.Result.Message.Attachments[0] != meta {
		t.Fatalf("HTTP native quote/reference producer: %d %s", status, body)
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/resolve", two.token, quoted)
	var recovered grantsPGRelayReply
	if status != 200 || json.Unmarshal(body, &recovered) != nil || !reflect.DeepEqual(recovered.Result, accepted.Result) {
		t.Fatal("HTTP quote/review receipt changed on read-only recovery")
	}
	claimRequest := grantsPGRelayRequest{ClientID: "history-private-claim", BindingID: two.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim, MessageID: source.Result.Message.ID, ExpectedTargetGeneration: 1}}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base, two.token, claimRequest)
	var claimed grantsPGRelayReply
	if status != 200 || json.Unmarshal(body, &claimed) != nil || claimed.Result.StructuredClaim == nil {
		t.Fatalf("history original structured claim: %d %s", status, body)
	}
	counts := func() [5]int64 {
		t.Helper()
		var got [5]int64
		if err := f.admin.QueryRow(t.Context(), `SELECT (SELECT current_seq FROM rooms WHERE room_id=$1),(SELECT revision FROM rooms WHERE room_id=$1),(SELECT count(*) FROM operations),(SELECT count(*) FROM attachment_references),(SELECT count(*) FROM audit)`, room).Scan(&got[0], &got[1], &got[2], &got[3], &got[4]); err != nil {
			t.Fatal(err)
		}
		return got
	}
	before := counts()
	if before[3] != 3 {
		t.Fatal("source and two explicit quotes did not each retain one reference")
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/history", one.token, relay.HistoryQuery{ID: source.Result.Message.ID})
	var page relay.HistoryPage
	if status != 200 || json.Unmarshal(body, &page) != nil || len(page.Messages) != 1 || page.Messages[0].State != "delivering" || page.Messages[0].Receipt != "" || page.Messages[0].Review == nil || *page.Messages[0].Review != anchor || bytes.Contains(body, []byte(claimed.Result.StructuredClaim.Receipt)) || bytes.Contains(body, []byte("origin_user")) || bytes.Contains(body, []byte("event_encoding")) {
		t.Fatalf("history private evidence boundary: %d %s", status, body)
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/history", one.token, relay.HistoryQuery{Limit: 1})
	if status != 200 || json.Unmarshal(body, &page) != nil || page.Messages[0].ID != accepted.Result.Message.ID || !page.HasMore {
		t.Fatal("bounded history did not return latest quote")
	}
	status, body = grantsPGCall(t, f, native, http.MethodPost, base+"/history", one.token, relay.HistoryQuery{Limit: 1, Cursor: page.NextCursor})
	if status != 200 || json.Unmarshal(body, &page) != nil || page.Messages[0].Text != "human quoted response" || page.Messages[0].Quote == nil || page.Messages[0].Quote.Text != source.Result.Message.Text {
		t.Fatal("stable history cursor lost human quote")
	}
	for _, request := range []struct {
		path, token string
		query       relay.HistoryQuery
	}{{base + "/history", device.access, relay.HistoryQuery{}}, {"/enterprise/device/v1/rooms/foreign-history-room/relay/history", one.token, relay.HistoryQuery{}}, {base + "/history", one.token, relay.HistoryQuery{Cursor: "pending:0"}}} {
		status, body = grantsPGCall(t, f, native, http.MethodPost, request.path, request.token, request.query)
		if status < 400 || bytes.Contains(body, []byte("full original body")) {
			t.Fatal("invalid history authority/view released content")
		}
	}
	if counts() != before {
		t.Fatal("history altered lease, Room sequence, receipts, references or audit")
	}
}
