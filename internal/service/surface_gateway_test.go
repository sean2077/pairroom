package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/websession"
)

type loopbackProxyRuntime struct {
	base  string
	token string
}

func (r *loopbackProxyRuntime) URL() string                 { return r.base + "#token=" + r.token }
func (r *loopbackProxyRuntime) Busy() bool                  { return false }
func (r *loopbackProxyRuntime) Close(context.Context) error { return nil }
func (r *loopbackProxyRuntime) ProxyBaseURL() string        { return r.base }
func (r *loopbackProxyRuntime) ProxyToken() string          { return r.token }

type loopbackProxyFactory struct {
	mu       sync.Mutex
	runtimes map[string]*loopbackProxyRuntime
	base     string
	token    string
}

func (f *loopbackProxyFactory) open(_ context.Context, room Room) (RoomRuntime, error) {
	runtime := &loopbackProxyRuntime{base: f.base, token: f.token}
	f.mu.Lock()
	if f.runtimes == nil {
		f.runtimes = map[string]*loopbackProxyRuntime{}
	}
	f.runtimes[room.ID] = runtime
	f.mu.Unlock()
	return runtime, nil
}

func TestRoomSurfaceGatewayUsesManagementSessionAndHidesRuntimeToken(t *testing.T) {
	var leakedCookie bool
	var sawBearer bool
	roomServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			leakedCookie = true
		}
		if r.Header.Get("Authorization") == "Bearer runtime-secret" {
			sawBearer = true
		}
		w.Header().Set("Set-Cookie", "pairroom_session_leak=1; Path=/")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		switch r.URL.Path {
		case "/", "/index.html":
			io.WriteString(w, `<html><body>room-index</body></html>`)
		case "/api/v1/snapshot":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"ok":true}`)
		case "/api/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"accepted":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer roomServer.Close()

	registry, project := testRegistry(t, testGitRepo(t))
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Surface Room", Bindings: specs(BindingNew, BindingNew, "surface"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	factory := &loopbackProxyFactory{base: roomServer.URL + "/", token: "runtime-secret"}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 2, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, Token: "management-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, room.ID)

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, managementRequest(http.MethodGet, "/api/v1/rooms/"+room.ID+"/surface/", "", false))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated surface status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	bootstrap := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrap, managementRequest(http.MethodPost, "/api/v1/session", "", true))
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookie := bootstrap.Result().Cookies()[0]

	sessionGet := httptest.NewRecorder()
	sessionRequest := managementRequest(http.MethodGet, "/api/v1/rooms/"+room.ID+"/surface/api/v1/session", "", false)
	sessionRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(sessionGet, sessionRequest)
	if sessionGet.Code != http.StatusOK {
		t.Fatalf("surface session status=%d body=%s", sessionGet.Code, sessionGet.Body.String())
	}
	if !strings.Contains(sessionGet.Body.String(), session.CSRF) {
		t.Fatalf("surface session did not return Management CSRF: %s", sessionGet.Body.String())
	}
	if strings.Contains(sessionGet.Body.String(), "runtime-secret") {
		t.Fatal("runtime token leaked through surface session")
	}
	if sessionGet.Header().Get("X-Frame-Options") != "SAMEORIGIN" || !strings.Contains(sessionGet.Header().Get("Content-Security-Policy"), "frame-ancestors 'self'") {
		t.Fatalf("surface session framing headers=%v", sessionGet.Header())
	}

	index := httptest.NewRecorder()
	indexRequest := managementRequest(http.MethodGet, "/api/v1/rooms/"+room.ID+"/surface/", "", false)
	indexRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(index, indexRequest)
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "room-index") {
		t.Fatalf("surface index status=%d body=%s", index.Code, index.Body.String())
	}
	for _, cookie := range index.Result().Cookies() {
		if strings.Contains(cookie.Name, "session_leak") || strings.HasPrefix(cookie.Name, "pairroom_session_") {
			t.Fatalf("runtime Set-Cookie leaked: %#v", cookie)
		}
	}
	if index.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("surface index X-Frame-Options=%q", index.Header().Get("X-Frame-Options"))
	}
	if strings.Contains(index.Body.String(), "runtime-secret") {
		t.Fatal("runtime token leaked through surface HTML")
	}

	snapshot := httptest.NewRecorder()
	snapshotRequest := managementRequest(http.MethodGet, "/api/v1/rooms/"+room.ID+"/surface/api/v1/snapshot", "", false)
	snapshotRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(snapshot, snapshotRequest)
	if snapshot.Code != http.StatusOK || snapshot.Body.String() != `{"ok":true}` {
		t.Fatalf("surface snapshot status=%d body=%s", snapshot.Code, snapshot.Body.String())
	}
	if !sawBearer {
		t.Fatal("gateway did not inject runtime bearer")
	}
	if leakedCookie {
		t.Fatal("management cookie was forwarded to the runtime")
	}

	missingCSRF := httptest.NewRecorder()
	writeRequest := managementRequest(http.MethodPost, "/api/v1/rooms/"+room.ID+"/surface/api/v1/messages", `{"text":"hi"}`, false)
	writeRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(missingCSRF, writeRequest)
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", missingCSRF.Code, missingCSRF.Body.String())
	}

	accepted := httptest.NewRecorder()
	okRequest := managementRequest(http.MethodPost, "/api/v1/rooms/"+room.ID+"/surface/api/v1/messages", `{"text":"hi"}`, false)
	okRequest.AddCookie(cookie)
	okRequest.Header.Set(websession.CSRFHeaderName, session.CSRF)
	server.Handler().ServeHTTP(accepted, okRequest)
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), `"accepted":true`) {
		t.Fatalf("proxied write status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	denied := httptest.NewRecorder()
	deniedRequest := managementRequest(http.MethodGet, "/api/v1/rooms/"+room.ID+"/surface/../../etc/passwd", "", false)
	deniedRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(denied, deniedRequest)
	if denied.Code == http.StatusOK {
		t.Fatalf("path traversal was proxied: status=%d body=%s", denied.Code, denied.Body.String())
	}

	unknown := httptest.NewRecorder()
	unknownRequest := managementRequest(http.MethodGet, "/api/v1/rooms/"+room.ID+"/surface/secret.txt", "", false)
	unknownRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(unknown, unknownRequest)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown surface path status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	var opened []string
	original := openRoomInBrowser
	openRoomInBrowser = func(raw string) error {
		opened = append(opened, raw)
		return nil
	}
	t.Cleanup(func() { openRoomInBrowser = original })
	openedOK := httptest.NewRecorder()
	server.Handler().ServeHTTP(openedOK, managementRequest(http.MethodPost, "/api/v1/rooms/"+room.ID+"/open-browser", "", true))
	if openedOK.Code != http.StatusOK || !strings.Contains(openedOK.Body.String(), `"opened":true`) {
		t.Fatalf("open-browser status=%d body=%s", openedOK.Code, openedOK.Body.String())
	}
	if len(opened) != 1 || !strings.HasPrefix(opened[0], roomServer.URL) {
		t.Fatalf("opened URLs=%v want prefix %s", opened, roomServer.URL)
	}
	if strings.Contains(openedOK.Body.String(), "token=") || strings.Contains(openedOK.Body.String(), "runtime-secret") {
		t.Fatal("open-browser response leaked a runtime token")
	}
}

func TestRoomSurfaceUnavailableWithoutRuntime(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Queued Room", Bindings: specs(BindingNew, BindingNew, "queued-surface"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, managementRequest(http.MethodGet, "/api/v1/rooms/"+room.ID+"/surface/", "", true))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "runtime_not_ready") {
		t.Fatalf("missing runtime status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Fatalf("unavailable surface must remain frameable: %v", recorder.Header())
	}
}

func TestRuntimePolicyMutationAndArchivedExternalOpen(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Policy Room", Bindings: specs(BindingNew, BindingNew, "policy"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	server, manager := newManagementTestServer(t, registry, SyntheticProvisioner{})

	updated := httptest.NewRecorder()
	server.Handler().ServeHTTP(updated, managementRequest(http.MethodPatch, "/api/v1/runtime-policy", `{"limit":8}`, true))
	if updated.Code != http.StatusOK || !strings.Contains(updated.Body.String(), `"limit":8`) {
		t.Fatalf("policy update status=%d body=%s", updated.Code, updated.Body.String())
	}
	if manager.Policy().Limit != 8 {
		t.Fatalf("manager limit=%d", manager.Policy().Limit)
	}

	rejected := httptest.NewRecorder()
	server.Handler().ServeHTTP(rejected, managementRequest(http.MethodPatch, "/api/v1/runtime-policy", `{"limit":0}`, true))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status=%d body=%s", rejected.Code, rejected.Body.String())
	}

	blocked := httptest.NewRecorder()
	server.Handler().ServeHTTP(blocked, managementRequest(http.MethodPost, "/api/v1/rooms/"+room.ID+"/open-browser", "", true))
	if blocked.Code != http.StatusConflict || !strings.Contains(blocked.Body.String(), "runtime_not_ready") {
		t.Fatalf("unready open-browser status=%d body=%s", blocked.Code, blocked.Body.String())
	}

	if _, err := registry.ArchiveRoom(context.Background(), room.ID); err != nil {
		t.Fatal(err)
	}
	archived := httptest.NewRecorder()
	server.Handler().ServeHTTP(archived, managementRequest(http.MethodPost, "/api/v1/rooms/"+room.ID+"/open-browser", "", true))
	if archived.Code != http.StatusConflict || !strings.Contains(archived.Body.String(), "room_archived") {
		t.Fatalf("archived open-browser status=%d body=%s", archived.Code, archived.Body.String())
	}
}
