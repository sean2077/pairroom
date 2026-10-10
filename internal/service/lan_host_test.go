package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

type lanHostFixture struct {
	management *ManagementServer
	local      *httptest.Server
	remote     *httptest.Server
	room       Room
	native     *nativeHostRuntime
	owner      relay.Auth
	invite     lanshare.Invite
}

func newLANHostFixture(t *testing.T) *lanHostFixture {
	t.Helper()
	registry, project := testRegistry(t, testGitRepo(t))
	noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		t.Error("LAN Native provisioning spawned a vendor adapter")
		return Binding{}, nil, errors.New("unexpected adapter")
	})
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "LAN test", HostMode: model.HostNative, Sharing: "lan", OwnerSlot: model.ActorSlot1, Agents: map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: model.RuntimeClaude, Provider: model.NativeProviderRef()}}}, noSpawn)
	if err != nil {
		t.Fatal(err)
	}
	factory := EmbeddedRuntimeFactory(registry, EmbeddedRuntimeConfig{Claude: agent.Config{Command: "missing-do-not-spawn"}, nativeWake: nativeWakerConfig{Wait: func(context.Context, time.Duration) error { return context.Canceled }}})
	manager, err := NewRuntimeManager(registry, factory, RuntimeManagerConfig{Limit: 5, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewManagementServer(ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: noSpawn, Token: "owner-management"})
	if err != nil {
		t.Fatal(err)
	}
	if status := s.lanHost.status(); status.Enabled || status.Endpoint != "" {
		t.Fatal("LAN listener was enabled before owner opt-in")
	}
	local := httptest.NewServer(s.Handler())
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	remote := httptest.NewUnstartedServer(http.HandlerFunc(s.lanHost.serve))
	remote.TLS, err = lanshare.ServerTLS(identity)
	if err != nil {
		t.Fatal(err)
	}
	remote.StartTLS()
	s.lanHost.mu.Lock()
	s.lanHost.config = lanHostConfig{Schema: 1, Enabled: true, Address: remote.Listener.Addr().String(), Identity: identity}
	s.lanHost.endpoint = remote.URL
	s.lanHost.mu.Unlock()
	n, err := s.sharedNativeRuntime(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	session := "private-native-session-" + relay.Digest(registry.Root())[:16]
	b, err := n.engine.Bind(model.ActorSlot1, relay.BindRequest{BindID: "host-binding", CredentialHash: relay.Digest("host-secret"), SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	owner := relay.Auth{Slot: b.Slot, BindID: b.BindID, Generation: b.Generation, SessionID: session, Secret: "host-secret"}
	if _, err = n.engine.ConfirmSession(owner, owner.SessionID, "/private/native/transcript"); err != nil {
		t.Fatal(err)
	}
	f := &lanHostFixture{management: s, local: local, remote: remote, room: room, native: n, owner: owner}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.Shutdown(stopCtx); err != nil {
			t.Errorf("stop LAN fixture Service: %v", err)
		}
		stopCancel()
		remote.Close()
		local.Close()
		// Fault tests deliberately leave claimed input without an ACK. Normal
		// manager shutdown preserves that busy runtime, so teardown must close
		// its current test-owned instance explicitly. This persists unknown
		// receipts and closes the Event Log without inventing collector stdout.
		// Resolve the current instance because a test may have reopened it.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if current, err := manager.runtimeForCompletion(room.ID); err == nil {
			if err := current.Close(closeCtx); err != nil {
				t.Errorf("close LAN fixture runtime: %v", err)
			}
		} else if !errors.Is(err, ErrRuntimeNotReady) {
			// Integrity fault tests may poison the Registry after releasing the
			// writer. Only that already-suspended case has no runtime to close.
			status := manager.Status(room.ID)
			if !errors.Is(err, ErrRegistryFailClosed) || status.Phase != RuntimeSuspended || status.OccupiesCapacity {
				t.Errorf("resolve LAN fixture runtime during cleanup: %v", err)
			}
		}
		closeCancel()
		drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer drainCancel()
		if err := manager.Shutdown(drainCtx); err != nil {
			t.Errorf("stop LAN fixture RuntimeManager: %v", err)
		}
	})
	var issued struct {
		Invite string `json:"invite"`
	}
	status := f.localCall(t, "/api/v1/rooms/"+room.ID+"/lan/invite", map[string]any{}, &issued, s.Token())
	if status != 200 {
		t.Fatalf("issue invite: HTTP %d", status)
	}
	f.invite, err = lanshare.ParseInvite(issued.Invite)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f *lanHostFixture) localCall(t *testing.T, path string, payload, result any, token string) int {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, f.local.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if result != nil && response.StatusCode < 300 {
		if err = json.Unmarshal(data, result); err != nil {
			t.Fatalf("decode response %s: %v", data, err)
		}
	}
	return response.StatusCode
}

type lanScopedTransport struct {
	base http.RoundTripper
	room *lanshare.RoomInfo
}

func (t lanScopedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	lanshare.SetMemberHeaders(r, t.room.BindID, t.room.Generation)
	return t.base.RoundTrip(r)
}
func (f *lanHostFixture) join(t *testing.T) (*http.Client, lanshare.JoinResponse, lanshare.Identity) {
	t.Helper()
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	client, err := lanshare.NewClient(f.invite, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.CloseIdleConnections)
	var joined lanshare.JoinResponse
	if err = lanshare.Call(context.Background(), client, f.invite, "join", lanshare.JoinRequest{InviteID: f.invite.InviteID, RequestID: "remote-request", Runtime: model.RuntimeCodex, Label: "colleague"}, &joined); err != nil {
		t.Fatal(err)
	}
	return client, joined, identity
}
func (f *lanHostFixture) accept(t *testing.T, client *http.Client, pending lanshare.JoinResponse) lanshare.JoinResponse {
	t.Helper()
	var admitted lanshare.JoinResponse
	if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/lan/accept", map[string]string{"receipt": pending.Receipt}, &admitted, f.management.Token()); status != 200 {
		t.Fatalf("accept: HTTP %d", status)
	}
	client.Transport = lanScopedTransport{base: client.Transport, room: admitted.Room}
	return admitted
}
func TestLANHostRequiresExactAdmissionAndScopedMembership(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	if pending.Status != "pending" || pending.Room != nil {
		t.Fatal("pending invitation exposed Room information")
	}
	if err := lanshare.Call(context.Background(), client, f.invite, "status", nil, new(any)); err == nil {
		t.Fatal("pending certificate read Room history")
	}
	if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/lan/accept", map[string]string{"receipt": pending.Receipt}, nil, f.management.cliToken); status != 403 {
		t.Fatal("relay setup token approved a peer")
	}
	receipt, err := lanshare.ParseReceipt(pending.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Fingerprint = relay.Digest("wrong-key")
	if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/lan/accept", map[string]string{"receipt": lanshare.EncodeReceipt(receipt)}, nil, f.management.Token()); status < 400 {
		t.Fatal("wrong receipt admitted")
	}
	admitted := f.accept(t, client, pending)
	if admitted.Room.Runtimes[model.ActorSlot2] != model.RuntimeCodex {
		t.Fatal("actual guest runtime was not selected")
	}
	var snapshot relay.Snapshot
	if err = lanshare.Call(context.Background(), client, f.invite, "status", nil, &snapshot); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(snapshot)
	for _, forbidden := range []string{"private-native-session", "/private/native/transcript", f.room.DataDir, f.management.registry.Root(), "credential_hash"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("shared DTO leaked %q", forbidden)
		}
	}
	if err = lanshare.Call(context.Background(), client, f.invite, "confirm", map[string]string{"session_id": "leak", "transcript_path": "/private/guest"}, nil); err == nil {
		t.Fatal("remote endpoint accepted vendor identity fields")
	}
	if err = lanshare.Call(context.Background(), client, f.invite, "accept", map[string]string{"receipt": pending.Receipt}, nil); err == nil {
		t.Fatal("member inherited owner administration")
	}
	var recovered lanshare.JoinResponse
	if err = lanshare.Call(context.Background(), client, f.invite, "join", lanshare.JoinRequest{InviteID: f.invite.InviteID, RequestID: "remote-request", Runtime: model.RuntimeCodex}, &recovered); err != nil || recovered.Status != "accepted" {
		t.Fatal("lost admission response could not recover same key")
	}
	badRoom := *admitted.Room
	badRoom.Generation++
	client.Transport = lanScopedTransport{base: client.Transport.(lanScopedTransport).base, room: &badRoom}
	if err = lanshare.Call(context.Background(), client, f.invite, "send", relay.SendRequest{ID: "stale", Text: "no"}, nil); err == nil {
		t.Fatal("stale generation gained current member authority")
	}
	client.Transport = lanScopedTransport{base: client.Transport.(lanScopedTransport).base, room: admitted.Room}
	if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/lan/revoke", map[string]any{}, nil, f.management.Token()); status != 200 {
		t.Fatal("revoke failed")
	}
	if err = lanshare.Call(context.Background(), client, f.invite, "status", nil, new(any)); err == nil {
		t.Fatal("revoked live TLS connection retained read authority")
	}
}
func TestLANSharedFilesRequireExplicitHistoryAndKeepBytes(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	f.accept(t, client, pending)
	content := "#!/bin/sh\nprintf 'reproduction only'\n"
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "repro.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, f.invite.Endpoint+"/lan/v1/rooms/"+f.room.ID+"/upload", &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-PairRoom-Attachment-Kind", "file")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var object model.Attachment
	err = json.NewDecoder(response.Body).Decode(&object)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || object.Kind != "file" {
		t.Fatalf("evidence upload: %+v %d %v", object, response.StatusCode, err)
	}
	if err = lanshare.Call(context.Background(), client, f.invite, "download", map[string]string{"id": object.ID}, nil); err == nil {
		t.Fatal("unpublished object was downloadable")
	}
	var message relay.Message
	if err = lanshare.Call(context.Background(), client, f.invite, "send", relay.SendRequest{ID: "evidence", Text: "repro", AttachmentIDs: []string{object.ID}}, &message); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]string{"id": object.ID})
	request, err = http.NewRequest(http.MethodPost, f.invite.Endpoint+"/lan/v1/rooms/"+f.room.ID+"/download", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	response, err = client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	downloaded, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 || string(downloaded) != content {
		t.Fatalf("file transport changed bytes: %d %v", response.StatusCode, err)
	}
	if err = lanshare.Call(context.Background(), client, f.invite, "download", map[string]string{"id": "../../private"}, nil); err == nil {
		t.Fatal("arbitrary path downloaded")
	}
}

type lanBlockedWriter struct {
	header           http.Header
	started, release chan struct{}
	writes           atomic.Int32
	flushes          atomic.Int32
	blockFlush       bool
	deadline         time.Time
}

func (w *lanBlockedWriter) Header() http.Header                { return w.header }
func (w *lanBlockedWriter) WriteHeader(int)                    {}
func (w *lanBlockedWriter) SetWriteDeadline(t time.Time) error { w.deadline = t; return nil }
func (w *lanBlockedWriter) Flush() {
	if w.flushes.Add(1) == 1 && w.blockFlush {
		close(w.started)
		<-w.release
	}
}
func (w *lanBlockedWriter) Write(data []byte) (int, error) {
	if w.writes.Add(1) == 1 && !w.blockFlush {
		close(w.started)
		<-w.release
	}
	return len(data), nil
}
func TestLANDownloadRevocationSerializesBoundedWrites(t *testing.T) {
	for _, blockFlush := range []bool{false, true} {
		name := "write"
		if blockFlush {
			name = "flush"
		}
		t.Run(name, func(t *testing.T) { testLANDownloadRevocation(t, blockFlush) })
	}
}
func testLANDownloadRevocation(t *testing.T, blockFlush bool) {
	t.Helper()
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, identity := f.join(t)
	f.accept(t, client, pending)
	key, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := f.native.engine.LANAuth(key)
	if err != nil {
		t.Fatal(err)
	}
	object, err := f.native.media.SaveEvidence("large.log", strings.NewReader(strings.Repeat("e", 96<<10)), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.native.engine.Send(f.owner, relay.SendRequest{ID: "large", Text: "evidence", AttachmentIDs: []string{object.ID}}); err != nil {
		t.Fatal(err)
	}
	w := &lanBlockedWriter{header: make(http.Header), started: make(chan struct{}), release: make(chan struct{}), blockFlush: blockFlush}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.management.lanHost.download(w, httptest.NewRequest(http.MethodPost, "/", nil), f.native, auth, object.ID)
	}()
	<-w.started
	if w.deadline.IsZero() || time.Until(w.deadline) > 2*time.Second {
		t.Fatal("download write lacks bounded deadline")
	}
	observed := make(chan error, 1)
	go func() {
		_, err := f.native.engine.AuthSummary(f.owner)
		observed <- err
	}()
	select {
	case err := <-observed:
		if err != nil {
			close(w.release)
			<-done
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(w.release)
		<-done
		t.Fatal("slow network write retained the Room lock and blocked its owner")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- f.native.engine.RevokeLANMember() }()
	select {
	case err := <-revoked:
		close(w.release)
		<-done
		t.Fatalf("revocation crossed an admitted write: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(w.release)
	if err = <-revoked; err != nil {
		t.Fatal(err)
	}
	writes := w.writes.Load()
	<-done
	if w.writes.Load() != writes {
		t.Fatal("download released bytes after revocation committed")
	}
}

func TestLANListenerCloseBeforeHTTPServeStarts(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	for _, operation := range []string{"close", "disable"} {
		t.Run(operation, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			address := listener.Addr().String()
			dir := filepath.Join(t.TempDir(), "lan")
			if err := privatefile.Mkdir(dir); err != nil {
				t.Fatal(err)
			}
			// This is the interval after Listen succeeds but before the Serve
			// goroutine registers its socket with http.Server. Server.Close
			// alone cannot release this listener.
			h := &lanHostServer{path: filepath.Join(dir, "host.json"), config: lanHostConfig{Schema: 1, Enabled: true, Address: address}, http: &http.Server{}, listener: listener, endpoint: lanshare.EndpointForAddress(address)}
			t.Cleanup(h.close)
			if operation == "disable" {
				if err := h.configure(false, address); err != nil {
					t.Fatal(err)
				}
			} else {
				h.close()
			}
			if h.status().Endpoint != "" {
				t.Fatal("closed listener remains advertised")
			}
			replacement, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatalf("%s returned before its port was released: %v", operation, err)
			}
			_ = replacement.Close()
		})
	}
}

func TestLANListenerOptInPersistedIdentityAndSafeRestart(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	h := f.management.lanHost
	address := f.remote.Listener.Addr().String()
	pin := h.status().HostPin
	// The fixture occupies this exact interface/port. A failed real listener
	// bind preserves its key and reports unavailable, never a false endpoint.
	if err := h.configure(true, address); err == nil {
		t.Fatal("listener collision was reported as enabled and available")
	}
	if status := h.status(); status.Endpoint != "" || status.Diagnostic == "" || status.HostPin != pin {
		t.Fatalf("failed bind lost identity or claimed availability: %+v", status)
	}
	f.remote.Close()
	if err := h.configure(true, address); err != nil {
		t.Fatal(err)
	}
	if status := h.status(); !status.Enabled || status.Endpoint != "https://"+address || status.HostPin != pin || status.Diagnostic != "" {
		t.Fatalf("explicit opt-in: %+v", status)
	}
	if err := h.configure(true, address); err != nil {
		t.Fatal("idempotent enabling failed")
	}
	for cycle := 0; cycle < 8; cycle++ {
		// No request or wait lets Serve start between configuration and
		// shutdown: either shutdown path must release the port immediately.
		if err := h.configure(false, address); err != nil {
			t.Fatal(err)
		}
		if err := h.configure(true, address); err != nil {
			t.Fatalf("immediate reconfigure cycle %d: %v", cycle, err)
		}
		h.close()
		if err := initLANHost(f.management); err != nil {
			t.Fatal(err)
		}
		h = f.management.lanHost
		if status := h.status(); status.HostPin != pin || status.Endpoint != "https://"+address || status.Diagnostic != "" {
			t.Fatalf("restart cycle %d lost the endpoint or trusted host identity: %+v", cycle, status)
		}
	}
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	client, err := lanshare.NewClient(f.invite, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	var pending lanshare.JoinResponse
	if err = lanshare.Call(context.Background(), client, f.invite, "join", lanshare.JoinRequest{InviteID: f.invite.InviteID, RequestID: "after-listener-restart", Runtime: model.RuntimeCodex}, &pending); err != nil || pending.Status != "pending" {
		t.Fatalf("real persisted pinned listener: %+v %v", pending, err)
	}
	if err = h.configure(false, address); err != nil {
		t.Fatal(err)
	}
	if status := h.status(); status.Enabled || status.Endpoint != "" || status.HostPin != pin {
		t.Fatal("disable lost identity or left listener advertised")
	}
	if err = initLANHost(f.management); err != nil {
		t.Fatal(err)
	}
	if status := f.management.lanHost.status(); status.Enabled || status.Endpoint != "" || status.HostPin != pin {
		t.Fatal("disabled host restarted its listener")
	}
}

func TestLANConfigurationRejectsUnsafeInterfacesAndMalformedIdentity(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	h := f.management.lanHost
	before := h.status()
	for _, address := range []string{"0.0.0.0:8877", "localhost:8877", "8.8.8.8:8877", "127.0.0.1:0", "[::]:8877"} {
		if err := h.configure(true, address); err == nil {
			t.Fatalf("unsafe listener accepted %s", address)
		}
	}
	addrs, err := net.InterfaceAddrs()
	unused := "10.254.253.1"
	if err == nil {
		for _, addr := range addrs {
			if strings.HasPrefix(addr.String(), unused+"/") {
				unused = "10.254.253.2"
			}
		}
	}
	if err = h.configure(true, unused+":8877"); err == nil {
		t.Fatal("non-local interface persisted")
	}
	if after := h.status(); after != before {
		t.Fatal("rejected listener configuration changed active identity or policy")
	}
	if err = privatefile.WriteJSON(h.path, lanHostConfig{Schema: 2}); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	if err = initLANHost(f.management); err == nil {
		t.Fatal("future host identity schema was silently reset")
	}
	unchanged, err := os.ReadFile(h.path)
	if err != nil || !bytes.Equal(saved, unchanged) {
		t.Fatal("invalid host identity was overwritten")
	}
	if err = privatefile.WriteJSON(h.path, lanHostConfig{Schema: 1, Enabled: true, Address: before.Address}); err != nil {
		t.Fatal(err)
	}
	if err = initLANHost(f.management); err == nil {
		t.Fatal("enabled host regenerated a missing identity")
	}
}

func TestLANOwnerSettingsRouteAndRoomStateKeepCapabilitiesPrivate(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	_, pending, _ := f.join(t)
	request := func(method, path, token string, body any) *httptest.ResponseRecorder {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, f.local.URL+path, bytes.NewReader(data))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.management.Handler().ServeHTTP(w, r)
		return w
	}
	w := request(http.MethodGet, "/api/v1/lan", f.management.Token(), nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private_key") || strings.Contains(w.Body.String(), "certificate_pem") {
		t.Fatalf("settings projection: %d %s", w.Code, w.Body.String())
	}
	w = request(http.MethodPut, "/api/v1/lan", f.management.cliToken, map[string]any{"enabled": false, "address": "127.0.0.1:8877"})
	if w.Code != 403 {
		t.Fatal("setup token changed LAN opt-in")
	}
	w = request(http.MethodPut, "/api/v1/lan", f.management.Token(), map[string]any{"enabled": true, "address": "0.0.0.0:8877"})
	if w.Code != 400 {
		t.Fatal("Settings accepted wildcard LAN listener")
	}
	w = request(http.MethodGet, "/api/v1/rooms/"+f.room.ID+"/lan", f.management.Token(), nil)
	var state relay.LANState
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &state) != nil || len(state.Pending) != 1 || state.Pending[0].RequestID != "remote-request" {
		t.Fatalf("pending receipt observation: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), pending.Receipt) {
		t.Fatal("host pending row supplied a receipt as proof from the colleague")
	}
	w = request(http.MethodPut, "/api/v1/lan", f.management.Token(), map[string]any{"enabled": false, "address": "127.0.0.1:8877"})
	var status lanHostStatus
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &status) != nil || status.Enabled || status.Endpoint != "" {
		t.Fatalf("owner disable response: %d %s", w.Code, w.Body.String())
	}
}

func TestLANMemberOperationsPreservePublicationAndHumanReceipts(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	admitted := f.accept(t, client, pending)
	ctx := context.Background()
	for _, action := range []string{"inspect", "confirm", "room", "summary", "doctor", "peer"} {
		if err := lanshare.Call(ctx, client, f.invite, action, map[string]any{}, new(any)); err != nil {
			t.Fatalf("member %s: %v", action, err)
		}
	}
	if err := lanshare.Call(ctx, client, f.invite, "park", map[string]bool{"enabled": false}, nil); err != nil {
		t.Fatal(err)
	}
	var empty lanshare.HeadResponse
	if err := lanshare.Call(ctx, client, f.invite, "head", lanshare.HeadRequest{Park: true}, &empty); err != nil || empty.Head != nil {
		t.Fatal("disabled park collected work")
	}
	if err := lanshare.Call(ctx, client, f.invite, "park", map[string]bool{"enabled": true}, nil); err != nil {
		t.Fatal(err)
	}
	if err := lanshare.Call(ctx, client, f.invite, "head", lanshare.HeadRequest{TimeoutSeconds: 31}, nil); err == nil {
		t.Fatal("unbounded member wait admitted")
	}
	m, err := f.native.engine.Send(f.owner, relay.SendRequest{ID: "head", Text: "reproduce the bug"})
	if err != nil {
		t.Fatal(err)
	}
	var head lanshare.HeadResponse
	if err = lanshare.Call(ctx, client, f.invite, "head", lanshare.HeadRequest{}, &head); err != nil || head.Head == nil || head.Head.Message.ID != m.ID {
		t.Fatalf("prepared head: %+v %v", head, err)
	}
	var claim lanshare.ClaimResponse
	if err = lanshare.Call(ctx, client, f.invite, "claim", lanshare.ClaimRequest{ID: m.ID, Digest: head.Head.Digest, Generation: admitted.Room.Generation}, &claim); err != nil || claim.Claim == nil {
		t.Fatalf("conditional claim: %+v %v", claim, err)
	}
	if err = lanshare.Call(ctx, client, f.invite, "ack", map[string]string{"id": m.ID, "receipt": claim.Claim.Receipt}, nil); err != nil {
		t.Fatal(err)
	}
	var publication relay.Publication
	if err = lanshare.Call(ctx, client, f.invite, "report", map[string]any{"report_seq": 1, "text": "@claude full reproduction result"}, &publication); err != nil || publication.Message == nil {
		t.Fatalf("publication: %+v %v", publication, err)
	}
	var receipt struct {
		Accepted    bool              `json:"accepted"`
		Publication relay.Publication `json:"publication"`
	}
	if err = lanshare.Call(ctx, client, f.invite, "publication", map[string]int{"report_seq": 1}, &receipt); err != nil || !receipt.Accepted || receipt.Publication.Message.ID != publication.Message.ID {
		t.Fatal("lost publication response could not reconcile")
	}
	var human relay.Message
	if err = lanshare.Call(ctx, client, f.invite, "user-send", relay.SendRequest{ID: "human", To: model.ActorSlot1, Text: "please investigate within your local permissions"}, &human); err != nil || !strings.HasPrefix(human.Author, "lan:") {
		t.Fatal("remote human provenance missing")
	}
	var humanReceipt struct {
		Accepted bool           `json:"accepted"`
		Message  *relay.Message `json:"message"`
	}
	if err = lanshare.Call(ctx, client, f.invite, "user-receipt", map[string]string{"id": "human"}, &humanReceipt); err != nil || !humanReceipt.Accepted || humanReceipt.Message.ID != human.ID {
		t.Fatal("lost remote human response lost original identity")
	}
	if err = lanshare.Call(ctx, client, f.invite, "failure", map[string]string{"error": "rate_limit"}, nil); err != nil {
		t.Fatal(err)
	}
	var history relay.HistoryPage
	if err = lanshare.Call(ctx, client, f.invite, "history", map[string]string{"id": human.ID}, &history); err != nil || len(history.Messages) != 1 {
		t.Fatal("same-room history was not available")
	}
	if err = lanshare.Call(ctx, client, f.invite, "leave", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if err = lanshare.Call(ctx, client, f.invite, "send", relay.SendRequest{ID: "after-leave", Text: "forbidden"}, nil); err == nil {
		t.Fatal("left membership retained effects")
	}
}

func TestLANUnauthorizedRequestsCannotActivateSuspendedRooms(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	admitted := f.accept(t, client, pending)
	ctx := context.Background()
	if err := f.management.runtimes.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	stranger, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	unapproved, err := lanshare.NewClient(f.invite, stranger)
	if err != nil {
		t.Fatal(err)
	}
	defer unapproved.CloseIdleConnections()
	unapproved.Transport = lanScopedTransport{base: unapproved.Transport, room: admitted.Room}
	for _, action := range []string{"status", "wake-candidate", "upload"} {
		if err = lanshare.Call(ctx, unapproved, f.invite, action, map[string]any{}, nil); err == nil {
			t.Fatalf("unapproved %s admitted", action)
		}
	}
	if err = lanshare.Call(ctx, client, f.invite, "doctor", nil, nil); err == nil {
		t.Fatal("doctor activated suspended runtime")
	}
	if phase := f.management.runtimes.Status(f.room.ID).Phase; phase != RuntimeSuspended {
		t.Fatalf("unauthorized observation consumed Runtime capacity: %s", phase)
	}
	var summary relay.Summary
	if err = lanshare.Call(ctx, client, f.invite, "summary", nil, &summary); err != nil || !summary.Bindings[model.ActorSlot2].Associated {
		t.Fatalf("admitted member could not resume same binding: %+v %v", summary, err)
	}
	if phase := f.management.runtimes.Status(f.room.ID).Phase; phase != RuntimeActive {
		t.Fatal("authorized remote operation did not activate")
	}
	if _, _, err = f.management.archiveRoomByID(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.management.registry.RestoreRoom(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	if err = lanshare.Call(ctx, client, f.invite, "status", nil, nil); err == nil {
		t.Fatal("restoring archived Room resurrected old membership")
	}
	var old lanshare.JoinResponse
	if err = lanshare.Call(ctx, client, f.invite, "join-status", lanshare.JoinStatusRequest{RequestID: "remote-request"}, &old); err != nil || old.Status != "revoked" {
		t.Fatalf("archive membership revocation was not durable: %+v %v", old, err)
	}
}

// The listener's capacity is shared, not first-come: one certified key that
// parks long-polls or transfers must not consume every slot and reject the
// members of other hosted Rooms with a capacity error.
func TestLANCapacityKeepsEveryCertifiedKeyWithinItsShare(t *testing.T) {
	h := &lanHostServer{inflight: make(map[string]int)}
	for i := 0; i < maxLANConcurrentPerKey; i++ {
		if !h.enter("member-a") {
			t.Fatalf("share refused at request %d", i)
		}
	}
	if h.enter("member-a") {
		t.Fatal("one certified key occupied more than its share")
	}
	if !h.enter("member-b") {
		t.Fatal("a saturated key rejected another member's request")
	}
	for i := 0; i < maxLANConcurrentPerKey; i++ {
		h.leave("member-a")
	}
	if h.inflight["member-a"] != 0 {
		t.Fatalf("released key stayed in the map: %d", h.inflight["member-a"])
	}
	for i := 0; h.inflightTotal < maxLANConcurrent; i++ {
		if !h.enter(fmt.Sprintf("member-%d", i)) {
			t.Fatalf("global capacity refused request %d", i)
		}
	}
	if h.enter("member-late") {
		t.Fatal("over-subscribed listener admitted another request")
	}
	if h.inflightTotal != maxLANConcurrent {
		t.Fatalf("in-flight total = %d, want %d", h.inflightTotal, maxLANConcurrent)
	}
}

// A LAN Room whose Runtime cannot be opened is still archivable: only an
// unopenable Runtime may skip the member revoke, never a fail-closed Registry
// or a lifecycle conflict.
func TestLANRevokeUnavailableToleratesOnlyUnopenableRuntimes(t *testing.T) {
	if !lanRevokeUnavailable(RuntimeFailed, errors.New("project unavailable")) {
		t.Fatal("failed runtime blocked the archive")
	}
	if !lanRevokeUnavailable(RuntimeSuspended, ErrRuntimeNotReady) {
		t.Fatal("suspended runtime blocked the archive")
	}
	for _, err := range []error{ErrRegistryFailClosed, ErrRuntimeLifecycleInProgress, ErrRoomNotFound} {
		if lanRevokeUnavailable(RuntimeActive, err) {
			t.Fatalf("archive ignored %v", err)
		}
	}
}
