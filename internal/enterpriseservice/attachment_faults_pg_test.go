package enterpriseservice

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseobject"
)

type observedAttachmentBody struct {
	io.ReadCloser
	entered chan struct{}
	once    sync.Once
}

func (b *observedAttachmentBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	return b.ReadCloser.Read(p)
}

type attachmentFaultCounts struct {
	bytes, pending, references, seq, operations int64
	state                                       string
}

func attachmentFaultState(t *testing.T, f *httpPGFixture, room, id string) attachmentFaultCounts {
	t.Helper()
	var got attachmentFaultCounts
	err := f.admin.QueryRow(t.Context(), `SELECT (SELECT bytes FROM attachment_usage WHERE scope='room' AND scope_id=$1),(SELECT pending FROM attachment_usage WHERE scope='room' AND scope_id=$1),(SELECT count(*) FROM attachment_references WHERE room_id=$1),(SELECT current_seq FROM rooms WHERE room_id=$1),(SELECT count(*) FROM attachment_operations WHERE room_id=$1)+(SELECT count(*) FROM operations WHERE room_id=$1)+(SELECT count(*) FROM relay_operations WHERE room_id=$1),(SELECT state FROM attachments WHERE object_id=$2)`, room, id).Scan(&got.bytes, &got.pending, &got.references, &got.seq, &got.operations, &got.state)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func attachmentFaultObjectRoot(t *testing.T, f *httpPGFixture, root string) *enterpriseobject.Store {
	t.Helper()
	objects, err := enterpriseobject.Open(root, f.cfg.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	prior := f.handler.Load()
	h, err := NewWithObjectStore(Config{DeploymentID: f.cfg.DeploymentID, PublicOrigin: f.origin}, prior.provider, prior.transactions, f.store, objects)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.Store(h)
	return objects
}

func attachmentFaultRejectUnverified(t *testing.T, f *httpPGFixture, client *http.Client, csrf, room, id, key string, objects *enterpriseobject.Store, before attachmentFaultCounts) {
	t.Helper()
	if _, err := objects.Inspect(t.Context(), id); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete upload became a private verified object: %v", err)
	}
	base := "/enterprise/api/v1/rooms/" + room + "/attachments/" + id
	status, body := f.post(client, base+"/verify", map[string]string{"client_id": key + "-verify"}, csrf)
	if status != http.StatusConflict || !bytes.Contains(body, []byte(`"class":"object_missing"`)) {
		t.Fatalf("incomplete upload did not retain definite object_missing verification: %d %s", status, body)
	}
	status, body = f.post(client, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": key + "-reference", "to": "slot1", "text": "must not reference incomplete bytes", "attachment_ids": []string{id}}, csrf)
	if status != http.StatusForbidden || !bytes.Contains(body, []byte(`"class":"authorization"`)) {
		t.Fatalf("unverified reservation became a message reference: %d %s", status, body)
	}
	if after := attachmentFaultState(t, f, room, id); after != before || after.state != "reserved" || after.references != 0 {
		t.Fatalf("failed upload/verify/reference changed reservation, quota, facts or receipts: %+v -> %+v", before, after)
	}
}

func TestAttachmentHTTPActualShortBodyAndDisconnectDoNotVerifyOrReference(t *testing.T) {
	for _, mode := range []string{"tls-short-body", "tcp-reset"} {
		t.Run(mode, func(t *testing.T) {
			f := newObjectHTTPFixture(t)
			root := filepath.Join(t.TempDir(), "objects")
			objects := attachmentFaultObjectRoot(t, f, root)
			client := f.browser("member-one")
			f.login(client)
			session := f.session(client)
			room := "interrupted-upload-room"
			f.createRoom(client, session.CSRF, room)
			data := bytes.Repeat([]byte("i"), 64<<10)
			reserved := objectHTTPReserve(t, f, client, room, "interrupt-reserve", data, "", session.CSRF)
			before := attachmentFaultState(t, f, room, reserved.ObjectID)
			if before.bytes != int64(len(data)) || before.pending != 1 || before.state != "reserved" {
				t.Fatal("reservation did not charge its frozen size/pending quota")
			}
			entered, finished := make(chan struct{}), make(chan struct{})
			prior := f.server.Config.Handler
			address := f.server.Listener.Addr().String()
			certificate := f.server.TLS.Certificates[0]
			f.server.Close()
			observed := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut && r.URL.Path == "/enterprise/api/v1/rooms/"+room+"/attachments/"+reserved.ObjectID+"/content" {
					r.Body = &observedAttachmentBody{ReadCloser: r.Body, entered: entered}
					defer close(finished)
				}
				prior.ServeHTTP(w, r)
			}))
			_ = observed.Listener.Close()
			var err error
			observed.Listener, err = net.Listen("tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			observed.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
			observed.StartTLS()
			f.server = observed
			t.Cleanup(observed.Close)
			u, _ := url.Parse(f.origin)
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", u.Host, &tls.Config{RootCAs: f.roots.Clone(), MinVersion: tls.VersionTLS12})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			cookie := f.cookie(client)
			if cookie == nil {
				t.Fatal("real authenticated cookie missing")
			}
			path := "/enterprise/api/v1/rooms/" + room + "/attachments/" + reserved.ObjectID + "/content"
			_, err = fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: %s\r\nContent-Type: application/octet-stream\r\nContent-Length: %d\r\nOrigin: %s\r\n%s: %s\r\nCookie: %s=%s\r\n\r\n", path, u.Host, len(data), f.origin, CSRFHeader, session.CSRF, cookie.Name, cookie.Value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = conn.Write(data[:1024]); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered: // Real body read: live SQL authentication/reservation already completed.
			case <-time.After(5 * time.Second):
				t.Fatal("request never reached the real object body reader")
			}
			if mode == "tcp-reset" {
				tcp, ok := conn.NetConn().(*net.TCPConn)
				if !ok {
					t.Fatal("raw TLS transport did not expose a real TCP connection")
				}
				if err = tcp.SetLinger(0); err != nil {
					t.Fatal(err)
				}
				if err = tcp.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err = conn.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("actual upload interruption did not settle the handler")
			}
			attachmentFaultRejectUnverified(t, f, client, session.CSRF, room, reserved.ObjectID, "interrupted", objects, before)
			if entries, err := os.ReadDir(filepath.Join(root, reserved.ObjectID)); err != nil || len(entries) != 0 {
				t.Fatalf("interrupted body retained partial files: %v", entries)
			}
			t.Logf("real %s: declared=%d sent=1024; reserved quota unchanged; verify object_missing; no references/facts/receipts", mode, len(data))
		})
	}
}

func TestAttachmentHTTPNormalUploadControlIsVerifiedAndReferenced(t *testing.T) {
	f := newObjectHTTPFixture(t)
	client := f.browser("member-one")
	f.login(client)
	session := f.session(client)
	const room = "normal-upload-control"
	f.createRoom(client, session.CSRF, room)
	data := bytes.Repeat([]byte("normal evidence\n"), 4096)
	reserved := objectHTTPReserve(t, f, client, room, "normal-reserve", data, "", session.CSRF)
	meta := objectHTTPFinish(t, f, client, room, "normal-verify", reserved, data, "", session.CSRF)
	status, body := f.post(client, "/enterprise/api/v1/rooms/"+room+"/messages", map[string]any{"client_id": "normal-reference", "to": "slot1", "text": "verified control", "attachment_ids": []string{meta.ID}}, session.CSRF)
	if status != http.StatusOK || !bytes.Contains(body, []byte(meta.ID)) {
		t.Fatalf("normal actual HTTP control did not publish verified evidence: %d %s", status, body)
	}
	state := attachmentFaultState(t, f, room, meta.ID)
	if state.bytes != int64(len(data)) || state.pending != 0 || state.references != 1 || state.state != "referenced" {
		t.Fatalf("normal control manifest/quota/reference state: %+v", state)
	}
}
