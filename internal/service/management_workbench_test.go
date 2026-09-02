package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestManagementRoomWorkbenchAssets(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})

	ux := httptest.NewRecorder()
	server.Handler().ServeHTTP(ux, managementRequest(http.MethodGet, "/management-ux.js", "", false))
	if ux.Code != http.StatusOK {
		t.Fatalf("management UX asset status=%d body=%s", ux.Code, ux.Body.String())
	}
	for _, marker := range []string{
		"roomPickerButton.classList.add('room-workspace-picker')",
		"action: focusGlobalSearch",
		"paletteReturnFocus = activeElement",
		"installRoomWorkspaceShortcuts()",
		"event.stopImmediatePropagation()",
		"const roomWorkspace = raw.startsWith('rooms/') && !app.hidden",
		"skipLink.hidden = app.hidden",
		"app.classList.toggle('room-workspace', roomWorkspace)",
		"tab.draggable = false",
		"target.draggable = true",
		"close?.addEventListener('pointerdown'",
		"rememberAdjacentRoomTabFocus(tab)",
		"pendingRoomTabFocusID = roomID",
		"focusTarget.focus({ preventScroll: true })",
		"event.key === 'Delete'",
		"event.button !== 1",
		"active.scrollIntoView",
		"roomTablist.scrollLeft += event.deltaY",
		"close.tabIndex = selected ? 0 : -1",
	} {
		if !strings.Contains(ux.Body.String(), marker) {
			t.Fatalf("management UX asset omitted Room workbench contract %q", marker)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage"} {
		if strings.Contains(ux.Body.String(), forbidden) {
			t.Fatalf("management UX asset must not use %s", forbidden)
		}
	}

	shell := httptest.NewRecorder()
	server.Handler().ServeHTTP(shell, managementRequest(http.MethodGet, "/", "", false))
	if shell.Code != http.StatusOK || !strings.Contains(shell.Body.String(), `href="/management-workbench.css"`) {
		t.Fatalf("Management Shell must load Room workbench styles before first paint")
	}

	styles := httptest.NewRecorder()
	server.Handler().ServeHTTP(styles, managementRequest(http.MethodGet, "/management-workbench.css", "", false))
	if styles.Code != http.StatusOK {
		t.Fatalf("management workbench styles status=%d body=%s", styles.Code, styles.Body.String())
	}
	for _, marker := range []string{
		"justify-content: flex-start",
		"padding-left: 0",
		".room-tab-target",
		".room-tab-close",
		"min-height: 28px",
		".app-shell.room-workspace .topbar",
		"height: 100dvh",
		".app-shell.room-workspace .management-mobile-nav",
		"@media (hover: none)",
		"prefers-reduced-motion",
	} {
		if !strings.Contains(styles.Body.String(), marker) {
			t.Fatalf("management workbench styles omitted %q", marker)
		}
	}
	if contentType := styles.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/css") {
		t.Fatalf("management workbench Content-Type=%q, want text/css", contentType)
	}
}
