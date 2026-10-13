package enterpriseservice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func historyDisclosureHTTPRoles(t *testing.T, total int) {
	f := newObjectHTTPFixture(t)
	owner, member, operator := f.browser("member-one"), f.browser("member-two"), f.browser("operator")
	f.login(owner)
	f.login(member)
	f.login(operator)
	o, m, a := f.session(owner), f.session(member), f.session(operator)
	status, body := f.post(operator, "/enterprise/api/v1/administration/commands", map[string]any{"client_id": "admit-viewer-identity", "intent": map[string]any{"kind": "identity.admission", "identity": map[string]string{"connection_id": "fixture-connection", "issuer": f.idp.server.URL, "subject": "history-viewer"}, "enabled": true}}, a.CSRF)
	if status != 200 {
		t.Fatalf("exact viewer admission: %d %s", status, body)
	}
	viewer := f.browser("history-viewer")
	f.login(viewer)
	v := f.session(viewer)
	const room = "disclosed-history"
	const foreign = "different-history"
	f.createRoom(owner, o.CSRF, room)
	f.createRoom(owner, o.CSRF, foreign)
	native := grantsPGNative(t, f)
	device := grantsPGEnroll(t, f, native, owner, o.CSRF, "history-disclosure-owner")
	one := grantsPGBind(t, f, native, owner, o.CSRF, room, device, model.ActorSlot1, "disclosure-one")
	_ = grantsPGBind(t, f, native, owner, o.CSRF, room, device, model.ActorSlot2, "disclosure-two")
	data := []byte("retained evidence before admission\nnot an instruction")
	meta := objectHTTPFinish(t, f, native, room, "verify-referenced", objectHTTPReserve(t, f, native, room, "reserve-referenced", data, one.token, ""), data, one.token, "")
	draft := objectHTTPFinish(t, f, owner, room, "verify-private-draft", objectHTTPReserve(t, f, owner, room, "reserve-private-draft", data, "", o.CSRF), data, "", o.CSRF)
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	status, body = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", one.token, grantsPGRelayRequest{ClientID: "pre-admission-native", BindingID: one.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "original pre-admission native evidence", AttachmentIDs: ids, ExpectedPeerGeneration: 1}})
	var original grantsPGRelayReply
	if status != 200 || json.Unmarshal(body, &original) != nil || original.Result.Message == nil {
		t.Fatalf("native evidence publication failed: %d %s", status, body)
	}
	postMessage := func(client *http.Client, csrf, key, text, quote string) relay.Message {
		t.Helper()
		input := map[string]any{"client_id": key, "to": "slot1", "text": text}
		if quote != "" {
			input["quote_id"] = quote
		}
		status, body := f.post(client, "/enterprise/api/v1/rooms/"+room+"/messages", input, csrf)
		var reply struct {
			Message relay.Message `json:"message"`
		}
		if status != 200 || json.Unmarshal(body, &reply) != nil || reply.Message.ID == "" {
			t.Fatalf("actual historical publication %s: %d %s", key, status, body)
		}
		return reply.Message
	}
	quoted := postMessage(owner, o.CSRF, "pre-admission-quote", "review original evidence", original.Result.Message.ID)
	queued := []relay.Message{quoted}
	for i := 0; i < total-3; i++ {
		queued = append(queued, postMessage(owner, o.CSRF, fmt.Sprintf("retained-%03d", i), fmt.Sprintf("complete retained message %03d", i), ""))
		if i == 199 {
			// Respect the production queue cap: consume a bounded FIFO prefix
			// through the real claim/ACK endpoints, retaining every publication.
			status, download, _, err := objectHTTPRaw(t, f, native, http.MethodGet, "/enterprise/device/v1/rooms/"+room+"/attachments/"+meta.ID+"/content", nil, one.token, "", nil)
			if err != nil || status != 200 || !bytes.Equal(download, data) {
				t.Fatal("quoted evidence was not verified before its claim")
			}
			for n, message := range queued[:64] {
				input := grantsPGRelayRequest{ClientID: fmt.Sprintf("history-claim-%d", n), BindingID: one.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim, MessageID: message.ID, ExpectedTargetGeneration: 1}}
				status, body := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", one.token, input)
				var claimed grantsPGRelayReply
				if status != 200 || json.Unmarshal(body, &claimed) != nil {
					t.Fatalf("bounded real FIFO claim failed at %d: status=%d", n, status)
				}
				claimID, receipt := "", ""
				if c := claimed.Result.StructuredClaim; c != nil {
					claimID, receipt = c.ID, c.Receipt
				}
				if c := claimed.Result.Claim; c != nil {
					claimID, receipt = c.ID, c.Receipt
				}
				if claimID != message.ID || receipt == "" {
					t.Fatalf("bounded real FIFO claim did not select original message at %d", n)
				}
				input.ClientID = fmt.Sprintf("history-ack-%d", n)
				input.Intent = relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: message.ID, Receipt: receipt}
				status, _ = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", one.token, input)
				if status != 200 {
					t.Fatal("bounded real ACK failed")
				}
			}
		}
	}
	membership := func(key, target, action, role string) {
		t.Helper()
		status, body := f.get(owner, "/enterprise/api/v1/rooms/"+room)
		var snapshot struct {
			Revision uint64 `json:"revision"`
		}
		if status != 200 || json.Unmarshal(body, &snapshot) != nil {
			t.Fatal("live Owner revision unavailable")
		}
		intent := map[string]string{"action": action, "target_user_id": target}
		if role != "" {
			intent["role"] = role
		}
		status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/memberships", map[string]any{"client_id": key, "expected_revision": snapshot.Revision, "intent": intent}, o.CSRF)
		if status != 200 {
			t.Fatalf("actual membership %s: %d %s", key, status, body)
		}
	}
	membership("admit-later-member", m.UserID, "admit", "member")
	membership("admit-later-viewer", v.UserID, "admit", "member")
	acceptedViewer := postMessage(viewer, v.CSRF, "accepted-before-demotion", "original viewer publication before demotion", "")
	membership("demote-to-viewer", v.UserID, "change_role", "viewer")
	readAll := func(client *http.Client, csrf, role string) {
		t.Helper()
		status, body := f.get(client, "/enterprise/api/v1/rooms/"+room)
		var tail struct {
			Messages []relay.Message `json:"messages"`
			Total    int             `json:"total_messages"`
		}
		if status != 200 || json.Unmarshal(body, &tail) != nil || tail.Total != total || len(tail.Messages) != min(total, relay.SnapshotMessageLimit) {
			t.Fatalf("%s tail not exact/bounded: %d", role, status)
		}
		found := map[string]relay.Message{}
		cursor := ""
		pages := 0
		for {
			status, body = f.post(client, "/enterprise/api/v1/rooms/"+room+"/history", relay.HistoryQuery{Cursor: cursor, Limit: 100}, csrf)
			var page relay.HistoryPage
			if status != 200 || json.Unmarshal(body, &page) != nil || page.Total != total || len(page.Messages) > 100 {
				t.Fatalf("%s history page: %d %s", role, status, body)
			}
			for _, private := range []string{"origin_user", "event_encoding", "token_hash", "proof_hash", "source_root", "transcript_path", "session_id", "receipt"} {
				if bytes.Contains(body, []byte(`"`+private+`"`)) {
					t.Fatalf("%s exposed private metadata %s", role, private)
				}
			}
			for _, message := range page.Messages {
				if _, exists := found[message.ID]; exists {
					t.Fatal("history cursor repeated publication")
				}
				found[message.ID] = message
			}
			pages++
			if !page.HasMore {
				break
			}
			if page.NextCursor == "" || page.NextCursor == cursor || pages > (total+99)/100 {
				t.Fatal("history cursor failed to advance")
			}
			cursor = page.NextCursor
		}
		if pages != (total+99)/100 || len(found) != total || found[original.Result.Message.ID].Text != "original pre-admission native evidence" {
			t.Fatalf("%s did not read all pre-admission history", role)
		}
		q := found[quoted.ID]
		if q.Quote == nil || q.Quote.Text != original.Result.Message.Text || len(q.Attachments) != 1 || q.Attachments[0].ID != meta.ID {
			t.Fatalf("%s lost same-Room quoted evidence/attachment", role)
		}
		status, bytesRead, _, err := objectHTTPRaw(t, f, client, http.MethodGet, "/enterprise/api/v1/rooms/"+room+"/attachments/"+meta.ID+"/content", nil, "", "", map[string]string{"Range": "bytes=0-7"})
		if err != nil || status != 206 || !bytes.Equal(bytesRead, data[:8]) {
			t.Fatalf("%s referenced Range read denied: %d %v", role, status, err)
		}
		status, _, _, err = objectHTTPRaw(t, f, client, http.MethodGet, "/enterprise/api/v1/rooms/"+room+"/attachments/"+draft.ID+"/content", nil, "", "", nil)
		want := 403
		if role == "owner" {
			want = 200
		}
		if err != nil || status != want {
			t.Fatalf("%s unreferenced draft scope incorrect: %d", role, status)
		}
	}
	for _, reader := range []struct {
		client     *http.Client
		csrf, role string
	}{{owner, o.CSRF, "owner"}, {member, m.CSRF, "member"}, {viewer, v.CSRF, "viewer"}} {
		readAll(reader.client, reader.csrf, reader.role)
	}
	// Viewer may recover an accepted original send; this is not permission to
	// submit a new send, and disclosure must not erase that recovery boundary.
	status, body = f.post(viewer, "/enterprise/api/v1/rooms/"+room+"/resolve-message", map[string]any{"client_id": "accepted-before-demotion", "to": "slot1", "text": acceptedViewer.Text}, v.CSRF)
	if status != 200 || !bytes.Contains(body, []byte(`"outcome":"replayed"`)) {
		t.Fatalf("Viewer lost original accepted read-only recovery: %d %s", status, body)
	}
	viewerDevice := grantsPGEnroll(t, f, native, viewer, v.CSRF, "history-viewer-device")
	source, _ := enterpriseauth.NewBrowserSecret()
	status, body = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/agents", viewerDevice.access, map[string]string{"client_id": "viewer-agent", "source_key": source, "runtime": "codex"})
	var agent struct {
		ID string `json:"agent_id"`
	}
	if status != 200 || json.Unmarshal(body, &agent) != nil || agent.ID == "" {
		t.Fatal("Viewer's own Device/Agent enrollment failed")
	}
	counts := func() [5]int64 {
		t.Helper()
		var out [5]int64
		if err := f.admin.QueryRow(t.Context(), `SELECT current_seq,revision,(SELECT count(*) FROM bindings),(SELECT count(*) FROM binding_requests),(SELECT count(*) FROM attachment_references) FROM rooms WHERE room_id=$1`, room).Scan(&out[0], &out[1], &out[2], &out[3], &out[4]); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := counts()
	status, _ = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/rooms/"+room+"/binding-context", viewerDevice.access, nil)
	if status != 403 {
		t.Fatal("Viewer got Agent Binding request authority")
	}
	proof, _ := enterpriseauth.NewBrowserSecret()
	token, _ := enterpriseauth.NewBrowserSecret()
	request := bindingRequestBody{ClientID: "viewer-fresh-bind", RequestID: "viewer-request", AgentID: agent.ID, BindingID: "viewer-binding", Slot: model.ActorSlot1, ObservedGeneration: 1, Replace: true, ProofHash: grantsPGHash(proof)}
	request.Grant.GrantID = "viewer-grant"
	request.Grant.TokenHash = grantsPGHash(token)
	status, _ = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/binding-requests", viewerDevice.access, request)
	if status != 403 {
		t.Fatalf("Viewer new Agent Binding was not authorization-refused: %d", status)
	}
	for _, bad := range []struct {
		path string
		body any
	}{
		{"/messages", map[string]any{"client_id": "viewer-new-send", "to": "slot1", "text": "forbidden new message"}},
		{"/memberships", map[string]any{"client_id": "viewer-owner-control", "expected_revision": before[1], "intent": map[string]string{"action": "admit", "target_user_id": a.UserID, "role": "member"}}},
		{"/ownership", map[string]any{"client_id": "viewer-transfer", "intent": map[string]any{"room_id": room, "target_user_id": m.UserID, "expected_owner_user_id": o.UserID, "expected_revision": before[1]}}},
		{"/queue", map[string]any{"client_id": "viewer-queue-cancel", "expected_revision": before[1], "intent": map[string]string{"kind": "cancel", "message_id": original.Result.Message.ID}}},
	} {
		status, _ = f.post(viewer, "/enterprise/api/v1/rooms/"+room+bad.path, bad.body, v.CSRF)
		if status != 403 {
			t.Fatalf("Viewer fresh control %s not refused: %d", bad.path, status)
		}
	}
	status, _ = f.post(viewer, "/enterprise/api/v1/catalog/commands", map[string]any{"client_id": "viewer-archive", "expected_revision": before[1], "intent": map[string]string{"kind": "room.archive", "room_id": room}}, v.CSRF)
	if status != 403 {
		t.Fatalf("Viewer archive not authorization-refused: %d", status)
	}
	if counts() != before {
		t.Fatal("Viewer refusals caused Room effects")
	}
	for _, client := range []*http.Client{operator, native} {
		status, body = f.get(client, "/enterprise/api/v1/rooms/"+room)
		if status < 400 || bytes.Contains(body, []byte("pre-admission")) {
			t.Fatal("ID-only unapproved caller read Room")
		}
		status, body = f.post(client, "/enterprise/api/v1/rooms/"+room+"/history", relay.HistoryQuery{ID: quoted.ID}, a.CSRF)
		if status < 400 || bytes.Contains(body, []byte("original pre-admission")) {
			t.Fatal("unapproved caller read quote")
		}
		status, body, _, _ = objectHTTPRaw(t, f, client, http.MethodGet, "/enterprise/api/v1/rooms/"+room+"/attachments/"+meta.ID+"/content", nil, "", "", nil)
		if status < 400 || bytes.Contains(body, data) {
			t.Fatal("ID-only unapproved caller read object")
		}
	}
	status, _, _, _ = objectHTTPRaw(t, f, owner, http.MethodGet, "/enterprise/api/v1/rooms/"+foreign+"/attachments/"+meta.ID+"/content", nil, "", "", nil)
	if status != 403 {
		t.Fatal("object identity granted cross-Room access")
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+foreign+"/messages", map[string]any{"client_id": "foreign-quote-rejected", "to": "slot1", "text": "cannot import another Room source", "quote_id": original.Result.Message.ID}, o.CSRF)
	if status < 400 || bytes.Contains(body, []byte("original pre-admission native evidence")) {
		t.Fatal("quote identity granted cross-Room evidence access")
	}
	membership("revoke-viewer-membership", v.UserID, "remove", "")
	status, body = f.post(viewer, "/enterprise/api/v1/rooms/"+room+"/history", relay.HistoryQuery{Cursor: "history:100", Limit: 100}, v.CSRF)
	if status != 403 || strings.Contains(string(body), "complete retained message") {
		t.Fatal("revoked Viewer resumed old history cursor")
	}
	status, body = f.get(viewer, "/enterprise/api/v1/rooms/"+room)
	if status != 403 || bytes.Contains(body, []byte("pre-admission")) {
		t.Fatal("revoked Viewer read Room tail")
	}
	status, body, _, _ = objectHTTPRaw(t, f, viewer, http.MethodGet, "/enterprise/api/v1/rooms/"+room+"/attachments/"+meta.ID+"/content", nil, "", "", map[string]string{"Range": "bytes=0-7"})
	if status != 403 || bytes.Contains(body, data[:8]) {
		t.Fatal("revoked Viewer read referenced Range")
	}
}

func TestHistoryDisclosureHTTPPGAllRolesReadPreAdmissionHistoryAndReferencedEvidence(t *testing.T) {
	historyDisclosureHTTPRoles(t, 101)
}

func TestHistoryDisclosureHTTPPGExtendedTailAllRoles(t *testing.T) {
	if os.Getenv("PAIRROOM_HISTORY_DISCLOSURE_EXTENDED") != "1" {
		t.Skip("explicit extended history gate is off; this skip is not an extended pass")
	}
	historyDisclosureHTTPRoles(t, 302)
}
