package enterpriseservice

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// Hash only authoritative business rows, not private session maintenance or
// connection ownership observations. Never emit row bodies or credentials.
func exactObjectDomain(t *testing.T, f *httpPGFixture) [32]byte {
	t.Helper()
	h := sha256.New()
	for _, table := range []string{"rooms", "room_members", "room_events", "operations", "relay_operations", "audit", "attachments", "attachment_operations", "attachment_references", "attachment_usage"} {
		var data []byte
		if f.admin.QueryRow(t.Context(), `SELECT COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb) FROM `+table+` t`).Scan(&data) != nil {
			t.Fatalf("business state hash unavailable for %s", table)
		}
		h.Write([]byte(table))
		h.Write(data)
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

func exactObjectReference(t *testing.T, f *httpPGFixture, c *http.Client, csrf, room, id string) {
	t.Helper()
	status, _ := f.post(c, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": "exact-object-reference", "to": "slot1", "text": "original referenced evidence", "attachment_ids": []string{id}}, csrf)
	if status != 200 {
		t.Fatalf("actual reference failed: status=%d", status)
	}
}

func exactObjectReopen(t *testing.T, f *httpPGFixture, root string) *enterpriseobject.Store {
	t.Helper()
	if f.store.Close() != nil {
		t.Fatal("Store close failed")
	}
	store, err := sqlstore.Open(t.Context(), f.cfg)
	if err != nil {
		t.Fatal("Store reopen failed")
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err = store.RecoverDeliveries(t.Context()); err != nil {
		t.Fatal("Store recovery failed")
	}
	f.store = store
	f.installHandler(store)
	// Object Store has no resident connection/Close; Open reads the same durable
	// root and a new handle, without copying bytes or reconstructing SQL state.
	return attachmentFaultObjectRoot(t, f, root)
}

func TestObjectDeletionPendingActualStoreReopenResumesExactlyOnce(t *testing.T) {
	f := newObjectHTTPFixture(t)
	root := filepath.Join(t.TempDir(), "objects")
	objects := attachmentFaultObjectRoot(t, f, root)
	owner := f.browser("member-one")
	f.login(owner)
	o := f.session(owner)
	room := "gc-reopened-room"
	f.createRoom(owner, o.CSRF, room)
	retained := []byte("retained referenced bytes")
	abandoned := []byte("abandoned orphan bytes")
	live := objectHTTPReserve(t, f, owner, room, "retain-reserve", retained, "", o.CSRF)
	objectHTTPFinish(t, f, owner, room, "retain-verify", live, retained, "", o.CSRF)
	exactObjectReference(t, f, owner, o.CSRF, room, live.ObjectID)
	orphan := objectHTTPReserve(t, f, owner, room, "orphan-reserve", abandoned, "", o.CSRF)
	objectHTTPFinish(t, f, owner, room, "orphan-verify", orphan, abandoned, "", o.CSRF)
	auth, err := f.store.AuthenticateSession(t.Context(), tokenHash(f.cookie(owner).Value))
	if err != nil {
		t.Fatal("actual session unavailable")
	}
	command := sqlstore.AttachmentObjectCommand{Auth: auth.Auth, RoomID: room, ClientID: "cancel-orphan-original", ObjectID: orphan.ObjectID}
	cancelled, err := f.store.CancelAttachment(t.Context(), command)
	if err != nil || cancelled.Outcome != sqlstore.OutcomeCommitted {
		t.Fatal("typed cancellation failed")
	}
	// Test clock only: bypass the immutable timestamp trigger to cross the
	// existing seven-day retention boundary, without changing eligibility/ACL.
	if _, err = f.admin.Exec(t.Context(), `ALTER TABLE attachments DISABLE TRIGGER attachments_times_once; UPDATE attachments SET abandoned_at=clock_timestamp()-interval '8 days' WHERE state='abandoned'; ALTER TABLE attachments ENABLE TRIGGER attachments_times_once`); err != nil {
		t.Fatal("test retention clock failed")
	}
	before := attachmentFaultState(t, f, room, orphan.ObjectID)
	// Disk-only gate deliberately allows the SQL mark but blocks physical Delete.
	if objects.BeginBackup(t.Context(), "disk-gate-original") != nil {
		t.Fatal("disk retention gate failed")
	}
	report, err := f.store.CollectAttachments(t.Context(), objects, 32)
	marked := attachmentFaultState(t, f, room, orphan.ObjectID)
	if !errors.Is(err, enterpriseobject.ErrBackup) || report.Deleted != 0 || marked.state != "deletion_pending" || marked.bytes != before.bytes || marked.references != 1 {
		t.Fatal("real deletion_pending boundary not retained")
	}
	if _, err = objects.Inspect(t.Context(), orphan.ObjectID); err != nil {
		t.Fatal("blocked deletion removed orphan bytes")
	}
	objects = exactObjectReopen(t, f, root)
	if reopened := attachmentFaultState(t, f, room, orphan.ObjectID); reopened != marked {
		t.Fatal("reopen changed durable pending state")
	}
	if objects.EndBackup(t.Context(), "disk-gate-original") != nil {
		t.Fatal("original disk gate release failed")
	}
	report, err = f.store.CollectAttachments(t.Context(), objects, 32)
	after := attachmentFaultState(t, f, room, orphan.ObjectID)
	if err != nil || report.Deleted != 1 || after.bytes != before.bytes-int64(len(abandoned)) || after.references != 1 {
		t.Fatal("restarted GC did not release orphan quota exactly once")
	}
	if _, err = objects.Inspect(t.Context(), orphan.ObjectID); !errors.Is(err, enterpriseobject.ErrDeleted) {
		t.Fatal("orphan bytes survived completed deletion")
	}
	if _, err = os.Stat(filepath.Join(root, orphan.ObjectID, "data")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted tombstone retained physical data")
	}
	path := "/enterprise/api/v1/rooms/" + room + "/attachments/" + live.ObjectID + "/content"
	status, body, _, err := objectHTTPRaw(t, f, owner, http.MethodGet, path, nil, "", "", nil)
	if err != nil || status != 200 || !bytes.Equal(body, retained) {
		t.Fatal("restarted GC damaged referenced bytes")
	}
	report, err = f.store.CollectAttachments(t.Context(), objects, 32)
	if err != nil || report.Deleted != 0 || attachmentFaultState(t, f, room, orphan.ObjectID) != after {
		t.Fatal("repeat GC double released quota or touched reference")
	}
	replay, err := f.store.ResolveAttachmentCancellation(t.Context(), command)
	if err != nil || replay.Outcome != sqlstore.OutcomeReplayed || replay.Attachment.ObjectID != cancelled.Attachment.ObjectID {
		t.Fatal("original cancellation recovery lost after restart")
	}
	t.Log("actual SQLStore close/reopen and object-root Open: durable deletion_pending resumed; 1 orphan deleted; referenced bytes preserved; repeat GC 0 and quota released once")
}

func TestObjectReferencedMissingAndReadDeniedHTTPRechecksAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "read-denied"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "read-denied" && runtime.GOOS != "windows" {
				t.Skip("actual Windows owned-file DACL boundary")
			}
			f := newObjectHTTPFixture(t)
			root := filepath.Join(t.TempDir(), "objects")
			attachmentFaultObjectRoot(t, f, root)
			owner, member := f.browser("member-one"), f.browser("member-two")
			f.login(owner)
			f.login(member)
			o, m := f.session(owner), f.session(member)
			room := "inaccessible-reference-room"
			f.createRoom(owner, o.CSRF, room)
			objectHTTPAdmit(t, f, owner, o.CSRF, room, m.UserID)
			data := bytes.Repeat([]byte("private referenced dependency body;"), 2200)
			reserved := objectHTTPReserve(t, f, owner, room, "dependency-reserve", data, "", o.CSRF)
			objectHTTPFinish(t, f, owner, room, "dependency-verify", reserved, data, "", o.CSRF)
			exactObjectReference(t, f, owner, o.CSRF, room, reserved.ObjectID)
			path := "/enterprise/api/v1/rooms/" + room + "/attachments/" + reserved.ObjectID + "/content"
			status, body, _, err := objectHTTPRaw(t, f, member, http.MethodGet, path, nil, "", "", map[string]string{"Range": "bytes=0-63"})
			if err != nil || status != 206 || !bytes.Equal(body, data[:64]) {
				t.Fatal("authorized positive Range missing")
			}
			before := exactObjectDomain(t, f)
			dataPath := filepath.Join(root, reserved.ObjectID, "data")
			if mode == "missing" {
				hold := dataPath + ".test-held"
				if os.Rename(dataPath, hold) != nil {
					t.Fatal("actual byte removal failed")
				}
				t.Cleanup(func() { _ = os.Rename(hold, dataPath) })
				if _, err = os.Open(dataPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing-byte positive control failed")
				}
			} else {
				principal, e := user.Current()
				if e != nil {
					t.Fatal("owned principal unavailable")
				}
				if exec.Command("icacls", dataPath, "/deny", "*"+principal.Uid+":(R)").Run() != nil {
					t.Fatal("owned file DACL deny failed")
				}
				t.Cleanup(func() { _ = exec.Command("icacls", dataPath, "/remove:d", "*"+principal.Uid).Run() })
				file, e := os.Open(dataPath)
				if file != nil {
					_ = file.Close()
				}
				if !errors.Is(e, os.ErrPermission) {
					t.Fatal("actual unreadable-byte positive control failed")
				}
			}
			status, body, headers, err := objectHTTPRaw(t, f, member, http.MethodGet, path, nil, "", "", map[string]string{"Range": "bytes=0-63"})
			if err != nil || status != 503 || !bytes.Equal(bytes.TrimSpace(body), []byte(`{"error":"object_unavailable"}`)) || bytes.Contains(body, data[:32]) || headers.Get("Content-Range") != "" || exactObjectDomain(t, f) != before {
				t.Fatal("missing/inaccessible referenced bytes leaked cache or changed business state")
			}
			status, view := f.get(owner, "/enterprise/api/v1/rooms/"+room)
			var v struct {
				Revision uint64 `json:"revision"`
			}
			if status != 200 || json.Unmarshal(view, &v) != nil {
				t.Fatal("Owner revision unavailable")
			}
			status, _ = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/memberships", map[string]any{"client_id": "remove-inaccessible-member", "expected_revision": v.Revision, "intent": map[string]string{"action": "remove", "target_user_id": m.UserID}}, o.CSRF)
			if status != 200 {
				t.Fatal("typed Member removal failed")
			}
			revoked := exactObjectDomain(t, f)
			status, body, _, err = objectHTTPRaw(t, f, member, http.MethodGet, path, nil, "", "", nil)
			if err != nil || status != 403 || !bytes.Contains(body, []byte(`"class":"authorization"`)) || bytes.Contains(body, data[:32]) || exactObjectDomain(t, f) != revoked {
				t.Fatal("dependency failure bypassed current revocation or disclosed old bytes")
			}
			t.Log("positive authorized Range, actual filesystem failure, generic 503 with no body/effects, then typed revocation 403 before physical bytes")
		})
	}
}

// A transport cut severs both existing lease/work sockets and stops accepting
// replacements. The assertion connection remains direct; no Backend is mocked.
type exactDependencyProxy struct {
	mu       sync.Mutex
	listener net.Listener
	conns    []net.Conn
	cut      bool
	done     chan struct{}
}

func exactStartDependencyProxy(t *testing.T, dsn string) (*exactDependencyProxy, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal("DSN parse unavailable")
	}
	target := u.Host
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("dependency proxy unavailable")
	}
	p := &exactDependencyProxy{listener: listener, done: make(chan struct{})}
	u.Host = listener.Addr().String()
	go func() {
		defer close(p.done)
		for {
			client, e := listener.Accept()
			if e != nil {
				return
			}
			server, e := net.DialTimeout("tcp", target, 3*time.Second)
			if e != nil {
				_ = client.Close()
				continue
			}
			p.mu.Lock()
			if p.cut {
				_ = client.Close()
				_ = server.Close()
				p.mu.Unlock()
				continue
			}
			p.conns = append(p.conns, client, server)
			p.mu.Unlock()
			go func() {
				defer client.Close()
				defer server.Close()
				go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
				_, _ = io.Copy(client, server)
			}()
		}
	}()
	t.Cleanup(p.close)
	return p, u.String()
}
func (p *exactDependencyProxy) close() {
	p.mu.Lock()
	p.cut = true
	_ = p.listener.Close()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	<-p.done
}

func TestObjectHTTPActualPostgreSQLDependencyCutDisclosesNoCachedState(t *testing.T) {
	f := newObjectHTTPFixture(t)
	root := filepath.Join(t.TempDir(), "objects")
	attachmentFaultObjectRoot(t, f, root)
	proxy, dsn := exactStartDependencyProxy(t, f.cfg.DSN)
	f.cfg.DSN = dsn
	exactObjectReopen(t, f, root)
	owner := f.browser("member-one")
	f.login(owner)
	o := f.session(owner)
	room := "db-dependency-room"
	f.createRoom(owner, o.CSRF, room)
	data := []byte("unique prior private dependency bytes")
	reserved := objectHTTPReserve(t, f, owner, room, "db-cut-reserve", data, "", o.CSRF)
	objectHTTPFinish(t, f, owner, room, "db-cut-verify", reserved, data, "", o.CSRF)
	exactObjectReference(t, f, owner, o.CSRF, room, reserved.ObjectID)
	path := "/enterprise/api/v1/rooms/" + room + "/attachments/" + reserved.ObjectID + "/content"
	status, body, _, err := objectHTTPRaw(t, f, owner, http.MethodGet, path, nil, "", "", nil)
	if err != nil || status != 200 || !bytes.Equal(body, data) {
		t.Fatal("warm authorized bytes missing")
	}
	status, body = f.get(owner, "/enterprise/api/v1/rooms/"+room)
	if status != 200 || !bytes.Contains(body, []byte("original referenced evidence")) {
		t.Fatal("warm history positive missing")
	}
	before := exactObjectDomain(t, f)
	proxy.close()
	for _, route := range []string{"/enterprise/api/v1/rooms/" + room, path} {
		status, body = f.get(owner, route)
		if status != 503 || !bytes.Equal(bytes.TrimSpace(body), []byte(`{"error":"session_unavailable"}`)) || bytes.Contains(body, data) || bytes.Contains(body, []byte("original referenced evidence")) {
			t.Fatalf("actual DB cut did not fail closed: status=%d", status)
		}
	}
	status, body = f.post(owner, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": "no-effect-during-db-cut", "to": "slot1", "text": "must remain absent"}, o.CSRF)
	if status != 503 || !bytes.Equal(bytes.TrimSpace(body), []byte(`{"error":"session_unavailable"}`)) || exactObjectDomain(t, f) != before {
		t.Fatal("actual DB cut admitted mutation or changed domain")
	}
	t.Log("actual lease/work socket cut plus replacement listener unavailable: warmed history/object GET and new mutation each 503 session_unavailable; direct SQL business state unchanged")
}
