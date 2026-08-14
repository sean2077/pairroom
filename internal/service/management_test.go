package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func newManagementTestServer(t *testing.T, registry *Registry, provisioner BindingProvisioner) (*ManagementServer, *RuntimeManager) {
	t.Helper()
	factory := &fakeRuntimeFactory{}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 2, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager, Provisioner: provisioner, Token: "management-secret",
	})
	if err != nil {
		_ = manager.Shutdown(context.Background())
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Errorf("shutdown runtime manager: %v", err)
		}
	})
	return server, manager
}

func managementRequest(method, target string, body string, authorized bool) *http.Request {
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, "http://127.0.0.1"+target, nil)
	} else {
		request = httptest.NewRequest(method, "http://127.0.0.1"+target, bytes.NewBufferString(body))
		request.Header.Set("Content-Type", "application/json")
	}
	if authorized {
		request.Header.Set("Authorization", "Bearer management-secret")
	}
	return request
}

func TestManagementShellAuthenticationAssetsAndSecurityHeaders(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})

	asset := httptest.NewRecorder()
	server.Handler().ServeHTTP(asset, managementRequest(http.MethodGet, "/management.js", "", false))
	if asset.Code != http.StatusOK {
		t.Fatalf("management asset status=%d body=%s", asset.Code, asset.Body.String())
	}
	for _, marker := range []string{"/api/v1/service", "completeBindings", "补全 Binding", "queue_position", "materializes on first turn", "roomHasBlockingPendingBindings"} {
		if !strings.Contains(asset.Body.String(), marker) {
			t.Fatalf("management asset omitted %q", marker)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage"} {
		if strings.Contains(asset.Body.String(), forbidden) {
			t.Fatalf("management asset must not use %s", forbidden)
		}
	}

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, managementRequest(http.MethodGet, "/api/v1/service", "", false))
	if unauthorized.Code != http.StatusUnauthorized || unauthorized.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("missing bearer status=%d headers=%v body=%s", unauthorized.Code, unauthorized.Header(), unauthorized.Body.String())
	}

	queryToken := httptest.NewRecorder()
	request := managementRequest(http.MethodGet, "/api/v1/service?token=management-secret", "", true)
	server.Handler().ServeHTTP(queryToken, request)
	if queryToken.Code != http.StatusUnauthorized {
		t.Fatalf("query token was accepted: status=%d body=%s", queryToken.Code, queryToken.Body.String())
	}

	authorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(authorized, managementRequest(http.MethodGet, "/api/v1/service", "", true))
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized snapshot status=%d body=%s", authorized.Code, authorized.Body.String())
	}
	if authorized.Header().Get("Cache-Control") != "no-store" || authorized.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("security headers missing: %v", authorized.Header())
	}
	var snapshot ServiceSnapshot
	if err := json.Unmarshal(authorized.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Healthy || len(snapshot.Projects) != 1 || snapshot.DataRoot != registry.Root() {
		t.Fatalf("unexpected service snapshot: %#v", snapshot)
	}

	crossSite := httptest.NewRecorder()
	crossSiteRequest := managementRequest(http.MethodPost, "/api/v1/import", `{"path":"/tmp/example"}`, true)
	crossSiteRequest.Header.Set("Origin", "https://evil.example")
	server.Handler().ServeHTTP(crossSite, crossSiteRequest)
	if crossSite.Code != http.StatusForbidden {
		t.Fatalf("cross-site mutation status=%d body=%s", crossSite.Code, crossSite.Body.String())
	}
}

func TestManagementBindingCompletionEndpointIsAtomicAndDurable(t *testing.T) {
	repo := testGitRepo(t)
	custom := filepath.Join(t.TempDir(), "legacy-api")
	if err := writeLegacyRoom(custom, repo, "legacy-api-room", "Legacy API", "", ""); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := registry.ImportLegacy(context.Background(), custom)
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.HasPendingBindings() {
		t.Fatal("legacy fixture did not expose pending bindings")
	}
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})

	body := `{"bindings":{"claude":{"mode":"new"},"codex":{"mode":"existing","session_id":"codex-api-existing"}}}`
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, managementRequest(http.MethodPost, "/api/v1/rooms/"+legacy.ID+"/bindings", body, true))
	if recorder.Code != http.StatusOK {
		t.Fatalf("binding completion status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var completed Room
	if err := json.Unmarshal(recorder.Body.Bytes(), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.HasPendingBindings() || completed.Bindings[model.ActorClaude].SessionID == "" || completed.Bindings[model.ActorCodex].SessionID != "codex-api-existing" {
		t.Fatalf("unexpected completed bindings: %#v", completed.Bindings)
	}
	for _, binding := range completed.Bindings {
		owner, ok := registry.BindingOwner(binding.Key())
		if !ok || owner != completed.ID {
			t.Fatalf("binding owner=%q ok=%v for %#v", owner, ok, binding)
		}
	}
	events, err := readEventsReadOnly(filepath.Join(custom, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Kind != EventRoomBindingsCompleted {
		t.Fatalf("last event=%q, want %q", events[len(events)-1].Kind, EventRoomBindingsCompleted)
	}

	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, managementRequest(http.MethodPost, "/api/v1/rooms/"+legacy.ID+"/bindings", body, true))
	if second.Code < 400 {
		t.Fatalf("second completion unexpectedly succeeded: status=%d body=%s", second.Code, second.Body.String())
	}
	after, err := readEventsReadOnly(filepath.Join(custom, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(events) {
		t.Fatalf("rejected second completion appended an event: before=%d after=%d", len(events), len(after))
	}
}

func TestArchiveWaitsForActiveTurnThenSuspendsBeforeLifecycleAppend(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	factory := &fakeRuntimeFactory{busy: true}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)
	activateRuntime(t, manager, rooms[0].ID)
	runtime := factory.get(rooms[0].ID)
	if runtime == nil {
		t.Fatal("active Room did not create a runtime")
	}

	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, Token: "management-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := readEventsReadOnly(filepath.Join(rooms[0].DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(recorder, managementRequest(http.MethodPost, "/api/v1/rooms/"+rooms[0].ID+"/archive", "", true))
		close(completed)
	}()

	select {
	case <-completed:
		t.Fatalf("archive returned while the Room still had active work: status=%d body=%s", recorder.Code, recorder.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	projected, ok := registry.Room(rooms[0].ID)
	if !ok || projected.Archived() {
		t.Fatalf("Room lifecycle changed before active work completed: %#v", projected)
	}
	during, err := readEventsReadOnly(filepath.Join(rooms[0].DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(during) != len(before) {
		t.Fatalf("archive appended before runtime became idle: before=%d during=%d", len(before), len(during))
	}
	if runtime.closeCount.Load() != 0 {
		t.Fatal("busy runtime was closed or interrupted")
	}

	runtime.busy.Store(false)
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("archive did not complete after the active turn became idle")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("archive status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	projected, ok = registry.Room(rooms[0].ID)
	if !ok || !projected.Archived() {
		t.Fatalf("Room was not archived: %#v", projected)
	}
	if runtime.closeCount.Load() != 1 {
		t.Fatalf("runtime close count=%d, want 1", runtime.closeCount.Load())
	}
	after, err := readEventsReadOnly(filepath.Join(rooms[0].DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before)+1 || after[len(after)-1].Kind != EventRoomArchived {
		t.Fatalf("unexpected archive event sequence: before=%d after=%d last=%q", len(before), len(after), after[len(after)-1].Kind)
	}
}

func TestManagementShutdownForceClosesActiveHandlerAfterDeadline(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	factory := &fakeRuntimeFactory{busy: true}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	activateRuntime(t, manager, rooms[0].ID)
	runtime := factory.get(rooms[0].ID)
	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, Token: "management-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()

	before, err := readEventsReadOnly(filepath.Join(rooms[0].DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/api/v1/rooms/"+rooms[0].ID+"/archive", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer management-secret")
	requestDone := make(chan error, 1)
	go func() {
		response, requestErr := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestDone <- requestErr
	}()

	// The busy Runtime keeps archive inside WaitAndSuspend, proving Shutdown has
	// an active mutating handler to drain.
	time.Sleep(40 * time.Millisecond)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	shutdownErr := server.Shutdown(shutdownCtx)
	cancel()
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error=%v, want context deadline exceeded", shutdownErr)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("forced server close did not stop Serve")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("forced server close did not cancel active archive request")
	}

	projected, ok := registry.Room(rooms[0].ID)
	if !ok || projected.Archived() {
		t.Fatalf("canceled archive changed lifecycle: %#v", projected)
	}
	after, err := readEventsReadOnly(filepath.Join(rooms[0].DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("canceled archive appended an event: before=%d after=%d", len(before), len(after))
	}

	runtime.busy.Store(false)
	managerCtx, managerCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer managerCancel()
	if err := manager.Shutdown(managerCtx); err != nil {
		t.Fatalf("shutdown runtime manager: %v", err)
	}
}
