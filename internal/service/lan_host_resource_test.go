package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestLANSourceLimitKeepsEstablishedSourcesUsableAtCapacity(t *testing.T) {
	h := &lanHostServer{config: lanHostConfig{Enabled: true}}
	request := func(address string) *http.Request { return &http.Request{RemoteAddr: address} }
	if !h.admitRequest(request("10.0.0.1:1234")) {
		t.Fatal("initial source was rejected")
	}
	for i := 0; i < 1023; i++ {
		if !h.admitRequest(request(fmt.Sprintf("192.168.%d.%d:1234", i/256, i%256))) {
			t.Fatal("bounded source table filled prematurely")
		}
	}
	if h.admitRequest(request("10.0.0.2:1234")) || len(h.rates) != 1024 {
		t.Fatal("untracked source bypassed the fixed table capacity")
	}
	if !h.admitRequest(request("10.0.0.1:5678")) || !h.admitRequest(request("[::ffff:10.0.0.1]:4321")) {
		t.Fatal("other source addresses exhausted an established colleague's rate budget")
	}
	if h.rates["10.0.0.1"].Count != 3 || len(h.rates) != 1024 {
		t.Fatal("mapped IPv4 or source port created another rate bucket")
	}
	for i := 3; i < 240; i++ {
		if !h.admitRequest(request("10.0.0.1:1234")) {
			t.Fatal("source's own fixed request budget changed")
		}
	}
	if h.admitRequest(request("10.0.0.1:1234")) {
		t.Fatal("established source bypassed its individual rate limit")
	}
	h.rates["192.168.0.0"] = lanRateWindow{At: time.Now().Add(-time.Minute), Count: 1}
	if !h.admitRequest(request("10.0.0.2:1234")) || len(h.rates) != 1024 {
		t.Fatal("expired source capacity was not reusable")
	}
	if h.admitRequest(request("client.invalid:1234")) || h.admitRequest(request("bad-address")) {
		t.Fatal("unobserved non-numeric source accepted")
	}
}

func TestLANColdUnauthorizedAdmissionAllocationsDoNotScaleWithHistory(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	f.accept(t, client, pending)
	ctx := context.Background()
	if err := f.management.runtimes.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	key := relay.Digest("never admitted")
	measure := func() float64 {
		t.Helper()
		var authErr error
		allocs := testing.AllocsPerRun(10, func() {
			_, authErr = f.management.lanHost.authorizedRuntime(req, f.room.ID, "status", key)
		})
		if !errors.Is(authErr, relay.ErrAuth) {
			t.Fatalf("unapproved cold request was not rejected: %v", authErr)
		}
		return allocs
	}
	short := measure()
	n, err := f.management.sharedNativeRuntime(ctx, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 512; i++ {
		if err := n.engine.Failure(f.owner, "rate_limit"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.management.runtimes.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	long := measure()
	// Replaying 512 more JSON facts allocated thousands of additional objects
	// per rejected certificate. A compact admission lookup has fixed work;
	// a small margin permits ordinary runtime bookkeeping on supported OSes.
	if long > short+80 {
		t.Fatalf("unapproved certificate scanned retained history: short=%.0f, long=%.0f allocations", short, long)
	}
	if phase := f.management.runtimes.Status(f.room.ID).Phase; phase != RuntimeSuspended {
		t.Fatalf("unapproved request activated Room: %s", phase)
	}
}

type lanUnreadBody struct{ reads atomic.Int32 }

func (r *lanUnreadBody) Read([]byte) (int, error) {
	r.reads.Add(1)
	return 0, io.EOF
}
func (*lanUnreadBody) Close() error { return nil }

func TestLANTransferCapacityRejectsBeforeBodyReadAndReleases(t *testing.T) {
	h := &lanHostServer{}
	var release []func()
	for i := 0; i < lanTransferLimit; i++ {
		unlock, ok := h.acquireTransfer(fmt.Sprintf("room-%d", i/lanRoomTransferLimit))
		if !ok {
			t.Fatal("transfer capacity filled early")
		}
		release = append(release, unlock)
	}
	for _, room := range []string{"room-0", "new-room"} {
		body := &lanUnreadBody{}
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Body = body
		w := httptest.NewRecorder()
		h.upload(w, r, &nativeHostRuntime{room: Room{ID: room}}, relay.Auth{})
		if w.Code != http.StatusTooManyRequests || body.reads.Load() != 0 {
			t.Fatal("full transfer capacity read an untrusted body")
		}
	}
	for _, unlock := range release {
		unlock()
	}
	if h.transfers != 0 || len(h.roomTransfers) != 0 {
		t.Fatal("finished transfer retained capacity")
	}
	one, ok := h.acquireTransfer("room")
	if !ok {
		t.Fatal("released global capacity was not reusable")
	}
	defer one()
	two, ok := h.acquireTransfer("room")
	if !ok {
		t.Fatal("per-Room transfer limit filled early")
	}
	defer two()
	if _, ok := h.acquireTransfer("room"); ok {
		t.Fatal("one Room consumed more than its transfer limit")
	}
	other, ok := h.acquireTransfer("other-room")
	if !ok {
		t.Fatal("a busy Room consumed another Room's capacity")
	}
	other()
}

type lanSlowUploadBody struct {
	prefix  *bytes.Reader
	blocked chan struct{}
	closed  chan struct{}
	start   sync.Once
	stop    sync.Once
}

func (r *lanSlowUploadBody) Read(p []byte) (int, error) {
	if r.prefix.Len() != 0 {
		return r.prefix.Read(p)
	}
	r.start.Do(func() { close(r.blocked) })
	<-r.closed
	return 0, io.ErrClosedPipe
}
func (r *lanSlowUploadBody) Close() error {
	r.stop.Do(func() { close(r.closed) })
	return nil
}

func TestLANUploadStreamsPrivatelyAndRevocationRemovesUncommittedStage(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, identity := f.join(t)
	f.accept(t, client, pending)
	key, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.native.engine.LANAuth(key)
	if err != nil {
		t.Fatal(err)
	}
	var prefix bytes.Buffer
	writer := multipart.NewWriter(&prefix)
	part, err := writer.CreateFormFile("file", "repro.log")
	if err != nil {
		t.Fatal(err)
	}
	const sent = 64 << 10
	if _, err := io.WriteString(part, strings.Repeat("e", sent)); err != nil {
		t.Fatal(err)
	}
	body := &lanSlowUploadBody{prefix: bytes.NewReader(prefix.Bytes()), blocked: make(chan struct{}), closed: make(chan struct{})}
	t.Cleanup(func() { _ = body.Close() })
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Body = body
	r.Header.Set("Content-Type", writer.FormDataContentType())
	r.Header.Set("X-PairRoom-Attachment-Kind", "file")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.management.lanHost.upload(w, r, f.native, a)
	}()
	select {
	case <-body.blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("upload did not reach the paused network read")
	}
	entries, err := os.ReadDir(f.native.media.Root())
	if err != nil || len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".staged-") {
		t.Fatalf("partial upload was buffered instead of privately staged: %v %v", entries, err)
	}
	stage, err := os.Open(filepath.Join(f.native.media.Root(), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	partial := make([]byte, sent/2)
	_, err = io.ReadFull(stage, partial)
	_ = stage.Close()
	if err != nil || string(partial) != strings.Repeat("e", len(partial)) {
		t.Fatalf("partial network bytes did not reach the private stage: %v", err)
	}
	progress := make(chan error, 1)
	go func() {
		_, err := f.native.engine.Send(f.owner, relay.SendRequest{ID: "during-upload", Text: "owner remains usable"})
		progress <- err
	}()
	select {
	case err := <-progress:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("network upload held the Room engine lock")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- f.native.engine.RevokeLANMember(f.owner) }()
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not interrupt the stalled request body")
	}
	<-done
	entries, err = os.ReadDir(f.native.media.Root())
	if err != nil || len(entries) != 0 {
		t.Fatalf("revoked upload left evidence or a temporary file: %v %v", entries, err)
	}
	if _, err := f.native.engine.Inspect(a); !errors.Is(err, relay.ErrAuth) {
		t.Fatal("upload cancellation did not preserve the revocation boundary")
	}
}

type lanObservedRequestBody struct {
	io.ReadCloser
	read  int
	ready chan struct{}
	once  sync.Once
}

func (b *lanObservedRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += n
	if b.read >= 32<<10 {
		b.once.Do(func() { close(b.ready) })
	}
	return n, err
}

func TestLANUploadRevocationInterruptsRealHTTPRead(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, identity := f.join(t)
	f.accept(t, client, pending)
	key, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.native.engine.LANAuth(key)
	if err != nil {
		t.Fatal(err)
	}
	reading, handled := make(chan struct{}), make(chan struct{})
	// Authentication is already covered by the pinned TLS fixture above.
	// This real HTTP connection exercises ResponseController read deadlines
	// against net/http's request-body locks rather than a fake body Close.
	network := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handled)
		r.Body = &lanObservedRequestBody{ReadCloser: r.Body, ready: reading}
		f.management.lanHost.upload(w, r, f.native, a)
	}))
	t.Cleanup(network.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	var prefix bytes.Buffer
	parts := multipart.NewWriter(&prefix)
	part, err := parts.CreateFormFile("file", "blocked.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, strings.Repeat("e", 64<<10)); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, network.URL, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", parts.FormDataContentType())
	req.Header.Set("X-PairRoom-Attachment-Kind", "file")
	response := make(chan int, 1)
	go func() {
		res, err := network.Client().Do(req)
		if err != nil {
			response <- 0
			return
		}
		_ = res.Body.Close()
		response <- res.StatusCode
	}()
	go func() { _, _ = writer.Write(prefix.Bytes()) }()
	select {
	case <-reading:
	case <-time.After(2 * time.Second):
		t.Fatal("network handler did not begin streaming the attachment")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- f.native.engine.RevokeLANMember(f.owner) }()
	select {
	case err := <-revoked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("revocation could not interrupt the live HTTP request read")
	}
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP handler retained an interrupted transfer")
	}
	_ = writer.Close()
	if status := <-response; status == http.StatusOK {
		t.Fatal("interrupted network upload reported a committed attachment")
	}
	entries, err := os.ReadDir(f.native.media.Root())
	if err != nil || len(entries) != 0 {
		t.Fatalf("interrupted HTTP request retained its stage: %v %v", entries, err)
	}
}

func TestLANAttachmentErrorsKeepQuotaGuidanceWithoutHostPaths(t *testing.T) {
	for _, kind := range []string{"file", "image"} {
		err := lanAttachmentError(&os.PathError{Op: "open", Path: "/private/host/data/attachments/object", Err: os.ErrPermission}, kind)
		if err == nil || strings.Contains(err.Error(), "/private/host/") || !strings.Contains(err.Error(), "5 MiB") {
			t.Fatalf("filesystem error escaped the LAN boundary: %v", err)
		}
		if kind == "file" && !strings.Contains(err.Error(), "UTF-8") {
			t.Fatal("evidence validation guidance was lost")
		}
	}
	for _, source := range []error{attachment.ErrSharedQuota, attachment.ErrTemporaryQuota, relay.ErrAuth, relay.ErrClosed, context.Canceled} {
		if got := lanAttachmentError(fmt.Errorf("private path: %w", source), "file"); got != source {
			t.Fatalf("safe structured error lost its actionable identity: %v", got)
		}
	}
}
