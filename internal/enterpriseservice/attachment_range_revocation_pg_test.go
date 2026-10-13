package enterpriseservice

import (
	"bytes"
	"context"
	"errors"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"io"
	"net/http"
	"testing"
)

func TestObjectHTTPPGRangeRevocationStopsAtBoundedChunk(t *testing.T) {
	f := newObjectHTTPFixture(t)
	owner, member := f.browser("member-one"), f.browser("member-two")
	f.login(owner)
	f.login(member)
	o, m := f.session(owner), f.session(member)
	const room = "range-revocation-room"
	f.createRoom(owner, o.CSRF, room)
	objectHTTPAdmit(t, f, owner, o.CSRF, room, m.UserID)
	data := bytes.Repeat([]byte("r"), 160000)
	meta := objectHTTPFinish(t, f, owner, room, "range-verify", objectHTTPReserve(t, f, owner, room, "range-reserve", data, "", o.CSRF), data, "", o.CSRF)
	if status, body := f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": "range-reference", "to": "slot1", "text": "", "attachment_ids": []string{meta.ID}}, o.CSRF); status != 200 {
		t.Fatalf("reference: %d %s", status, body)
	}
	auth, err := f.store.AuthenticateSession(t.Context(), tokenHash(f.cookie(owner).Value))
	if err != nil {
		t.Fatal(err)
	}
	backend := &revokeObjectReadBackend{Store: f.store}
	backend.revoke = func(ctx context.Context) error {
		snap, err := f.store.ReadonlySnapshot(ctx, auth.Auth, room)
		if err != nil {
			return err
		}
		_, err = f.store.CommitRoomMembership(ctx, sqlstore.RoomMembershipCommand{Auth: auth.Auth, RoomID: room, ClientID: "remove-during-range", ExpectedRevision: snap.Revision, Intent: sqlstore.RoomMembershipIntent{Action: "remove", TargetUserID: m.UserID}})
		return err
	}
	prior := f.handler.Load()
	replacement, err := NewWithObjectStore(Config{DeploymentID: f.cfg.DeploymentID, PublicOrigin: f.origin}, prior.provider, prior.transactions, backend, prior.objects)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.Store(replacement)
	path := "/enterprise/api/v1/rooms/" + room + "/attachments/" + meta.ID + "/content"
	status, body, headers, err := objectHTTPRaw(t, f, member, http.MethodGet, path, nil, "", "", map[string]string{"Range": "bytes=1024-131071"})
	if status != http.StatusPartialContent || headers.Get("Content-Range") != "bytes 1024-131071/160000" || !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(body, data[1024:1024+(32<<10)]) || backend.calls.Load() != 4 {
		t.Fatalf("revoked Range released incorrect bytes: status=%d bytes=%d checks=%d error=%v", status, len(body), backend.calls.Load(), err)
	}
	status, _, _, err = objectHTTPRaw(t, f, member, http.MethodGet, path, nil, "", "", map[string]string{"Range": "bytes=0-1"})
	if err != nil || status != http.StatusForbidden {
		t.Fatal("removed member retained Range capability", status, err)
	}
}
