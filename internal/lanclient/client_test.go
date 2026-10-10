package lanclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

const fixtureSecret = "local-only-relay-secret"

type wireRequest struct {
	Action string
	Body   string
	Key    string
}

type remoteFixture struct {
	t              *testing.T
	mu             sync.Mutex
	server         *httptest.Server
	invite         lanshare.Invite
	requests       []wireRequest
	status         string
	requestID      string
	key            string
	room           lanshare.RoomInfo
	lostJoin       bool
	nextJoinStatus string
	handler        func(http.ResponseWriter, *http.Request, string, []byte)
	admissionHook  func(http.ResponseWriter, *http.Request, lanshare.JoinResponse) bool
}

func localDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenAt(filepath.Join(localDir(t), "pairroom", "lan-clients"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func newRemote(t *testing.T) *remoteFixture {
	t.Helper()
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	config, err := lanshare.ServerTLS(identity)
	if err != nil {
		t.Fatal(err)
	}
	collaboration, err := (model.Collaboration{}).ForCreation()
	if err != nil {
		t.Fatal(err)
	}
	f := &remoteFixture{t: t, status: "accepted", room: lanshare.RoomInfo{RoomID: "shared-room", Name: "Shared fixture", Slot: model.ActorSlot2, Generation: 7, BindID: "remote-binding", Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeGrok}, Collaboration: &collaboration}}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.server.TLS = config
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	pin, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	f.invite = lanshare.Invite{Version: 1, Endpoint: f.server.URL, HostPin: pin, RoomID: f.room.RoomID, InviteID: "invitation-one", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	return f
}

func reply(w http.ResponseWriter, value any) { _ = json.NewEncoder(w).Encode(value) }

func loseResponse(w http.ResponseWriter) {
	h, ok := w.(http.Hijacker)
	if !ok {
		panic("HTTP/1 fixture has no hijacker")
	}
	connection, _, err := h.Hijack()
	if err == nil {
		_ = connection.Close()
	}
}

func (f *remoteFixture) serve(w http.ResponseWriter, r *http.Request) {
	action := filepath.Base(r.URL.Path)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Error(err)
		w.WriteHeader(400)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/lan/v1/rooms/shared-room/"+action {
		f.t.Errorf("unscoped direct route: %s %s", r.Method, r.URL.Path)
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
		f.t.Error("missing per-Room mutual TLS identity")
		w.WriteHeader(403)
		return
	}
	key := lanshare.Fingerprint(r.TLS.PeerCertificates[0])
	for _, header := range []string{"Authorization", "X-PairRoom-Session", "X-PairRoom-Bind", "X-PairRoom-Generation"} {
		if r.Header.Get(header) != "" {
			f.t.Errorf("local credential header reached peer: %s", header)
		}
	}
	f.mu.Lock()
	remoteBind, generation := f.room.BindID, f.room.Generation
	f.mu.Unlock()
	if action != "join" && action != "join-status" && (r.Header.Get("X-PairRoom-LAN-Bind") != remoteBind || r.Header.Get("X-PairRoom-LAN-Generation") != fmt.Sprint(generation)) {
		f.t.Error("request lost original accepted binding and generation")
	}
	for _, secret := range []string{fixtureSecret, "official-private-session", "private-transcript.jsonl"} {
		if strings.Contains(string(data), secret) {
			f.t.Errorf("local identity reached peer payload: %s", secret)
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, wireRequest{Action: action, Body: string(data), Key: key})
	if action == "join" {
		var request lanshare.JoinRequest
		if json.Unmarshal(data, &request) != nil {
			f.mu.Unlock()
			w.WriteHeader(400)
			return
		}
		if f.requestID != "" && f.status != "expired" && f.status != "revoked" && (f.requestID != request.RequestID || f.key != key) {
			f.t.Error("admission retry rotated request or key")
		}
		if f.requestID != request.RequestID && f.nextJoinStatus != "" {
			f.status, f.nextJoinStatus = f.nextJoinStatus, ""
		}
		f.requestID, f.key = request.RequestID, key
		f.room.Runtimes[f.room.Slot] = request.Runtime
	}
	if action == "join" || action == "join-status" {
		var response lanshare.JoinResponse
		response.Status = f.status
		response.Receipt = lanshare.EncodeReceipt(lanshare.Receipt{RoomID: f.invite.RoomID, RequestID: f.requestID, Fingerprint: f.key})
		if f.status == "accepted" {
			room := f.room
			room.Runtimes = make(map[model.ActorID]model.RuntimeKind, len(f.room.Runtimes))
			for slot, kind := range f.room.Runtimes {
				room.Runtimes[slot] = kind
			}
			response.Room = &room
		}
		lost := f.lostJoin && action == "join"
		if action == "join" {
			f.lostJoin = false
		}
		hook := f.admissionHook
		f.mu.Unlock()
		if lost {
			loseResponse(w)
			return
		}
		if hook != nil && hook(w, r, response) {
			return
		}
		reply(w, response)
		return
	}
	handler := f.handler
	f.mu.Unlock()
	if handler != nil {
		handler(w, r, action, data)
		return
	}
	switch action {
	case "summary":
		reply(w, relay.Summary{RoomID: "shared-room", HostMode: model.HostNative})
	case "status":
		reply(w, relay.Snapshot{RoomID: "shared-room", HostMode: model.HostNative})
	case "history":
		reply(w, relay.HistoryPage{})
	case "doctor":
		reply(w, map[string]any{"relay": relay.Summary{RoomID: "shared-room"}, "protocol": "native-test", "service_version": "fixture"})
	case "confirm", "inspect":
		reply(w, relay.Binding{Slot: model.ActorSlot2, BindID: "remote-binding", Generation: 7, Active: true})
	case "park":
		reply(w, map[string]bool{"enabled": true, "unbound": true})
	case "unbind":
		f.mu.Lock()
		f.status = "revoked"
		f.mu.Unlock()
		reply(w, map[string]bool{"unbound": true})
	default:
		f.t.Errorf("unexpected remote operation: %s", action)
		w.WriteHeader(404)
	}
}

func (f *remoteFixture) setHandler(handler func(http.ResponseWriter, *http.Request, string, []byte)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
}

func (f *remoteFixture) actions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []string
	for _, request := range f.requests {
		result = append(result, request.Action)
	}
	return result
}

func (f *remoteFixture) options(t *testing.T) JoinOptions {
	t.Helper()
	return JoinOptions{Invite: lanshare.EncodeInvite(f.invite), Workspace: localDir(t), Runtime: model.RuntimeGrok, SessionID: "official-private-session", BindID: "local-binding", CredentialHash: relay.Digest(fixtureSecret), Label: "colleague"}
}

func (f *remoteFixture) join(t *testing.T, s *Store) (*Client, relay.Auth, JoinOptions) {
	t.Helper()
	options := f.options(t)
	c, joined, err := s.Join(context.Background(), options)
	if err != nil || joined.Status != "accepted" || joined.Binding == nil {
		t.Fatalf("join failed: %+v, %v", joined, err)
	}
	return c, relay.Auth{Slot: model.ActorSlot2, BindID: options.BindID, Generation: 7, SessionID: options.SessionID, Secret: fixtureSecret}, options
}

func TestReadOnlyDiscoveryAndBoundProbeNeverCreateOrRewriteState(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if items, err := s.List(ctx); err != nil || len(items) != 0 {
		t.Fatalf("absent discovery: %v %v", items, err)
	}
	if _, err := os.Stat(s.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Open/List created an absent client store")
	}
	f := newRemote(t)
	c, auth, _ := f.join(t, s)
	file := filepath.Join(c.dir, "client.json")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	other, err := OpenAt(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	reloaded, err := other.Get(ctx, c.id)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := reloaded.Metadata(ctx)
	if err != nil || metadata.ID != c.id || metadata.Invite.HostPin != f.invite.HostPin || metadata.Invite.Endpoint != f.invite.Endpoint || metadata.Workspace == "" || metadata.Binding.SessionID != auth.SessionID {
		t.Fatalf("exact local route lost: %+v %v", metadata, err)
	}
	for _, action := range []string{"summary", "status", "history", "doctor", "inspect"} {
		var result map[string]any
		if err := reloaded.Relay(ctx, auth, action, nil, &result); err != nil {
			t.Fatalf("read-only %s: %v", action, err)
		}
	}
	snapshots, err := other.List(ctx)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("snapshot: %+v %v", snapshots, err)
	}
	projection, _ := json.Marshal(snapshots)
	for _, private := range []string{auth.SessionID, fixtureSecret, "PRIVATE KEY", "request_id", "receipt", "credential_hash"} {
		if strings.Contains(string(projection), private) {
			t.Errorf("private state in public snapshot: %s", private)
		}
	}
	after, _ := os.ReadFile(file)
	afterInfo, _ := os.Stat(file)
	if string(before) != string(after) || !info.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("read-only discovery/preflight rewrote local admission or receipts")
	}
	if got := strings.Join(f.actions(), ","); got != "join,summary,status,history,doctor,inspect" {
		t.Fatalf("read-only operations caused recovery or delivery: %s", got)
	}
}

func TestJoinRecoversLostAcceptanceWithOriginalKeyRequestAndRoute(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	options := f.options(t)
	f.lostJoin = true
	c, _, err := s.Join(ctx, options)
	if !errors.Is(err, ErrTransportUnavailable) || c == nil {
		t.Fatalf("lost response classification: %v", err)
	}
	original, err := c.read(ctx)
	if err != nil || original.Status != "pending" {
		t.Fatalf("lost response invented admission: %+v %v", original, err)
	}
	c, joined, err := s.Join(ctx, options)
	if err != nil || joined.Status != "accepted" || joined.Binding.BindID != options.BindID {
		t.Fatalf("same request did not recover admission: %+v %v", joined, err)
	}
	accepted, err := c.read(ctx)
	if err != nil || accepted.RequestID != original.RequestID || accepted.Identity != original.Identity {
		t.Fatal("recovery rotated private Room identity")
	}
	publicReceipt, err := lanshare.ParseReceipt(joined.Receipt)
	key, _ := original.Identity.Fingerprint()
	if err != nil || publicReceipt.RoomID != f.invite.RoomID || publicReceipt.RequestID != original.RequestID || publicReceipt.Fingerprint != key {
		t.Fatal("public receipt is not tied to the original request and key")
	}
	changed := f.invite
	changed.Endpoint, changed.InviteID = "https://192.168.254.1:1", "new-public-invite"
	options.Invite = lanshare.EncodeInvite(changed)
	if _, again, err := s.Join(ctx, options); err != nil || again.Receipt != joined.Receipt {
		t.Fatalf("repeat accepted join required approval: %+v %v", again, err)
	}
	metadata, _ := c.Metadata(ctx)
	if metadata.Invite.Endpoint != f.invite.Endpoint {
		t.Fatal("a new invitation silently rerouted an existing accepted binding")
	}
	if got := strings.Join(f.actions(), ","); got != "join,join,join-status" {
		t.Fatalf("unexpected admission recovery operations: %s", got)
	}
	options.Workspace = localDir(t)
	if _, _, err := s.Join(ctx, options); err == nil {
		t.Fatal("repeated join retargeted an existing local binding")
	}
	if len(f.actions()) != 3 {
		t.Fatal("conflicting local association contacted remote")
	}
}

func TestCrossHostSessionOwnershipAndOfflineDetach(t *testing.T) {
	ctx := context.Background()
	s, first, second := newStore(t), newRemote(t), newRemote(t)
	c, auth, options := first.join(t, s)
	otherOptions := options
	otherOptions.Invite, otherOptions.BindID = lanshare.EncodeInvite(second.invite), "other-local-binding"
	if _, _, err := s.Join(ctx, otherOptions); !errors.Is(err, nativeidentity.ErrOwned) {
		t.Fatalf("same session admitted by two hosts: %v", err)
	}
	if len(second.actions()) != 0 {
		t.Fatal("conflicting session reached second host")
	}
	first.server.Close()
	if err := c.Detach(ctx, auth); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Resume(ctx); !errors.Is(err, ErrInactive) {
		t.Fatalf("detached membership resumed: %v", err)
	}
	if err := c.Maintenance(ctx); !errors.Is(err, ErrInactive) {
		t.Fatalf("detached observer remained active: %v", err)
	}
	other, joined, err := s.Join(ctx, otherOptions)
	if err != nil || joined.Status != "accepted" || other.id == c.id {
		t.Fatalf("explicit detach did not release exact local association: %+v %v", joined, err)
	}
	if err := c.Detach(ctx, auth); !errors.Is(err, nativeidentity.ErrOwned) {
		t.Fatalf("stale detach did not protect replacement: %v", err)
	}
	r, _ := other.read(ctx)
	if err := s.identities.Check(ctx, reservation(r)); err != nil {
		t.Fatal("stale detach removed another host's reservation")
	}
}

func TestClientStatePrivateAndMalformedIdentityFailsClosed(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, _, _ := f.join(t, s)
	if err := privatefile.CheckDirectory(s.Root()); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.CheckDirectory(c.dir); err != nil {
		t.Fatal(err)
	}
	var r record
	if err := privatefile.ReadJSON(filepath.Join(c.dir, "client.json"), 2<<20, &r); err != nil {
		t.Fatal(err)
	}
	if r.Identity.PrivateKeyPEM == "" || r.CredentialHash != relay.Digest(fixtureSecret) {
		t.Fatal("private client identity is incomplete")
	}
	r.Schema = 99
	if err := privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), r); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(c.dir, "client.json"))
	if _, err := s.Get(ctx, c.id); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("future identity was accepted: %v", err)
	}
	if _, err := s.List(ctx); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("malformed record was silently omitted: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(c.dir, "client.json"))
	if string(before) != string(after) || len(f.actions()) != 1 {
		t.Fatal("failed validation repaired state or contacted remote")
	}
}

func TestRejectedCrossHostSessionDoesNotPoisonTargetRoom(t *testing.T) {
	ctx := context.Background()
	s, first, second := newStore(t), newRemote(t), newRemote(t)
	_, _, options := first.join(t, s)
	options.Invite, options.BindID = lanshare.EncodeInvite(second.invite), "rejected-binding"
	if _, _, err := s.Join(ctx, options); !errors.Is(err, nativeidentity.ErrOwned) {
		t.Fatalf("conflicting session was not rejected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), ID(second.invite), "client.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("ownership rejection installed a target Room identity")
	}
	options.SessionID, options.BindID = "fresh-native-session", "fresh-local-binding"
	_, joined, err := s.Join(ctx, options)
	if err != nil || joined.Binding == nil || joined.Binding.SessionID != "fresh-native-session" {
		t.Fatalf("rejected attempt poisoned a fresh join: %+v %v", joined, err)
	}
	if len(second.actions()) != 1 {
		t.Fatal("rejected session contacted the host")
	}
}

// The private catalog must not fail every LAN surface because of an unrelated
// file (OS or sync metadata) in it. Only an entry that names a LAN client is
// still held to the client-record boundary.
func TestListIgnoresUnrelatedCatalogEntriesAndKeepsClientShapedOnesStrict(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, _, _ := f.join(t, s)
	for name, contents := range map[string]string{"Thumbs.db": "junk", "desktop.ini": "[.ShellClassInfo]"} {
		if err := os.WriteFile(filepath.Join(s.Root(), name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A full catalog's worth of unrelated entries still consumes no Room slots.
	for i := 0; i < maxClients; i++ {
		if err := os.WriteFile(filepath.Join(s.Root(), fmt.Sprintf("metadata-%04d.tmp", i)), []byte("metadata"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snapshots, err := s.List(ctx)
	if err != nil || len(snapshots) != 1 || snapshots[0].ID != c.id {
		t.Fatalf("unrelated catalog entries broke List: %+v %v", snapshots, err)
	}
	second := newRemote(t)
	options := second.options(t)
	options.SessionID, options.BindID = "second-native-session", "second-local-binding"
	if _, result, err := s.Join(ctx, options); err != nil || result.Binding == nil {
		t.Fatalf("unrelated catalog entries consumed admission capacity: %+v %v", result, err)
	}
	// An entry whose name does claim to be a client is a client record or nothing.
	if err := os.WriteFile(filepath.Join(s.Root(), "lan_"+strings.Repeat("a", 32)), []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(ctx); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("client-shaped file was not rejected: %v", err)
	}
	third := newRemote(t)
	options = third.options(t)
	options.SessionID, options.BindID = "third-native-session", "third-local-binding"
	if _, _, err := s.Join(ctx, options); !errors.Is(err, ErrInvalidState) || len(third.actions()) != 0 {
		t.Fatalf("admission ignored a malformed client-shaped entry: %v", err)
	}
}

func TestClientDirectoryCapacityStillBoundsAdmissionAndDiscovery(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	if err := os.MkdirAll(filepath.Dir(s.Root()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := privatefile.Mkdir(s.Root()); err != nil {
		t.Fatal(err)
	}
	// Interrupted admissions retain their directory slots; ignoring unrelated
	// entries must not turn the bound into an unlimited pending-client store.
	for i := 0; i < maxClients; i++ {
		if err := privatefile.Mkdir(filepath.Join(s.Root(), fmt.Sprintf("lan_%032x", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Join(ctx, f.options(t)); err == nil || err.Error() != "LAN joined Room limit exceeded" {
		t.Fatalf("admission exceeded the client-directory bound: %v", err)
	}
	if len(f.actions()) != 0 {
		t.Fatal("capacity refusal contacted the host")
	}
	if err := privatefile.Mkdir(filepath.Join(s.Root(), fmt.Sprintf("lan_%032x", maxClients))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(ctx); err == nil || err.Error() != "LAN joined Room limit exceeded" {
		t.Fatalf("discovery accepted an over-capacity client catalog: %v", err)
	}
}
