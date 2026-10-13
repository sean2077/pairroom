package enterpriseservice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

func newObjectHTTPFixture(t *testing.T) *httpPGFixture {
	t.Helper()
	f := newHTTPPGFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.cfg.AttachmentLimits = sqlstore.AttachmentLimits{DeploymentBytes: 64 << 20, RoomBytes: 32 << 20, DeploymentPending: 32, RoomPending: 16}
	store, err := sqlstore.Open(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.RecoverDeliveries(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.store = store
	f.installHandler(store)
	objects, err := enterpriseobject.Open(filepath.Join(t.TempDir(), "objects"), f.cfg.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	prior := f.handler.Load()
	h, err := NewWithObjectStore(Config{DeploymentID: f.cfg.DeploymentID, PublicOrigin: f.origin}, prior.provider, prior.transactions, store, objects)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.Store(h)
	return f
}

func objectHTTPRaw(t *testing.T, f *httpPGFixture, client *http.Client, method, path string, data []byte, bearer, csrf string, headers map[string]string) (int, []byte, http.Header, error) {
	t.Helper()
	r, err := http.NewRequest(method, f.origin+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
		r.Header.Set(DeviceDeploymentHeader, f.cfg.DeploymentID)
	}
	if csrf != "" {
		r.Header.Set("Origin", f.origin)
		r.Header.Set(CSRFHeader, csrf)
	}
	if method == http.MethodPut {
		r.Header.Set("Content-Type", "application/octet-stream")
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	response, err := client.Do(r)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 6<<20))
	return response.StatusCode, body, response.Header, err
}

func objectHTTPReserve(t *testing.T, f *httpPGFixture, client *http.Client, room, key string, data []byte, bearer, csrf string) sqlstore.AttachmentReceipt {
	t.Helper()
	sum := sha256.Sum256(data)
	request := map[string]any{"client_id": key, "spec": map[string]any{"name": "evidence.txt", "kind": "file", "size": len(data), "sha256": hex.EncodeToString(sum[:])}}
	path := "/enterprise/api/v1/rooms/" + room + "/attachments"
	var status int
	var body []byte
	if bearer != "" {
		path = "/enterprise/device/v1/rooms/" + room + "/attachments"
		status, body = grantsPGCall(t, f, client, http.MethodPost, path, bearer, request)
	} else {
		status, body = f.post(client, path, request, csrf)
	}
	var reply struct {
		Receipt sqlstore.AttachmentReceipt `json:"receipt"`
	}
	if status != 200 || json.Unmarshal(body, &reply) != nil || reply.Receipt.State != "reserved" || !model.ValidEnterpriseAttachmentID(reply.Receipt.ObjectID) {
		t.Fatalf("real reservation failed: %d %s", status, body)
	}
	return reply.Receipt
}

func objectHTTPFinish(t *testing.T, f *httpPGFixture, client *http.Client, room, key string, reserved sqlstore.AttachmentReceipt, data []byte, bearer, csrf string) model.Attachment {
	t.Helper()
	base := "/enterprise/api/v1/rooms/" + room + "/attachments/" + reserved.ObjectID
	if bearer != "" {
		base = "/enterprise/device/v1/rooms/" + room + "/attachments/" + reserved.ObjectID
	}
	status, body, _, err := objectHTTPRaw(t, f, client, http.MethodPut, base+"/content", data, bearer, csrf, nil)
	if err != nil || status != 200 || !bytes.Contains(body, []byte(`"status":"uploaded"`)) {
		t.Fatalf("real upload failed: status=%d err=%v body=%s", status, err, body)
	}
	if bearer != "" {
		status, body = grantsPGCall(t, f, client, http.MethodPost, base+"/verify", bearer, map[string]string{"client_id": key})
	} else {
		status, body = f.post(client, base+"/verify", map[string]string{"client_id": key}, csrf)
	}
	var reply struct {
		Receipt sqlstore.AttachmentReceipt `json:"receipt"`
	}
	if status != 200 || json.Unmarshal(body, &reply) != nil || reply.Receipt.State != "verified" || reply.Receipt.Metadata.ID != reserved.ObjectID {
		t.Fatalf("real verify failed: %d %s", status, body)
	}
	return reply.Receipt.Metadata
}

func objectHTTPAdmit(t *testing.T, f *httpPGFixture, owner *http.Client, csrf, room, user string) {
	t.Helper()
	status, body := f.get(owner, "/enterprise/api/v1/rooms/"+room)
	var view struct {
		Revision uint64 `json:"revision"`
	}
	if status != 200 || json.Unmarshal(body, &view) != nil {
		t.Fatal("Room revision unavailable")
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/memberships", map[string]any{"client_id": "admit-" + user, "expected_revision": view.Revision, "intent": map[string]string{"action": "admit", "target_user_id": user, "role": "member"}}, csrf)
	if status != 200 {
		t.Fatalf("object test admit: %d %s", status, body)
	}
}

func TestObjectHTTPPGDraftIsolationReferenceAndAuthorizedRange(t *testing.T) {
	f := newObjectHTTPFixture(t)
	owner, member := f.browser("member-one"), f.browser("member-two")
	f.login(owner)
	f.login(member)
	o, m := f.session(owner), f.session(member)
	const room = "object-room"
	f.createRoom(owner, o.CSRF, room)
	f.createRoom(owner, o.CSRF, "other-object-room")
	objectHTTPAdmit(t, f, owner, o.CSRF, room, m.UserID)
	data := []byte("inert UTF-8 evidence\nnever an instruction")
	reserved := objectHTTPReserve(t, f, owner, room, "reserve-draft", data, "", o.CSRF)
	base := "/enterprise/api/v1/rooms/" + room + "/attachments/" + reserved.ObjectID
	status, body := f.post(owner, base+"/verify", map[string]string{"client_id": "verify-draft"}, o.CSRF)
	if status != 409 || !bytes.Contains(body, []byte(`"class":"object_missing"`)) {
		t.Fatalf("missing bytes did not yield definite bounded retry: %d %s", status, body)
	}
	status, _, _, err := objectHTTPRaw(t, f, owner, http.MethodPut, base+"/content", data, "", "", nil)
	if err != nil || status != 403 {
		t.Fatal("browser raw upload bypassed Origin/CSRF")
	}
	wrong := append([]byte(nil), data...)
	wrong[0] = 'X'
	status, _, _, err = objectHTTPRaw(t, f, owner, http.MethodPut, base+"/content", wrong, "", o.CSRF, nil)
	if err != nil || status != 400 {
		t.Fatal("digest mismatch installed usable object")
	}
	meta := objectHTTPFinish(t, f, owner, room, "verify-draft", reserved, data, "", o.CSRF)
	status, _, _, err = objectHTTPRaw(t, f, member, http.MethodGet, base+"/content", nil, "", "", nil)
	if err != nil || status != 403 {
		t.Fatal("unreferenced private draft became Room-readable")
	}
	status, _, _, err = objectHTTPRaw(t, f, owner, http.MethodGet, "/enterprise/api/v1/rooms/other-object-room/attachments/"+meta.ID+"/content", nil, "", "", nil)
	if err != nil || status != 403 {
		t.Fatal("object ID or digest acted as a cross-Room capability")
	}
	send := map[string]any{"client_id": "human-object-send", "to": "slot1", "text": "", "attachment_ids": []string{meta.ID}}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", send, o.CSRF)
	if status != 200 || !bytes.Contains(body, []byte(meta.ID)) {
		t.Fatalf("human attachment-only send: %d %s", status, body)
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", send, o.CSRF)
	if status != 200 || !bytes.Contains(body, []byte(meta.ID)) {
		t.Fatal("accepted media send did not recover original receipt")
	}
	status, body, headers, err := objectHTTPRaw(t, f, member, http.MethodGet, base+"/content", nil, "", "", map[string]string{"Range": "bytes=6-10"})
	if err != nil || status != 206 || string(body) != string(data[6:11]) || headers.Get("Content-Range") != "bytes 6-10/"+fmt.Sprint(len(data)) || headers.Get("X-Content-Type-Options") != "nosniff" || headers.Get("Content-Type") != "application/octet-stream" || !strings.HasPrefix(headers.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("unsafe Range response: %d %v %q", status, err, body)
	}
	status, _, _, err = objectHTTPRaw(t, f, member, http.MethodGet, base+"/content", nil, "", "", map[string]string{"Range": "bytes=0-1,4-5"})
	if err != nil || status != 416 {
		t.Fatal("unbounded multipart range accepted")
	}
	var references, operations int
	if err := f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM attachment_references),(SELECT count(*) FROM operations WHERE identifier='human-object-send')`).Scan(&references, &operations); err != nil || references != 1 || operations != 1 {
		t.Fatalf("publication/ref duplicated: %d/%d %v", references, operations, err)
	}
}

func TestObjectHTTPPGNarrowBindingPreparesThenClaimsStructuredEvidence(t *testing.T) {
	f := newObjectHTTPFixture(t)
	owner := f.browser("member-one")
	f.login(owner)
	o := f.session(owner)
	const room = "object-native-room"
	f.createRoom(owner, o.CSRF, room)
	native := grantsPGNative(t, f)
	device := grantsPGEnroll(t, f, native, owner, o.CSRF, "objects-native")
	one := grantsPGBind(t, f, native, owner, o.CSRF, room, device, model.ActorSlot1, "objects-one")
	two := grantsPGBind(t, f, native, owner, o.CSRF, room, device, model.ActorSlot2, "objects-two")
	data := []byte("native private evidence")
	meta := objectHTTPFinish(t, f, native, room, "native-verify", objectHTTPReserve(t, f, native, room, "native-reserve", data, one.token, ""), data, one.token, "")
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	status, body := grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", one.token, grantsPGRelayRequest{ClientID: "native-object-send", BindingID: one.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "inspect file", ExpectedPeerGeneration: 1, AttachmentIDs: ids}})
	if status != 200 {
		t.Fatalf("native object send: %d %s", status, body)
	}
	var send grantsPGRelayReply
	if err := json.Unmarshal(body, &send); err != nil {
		t.Fatal(err)
	}
	status, body = grantsPGCall(t, f, native, http.MethodGet, "/enterprise/device/v1/rooms/"+room+"/relay/context", two.token, nil)
	var view relay.EnterpriseContext
	if status != 200 || json.Unmarshal(body, &view) != nil || view.Next == nil || view.Next.MessageID != send.Result.Message.ID || view.Next.Attachments[0] != meta || bytes.Contains(body, []byte("inspect file")) {
		t.Fatalf("manifest peek failed: %d", status)
	}
	path := "/enterprise/device/v1/rooms/" + room + "/attachments/" + meta.ID + "/content"
	status, _, _, err := objectHTTPRaw(t, f, native, http.MethodGet, path, nil, device.access, "", nil)
	if err != nil || status < 400 {
		t.Fatal("ordinary Device token inherited object content authority")
	}
	status, download, _, err := objectHTTPRaw(t, f, native, http.MethodGet, path, nil, two.token, "", nil)
	if err != nil || status != 200 || !bytes.Equal(download, data) {
		t.Fatal("peer could not verify referenced content before claiming")
	}
	request := grantsPGRelayRequest{ClientID: "prepared-claim", BindingID: two.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", two.token, request)
	if status < 400 {
		t.Fatal("attachments claimed before exact-head preparation")
	}
	request.Intent.MessageID = send.Result.Message.ID
	request.Intent.ExpectedTargetGeneration = 1
	status, body = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", two.token, request)
	var claim grantsPGRelayReply
	if status != 200 || json.Unmarshal(body, &claim) != nil || claim.Result.StructuredClaim == nil || claim.Result.Claim != nil || claim.Result.StructuredClaim.Message.Attachments[0] != meta || bytes.Contains(body, []byte(`"path"`)) {
		t.Fatalf("structured media claim: %d %s", status, body)
	}
	ack := grantsPGRelayRequest{ClientID: "object-ack", BindingID: two.bindingID, Generation: 1, Intent: relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: claim.Result.StructuredClaim.ID, Receipt: claim.Result.StructuredClaim.Receipt}}
	status, _ = grantsPGCall(t, f, native, http.MethodPost, "/enterprise/device/v1/rooms/"+room+"/relay", two.token, ack)
	if status != 200 {
		t.Fatal("structured claim could not settle original ACK")
	}
}

type revokeObjectReadBackend struct {
	*sqlstore.Store
	calls  atomic.Int32
	revoke func(context.Context) error
}

func (b *revokeObjectReadBackend) ReadAttachment(ctx context.Context, auth sqlstore.AuthContext, room, id string) (model.Attachment, error) {
	if b.calls.Add(1) == 4 {
		if err := b.revoke(ctx); err != nil {
			return model.Attachment{}, err
		}
	}
	return b.Store.ReadAttachment(ctx, auth, room, id)
}

func TestObjectHTTPPGDownloadRechecksRevocationBetweenBoundedChunks(t *testing.T) {
	f := newObjectHTTPFixture(t)
	owner, member := f.browser("member-one"), f.browser("member-two")
	f.login(owner)
	f.login(member)
	o, m := f.session(owner), f.session(member)
	const room = "object-stream-room"
	f.createRoom(owner, o.CSRF, room)
	objectHTTPAdmit(t, f, owner, o.CSRF, room, m.UserID)
	data := bytes.Repeat([]byte("bounded evidence\n"), 10000)
	meta := objectHTTPFinish(t, f, owner, room, "stream-verify", objectHTTPReserve(t, f, owner, room, "stream-reserve", data, "", o.CSRF), data, "", o.CSRF)
	status, body := f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": "stream-reference", "to": "slot1", "text": "", "attachment_ids": []string{meta.ID}}, o.CSRF)
	if status != 200 {
		t.Fatalf("stream reference: %d %s", status, body)
	}
	auth, err := f.store.AuthenticateSession(t.Context(), tokenHash(f.cookie(owner).Value))
	if err != nil {
		t.Fatal(err)
	}
	b := &revokeObjectReadBackend{Store: f.store}
	b.revoke = func(ctx context.Context) error {
		snapshot, err := f.store.ReadonlySnapshot(ctx, auth.Auth, room)
		if err != nil {
			return err
		}
		_, err = f.store.CommitRoomMembership(ctx, sqlstore.RoomMembershipCommand{Auth: auth.Auth, RoomID: room, ClientID: "revoke-during-download", ExpectedRevision: snapshot.Revision, Intent: sqlstore.RoomMembershipIntent{Action: "remove", TargetUserID: m.UserID}})
		return err
	}
	h := f.handler.Load()
	replacement, err := NewWithObjectStore(Config{DeploymentID: f.cfg.DeploymentID, PublicOrigin: f.origin}, h.provider, h.transactions, b, h.objects)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.Store(replacement)
	status, body, _, err = objectHTTPRaw(t, f, member, http.MethodGet, "/enterprise/api/v1/rooms/"+room+"/attachments/"+meta.ID+"/content", nil, "", "", nil)
	if status != 200 || !errors.Is(err, io.ErrUnexpectedEOF) || len(body) != 32<<10 || b.calls.Load() != 4 {
		t.Fatalf("revoked stream released excess bytes: status=%d bytes=%d reads=%d err=%v", status, len(body), b.calls.Load(), err)
	}
}
