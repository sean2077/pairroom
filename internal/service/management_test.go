package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/websession"
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
	for _, marker := range []string{"/api/v1/session", "/api/v1/service", "X-PairRoom-CSRF", "createBrowserSession", "credentialFromInput", "submitCredentialLogin", "logoutBrowserSession", "login-form", "login-token", "logout-button", "浏览器会话已过期", "completeBindings", "补全 Binding", "queue_position", "materializes on first turn", "roomHasBlockingPendingBindings", "renderProjects", "renderRuntimes", "renderSettings", "/suspend", "pairroom daemon open", "pairroom daemon status", "--recover-stale-lock", "/api/v1/projects/", "/refresh", "confirm_project_id", "/api/v1/rooms/batch-archive", "/api/v1/rooms/batch-delete", "acknowledge_data_loss", "selectedRoomIDs", "confirm-input", "confirm-input-label", "confirm-ack", "project_refresh", "project_removal", "room_deletion", "pending_room_cleanup", "批量归档", "批量清理", "永久清除", "room-action-control", "room-select-control", "button secondary-button compact-button room-action-control room-select-control", "#/rooms/", "openRoomInBrowserAction", "/api/v1/runtime-policy", "/surface/", "恢复后才能打开"} {
		if !strings.Contains(asset.Body.String(), marker) {
			t.Fatalf("management asset omitted %q", marker)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage", "window.prompt(", "window.confirm("} {
		if strings.Contains(asset.Body.String(), forbidden) {
			t.Fatalf("management asset must not use %s", forbidden)
		}
	}

	uxAsset := httptest.NewRecorder()
	server.Handler().ServeHTTP(uxAsset, managementRequest(http.MethodGet, "/management-ux.js", "", false))
	if uxAsset.Code != http.StatusOK {
		t.Fatalf("management UX asset status=%d body=%s", uxAsset.Code, uxAsset.Body.String())
	}
	for _, marker := range []string{"management-command-dialog", "management-mobile-nav", "Control+K Meta+K", "flushDeferredRefresh", "installKeyboardNavigation"} {
		if !strings.Contains(uxAsset.Body.String(), marker) {
			t.Fatalf("management UX asset omitted %q", marker)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage"} {
		if strings.Contains(uxAsset.Body.String(), forbidden) {
			t.Fatalf("management UX asset must not use %s", forbidden)
		}
	}

	uxStyles := httptest.NewRecorder()
	server.Handler().ServeHTTP(uxStyles, managementRequest(http.MethodGet, "/management-ux.css", "", false))
	if uxStyles.Code != http.StatusOK {
		t.Fatalf("management UX styles status=%d body=%s", uxStyles.Code, uxStyles.Body.String())
	}
	for _, marker := range []string{".management-command-dialog", ".management-mobile-nav", "@media (max-width: 900px)", "prefers-reduced-motion"} {
		if !strings.Contains(uxStyles.Body.String(), marker) {
			t.Fatalf("management UX styles omitted %q", marker)
		}
	}

	shell := httptest.NewRecorder()
	server.Handler().ServeHTTP(shell, managementRequest(http.MethodGet, "/", "", false))
	if shell.Code != http.StatusOK || !strings.Contains(shell.Body.String(), `/management-ux.css`) ||
		!strings.Contains(shell.Body.String(), `/management-ux.js`) ||
		!strings.Contains(shell.Body.String(), `id="login-screen"`) ||
		!strings.Contains(shell.Body.String(), `id="login-form"`) ||
		!strings.Contains(shell.Body.String(), `id="login-token"`) ||
		!strings.Contains(shell.Body.String(), `id="login-submit"`) ||
		!strings.Contains(shell.Body.String(), `id="logout-button"`) ||
		!strings.Contains(shell.Body.String(), "不会写入 Web Storage") ||
		!strings.Contains(shell.Body.String(), `id="confirm-input"`) ||
		!strings.Contains(shell.Body.String(), `id="confirm-expected"`) ||
		!strings.Contains(shell.Body.String(), `id="confirm-input-label"`) ||
		!strings.Contains(shell.Body.String(), `id="confirm-ack"`) ||
		!strings.Contains(shell.Body.String(), `id="confirm-ack-label"`) ||
		!strings.Contains(shell.Body.String(), `id="room-tree"`) ||
		!strings.Contains(shell.Body.String(), `id="room-tabstrip"`) ||
		!strings.Contains(shell.Body.String(), `id="room-stage"`) {
		t.Fatalf("Management Shell omitted authentication or irreversible-operation controls: status=%d body=%s", shell.Code, shell.Body.String())
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

func TestManagementPollingSkipsUnchangedSnapshotRenders(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})

	asset := httptest.NewRecorder()
	server.Handler().ServeHTTP(asset, managementRequest(http.MethodGet, "/management.js", "", false))
	if asset.Code != http.StatusOK {
		t.Fatalf("management asset status=%d body=%s", asset.Code, asset.Body.String())
	}
	for _, marker := range []string{
		"renderedSnapshotKey: ''",
		"delete renderableSnapshot.generated_at;",
		"if (renderableRuntime.busy) delete renderableRuntime.last_used_at;",
		"const snapshotChanged = nextRenderKey !== state.renderedSnapshotKey;",
		"const showProgress = notify || forceRender;",
		"if (forceRender || snapshotChanged) {",
		"window.dispatchEvent(new Event('pairroom:management-render-pending'));",
		"state.renderedSnapshotKey = snapshotRenderKey(state.snapshot);",
	} {
		if !strings.Contains(asset.Body.String(), marker) {
			t.Fatalf("management polling regression guard omitted %q", marker)
		}
	}
	for _, forbidden := range []string{
		"updateChrome();\n      if (forceRender || canRenderNow()) {",
		"if (state.refreshPromise) return state.refreshPromise;\n    $('refresh-button').classList.add('spinning');",
	} {
		if strings.Contains(asset.Body.String(), forbidden) {
			t.Fatalf("management polling retained unconditional refresh behavior %q", forbidden)
		}
	}

	uxAsset := httptest.NewRecorder()
	server.Handler().ServeHTTP(uxAsset, managementRequest(http.MethodGet, "/management-ux.js", "", false))
	if uxAsset.Code != http.StatusOK {
		t.Fatalf("management UX asset status=%d body=%s", uxAsset.Code, uxAsset.Body.String())
	}
	if marker := "window.addEventListener('pairroom:management-render-pending'"; !strings.Contains(uxAsset.Body.String(), marker) {
		t.Fatalf("management UX deferred-refresh guard omitted %q", marker)
	}
}

func TestManagementProjectCardsKeepUnavailableRemovalReachable(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})

	asset := httptest.NewRecorder()
	server.Handler().ServeHTTP(asset, managementRequest(http.MethodGet, "/management.js", "", false))
	if asset.Code != http.StatusOK {
		t.Fatalf("management asset status=%d body=%s", asset.Code, asset.Body.String())
	}
	for _, marker := range []string{
		"projectRemovalButton(project, rooms.length, true)",
		"本地路径不可用不影响注销此空 Project。",
		"显示并永久清除已归档 Room 后即可注销 Project。",
		"显示已归档 Room",
	} {
		if !strings.Contains(asset.Body.String(), marker) {
			t.Fatalf("management asset omitted %q", marker)
		}
	}
	if forbidden := "rooms.length === 0 ? projectRemovalButton"; strings.Contains(asset.Body.String(), forbidden) {
		t.Fatalf("management asset must not hide Project removal behind %q", forbidden)
	}

	style := httptest.NewRecorder()
	server.Handler().ServeHTTP(style, managementRequest(http.MethodGet, "/management.css", "", false))
	if style.Code != http.StatusOK {
		t.Fatalf("management stylesheet status=%d body=%s", style.Code, style.Body.String())
	}
	for _, marker := range []string{
		".project-card-header { display: grid; grid-template-columns: auto minmax(0, 1fr);",
		".project-card-actions { grid-column: 1 / -1; min-width: 0; display: flex; flex-wrap: wrap; justify-content: flex-end;",
		".room-actions > .room-action-control {",
		".room-select-control input {",
	} {
		if !strings.Contains(style.Body.String(), marker) {
			t.Fatalf("management stylesheet omitted %q", marker)
		}
	}
}

func TestManagementBrowserSessionSurvivesBootstrapAndRequiresCSRF(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})

	rejected := httptest.NewRecorder()
	rejectedRequest := managementRequest(http.MethodPost, "/api/v1/session", "", false)
	rejectedRequest.Header.Set("Authorization", "Bearer invalid-management-secret")
	server.Handler().ServeHTTP(rejected, rejectedRequest)
	if rejected.Code != http.StatusUnauthorized || len(rejected.Result().Cookies()) != 0 {
		t.Fatalf("invalid credential created a browser session: status=%d cookies=%#v body=%s", rejected.Code, rejected.Result().Cookies(), rejected.Body.String())
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
	cookies := bootstrap.Result().Cookies()
	if len(cookies) != 1 || session.CSRF == "" {
		t.Fatalf("bootstrap cookie/session missing: cookies=%#v session=%#v", cookies, session)
	}
	cookie := cookies[0]
	if !strings.HasPrefix(cookie.Name, "pairroom_management_") || cookie.Path != "/api/v1/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("unexpected Management session cookie: %#v", cookie)
	}

	resumed := httptest.NewRecorder()
	resumedRequest := managementRequest(http.MethodGet, "/api/v1/service", "", false)
	resumedRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(resumed, resumedRequest)
	if resumed.Code != http.StatusOK || len(resumed.Result().Cookies()) != 1 {
		t.Fatalf("resumed session status=%d cookies=%#v body=%s", resumed.Code, resumed.Result().Cookies(), resumed.Body.String())
	}

	projectPath := testGitRepo(t)
	body, err := json.Marshal(map[string]string{"path": projectPath})
	if err != nil {
		t.Fatal(err)
	}
	missingCSRF := httptest.NewRecorder()
	missingRequest := managementRequest(http.MethodPost, "/api/v1/projects", string(body), false)
	missingRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(missingCSRF, missingRequest)
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", missingCSRF.Code, missingCSRF.Body.String())
	}

	accepted := httptest.NewRecorder()
	acceptedRequest := managementRequest(http.MethodPost, "/api/v1/projects", string(body), false)
	acceptedRequest.AddCookie(cookie)
	acceptedRequest.Header.Set(websession.CSRFHeaderName, session.CSRF)
	server.Handler().ServeHTTP(accepted, acceptedRequest)
	if accepted.Code != http.StatusCreated {
		t.Fatalf("valid CSRF status=%d body=%s", accepted.Code, accepted.Body.String())
	}

	logout := httptest.NewRecorder()
	logoutRequest := managementRequest(http.MethodDelete, "/api/v1/session", "", false)
	logoutRequest.AddCookie(cookie)
	logoutRequest.Header.Set(websession.CSRFHeaderName, session.CSRF)
	server.Handler().ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status=%d body=%s", logout.Code, logout.Body.String())
	}
	after := httptest.NewRecorder()
	afterRequest := managementRequest(http.MethodGet, "/api/v1/service", "", false)
	afterRequest.AddCookie(cookie)
	server.Handler().ServeHTTP(after, afterRequest)
	if after.Code != http.StatusUnauthorized {
		t.Fatalf("revoked session status=%d body=%s", after.Code, after.Body.String())
	}
}

func TestManagementSessionCookieNameIsStableAndDataRootScoped(t *testing.T) {
	first := managementSessionCookieName(`C:\pairroom\one`)
	if first == "" || first != managementSessionCookieName(`C:\pairroom\one`) {
		t.Fatalf("Management cookie name is not stable: %q", first)
	}
	if second := managementSessionCookieName(`C:\pairroom\two`); second == first {
		t.Fatalf("distinct data roots share Management cookie %q", first)
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

func TestArchiveInterruptsActiveTurnThenSuspendsBeforeLifecycleAppend(t *testing.T) {
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
		if recorder.Code != http.StatusOK {
			t.Fatalf("archive did not interrupt and complete: status=%d body=%s", recorder.Code, recorder.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("archive did not interrupt the active Turn promptly")
	}
	if runtime.interruptCount.Load() < 1 {
		t.Fatalf("archive never interrupted the active Turn: interrupts=%d", runtime.interruptCount.Load())
	}
	projected, ok := registry.Room(rooms[0].ID)
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
	factory := &fakeRuntimeFactory{busy: true, stayBusyOnInterrupt: true}
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

	// The busy Runtime keeps archive inside InterruptAndSuspend, proving
	// Shutdown has an active mutating handler to drain.
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

func TestManagementSnapshotIncludesSummaryPolicyAndCapabilities(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Dashboard Room",
		Bindings:  specs(BindingNew, BindingNew, "dashboard"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	server, manager := newManagementTestServer(t, registry, SyntheticProvisioner{})
	activateRuntime(t, manager, room.ID)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, managementRequest(http.MethodGet, "/api/v1/service", "", true))
	if recorder.Code != http.StatusOK {
		t.Fatalf("snapshot status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var snapshot ServiceSnapshot
	if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.RuntimePolicy.Limit != 2 || snapshot.RuntimePolicy.IdleTimeoutSeconds != int64(time.Hour/time.Second) {
		t.Fatalf("unexpected runtime policy: %#v", snapshot.RuntimePolicy)
	}
	if snapshot.Summary.Projects != 1 || snapshot.Summary.Rooms != 1 || snapshot.Summary.ActiveRooms != 1 ||
		snapshot.Summary.ActiveRuntimes != 1 || snapshot.Summary.RuntimeCapacityUsed != 1 {
		t.Fatalf("unexpected service summary: %#v", snapshot.Summary)
	}
	if !snapshot.Capabilities.LegacyImport || !snapshot.Capabilities.RuntimeSuspend || !snapshot.Capabilities.ProjectRefresh ||
		!snapshot.Capabilities.ProjectRemoval || !snapshot.Capabilities.RoomDeletion || snapshot.Capabilities.ServerPathBrowser ||
		!snapshot.Capabilities.RuntimePolicyMutation || !snapshot.Capabilities.RoomSurface {
		t.Fatalf("unexpected capability surface: %#v", snapshot.Capabilities)
	}
}

func TestManagementSuspendEndpointProtectsBusyTurnsAndCancelsQueuedRooms(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 2)
	factory := &fakeRuntimeFactory{busy: true}
	manager, err := NewRuntimeManager(registry, factory.open, RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 5 * time.Millisecond, CloseTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer shutdownRuntimeManager(t, manager, factory)
	activateRuntime(t, manager, rooms[0].ID)
	if _, err := manager.RequestActivation(rooms[1].ID); err != nil {
		t.Fatal(err)
	}
	waitRuntimeStatus(t, manager, rooms[1].ID, func(status RuntimeStatus) bool { return status.Phase == RuntimeQueued })

	server, err := NewManagementServer(ManagementServerConfig{
		Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, Token: "management-secret",
	})
	if err != nil {
		t.Fatal(err)
	}

	busy := httptest.NewRecorder()
	server.Handler().ServeHTTP(busy, managementRequest(http.MethodPost, "/api/v1/rooms/"+rooms[0].ID+"/suspend", "", true))
	if busy.Code != http.StatusConflict || !strings.Contains(strings.ToLower(busy.Body.String()), "active work") {
		t.Fatalf("busy suspend status=%d body=%s", busy.Code, busy.Body.String())
	}
	if runtime := factory.get(rooms[0].ID); runtime == nil || runtime.closeCount.Load() != 0 {
		t.Fatalf("busy runtime was interrupted: %#v", runtime)
	}

	queued := httptest.NewRecorder()
	server.Handler().ServeHTTP(queued, managementRequest(http.MethodPost, "/api/v1/rooms/"+rooms[1].ID+"/suspend", "", true))
	if queued.Code != http.StatusOK {
		t.Fatalf("queued suspend status=%d body=%s", queued.Code, queued.Body.String())
	}
	var status RuntimeStatus
	if err := json.Unmarshal(queued.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.Phase != RuntimeSuspended || status.QueuePosition != 0 || status.OccupiesCapacity {
		t.Fatalf("queued room was not safely canceled: %#v", status)
	}
}

func TestManagementRuntimeControlErrorsUseConflict(t *testing.T) {
	server := &ManagementServer{}
	for _, testCase := range []struct {
		name string
		err  error
	}{
		{name: "busy", err: ErrRuntimeBusy},
		{name: "close uncertain", err: fmt.Errorf("%w: cleanup not proven", ErrRuntimeCloseUncertain)},
		{name: "drain aborted", err: fmt.Errorf("%w: runtime changed", ErrRuntimeDrainAborted)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server.writeError(recorder, testCase.err)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
