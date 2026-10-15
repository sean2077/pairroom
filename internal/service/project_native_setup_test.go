package service

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/projectsetup"
	"github.com/sean2077/pairroom/internal/websession"
)

func TestManagementProjectNativeSetupAuthReadOnlyAndInstall(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	endpoint := "/api/v1/projects/" + project.ID + "/native-setup"
	checkpoint := filepath.Join(registry.Root(), "service-registry.json")
	before, err := os.ReadFile(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "POST"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest(method, endpoint, `{}`, false))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", method, response.Code)
		}
		request := managementRequest(method, endpoint, `{}`, false)
		request.Header.Set("Authorization", "Bearer "+server.cliToken)
		response = httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("scoped token %s: %d", method, response.Code)
		}
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("GET", endpoint, "", true))
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("setup status was cacheable")
	}
	var status projectsetup.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Runtimes) != 4 {
		t.Fatal("missing runtimes")
	}
	after, _ := os.ReadFile(checkpoint)
	if !bytes.Equal(before, after) {
		t.Fatal("inspection wrote checkpoint")
	}
	path := filepath.Join(project.Root, ".agents/skills/pairroom-relay/SKILL.md")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("inspection installed skill")
	}
	var selected projectsetup.Item
	for _, runtime := range status.Runtimes {
		if runtime.Runtime == "codex" {
			selected = runtime.Skill
		}
	}
	request := projectsetup.Request{Runtime: "codex", Component: "skill", Revision: selected.Revision}
	body, _ := json.Marshal(request)
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("POST", endpoint, string(body), true))
	if response.Code != http.StatusOK {
		t.Fatalf("install=%d %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project.Root, ".codex/hooks.json")); !os.IsNotExist(err) {
		t.Fatal("unselected hook installed")
	}
	// The consumed first-install revision must never become an overwrite token.
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("POST", endpoint, string(body), true))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale revision accepted: %d", response.Code)
	}
}

func TestManagementProjectNativeSetupConfirmsExistingAndRejectsArbitraryPaths(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	endpoint := "/api/v1/projects/" + project.ID + "/native-setup"
	path := filepath.Join(project.Root, ".codex/hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"permissions":{"keep":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	status, err := projectsetup.Inspect(project.Root)
	if err != nil {
		t.Fatal(err)
	}
	var hook projectsetup.Item
	for _, runtime := range status.Runtimes {
		if runtime.Runtime == "codex" {
			hook = runtime.Hook
		}
	}
	body, _ := json.Marshal(projectsetup.Request{Runtime: "codex", Component: "hook", Revision: hook.Revision})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("POST", endpoint, string(body), true))
	if response.Code != http.StatusConflict {
		t.Fatalf("confirmation not enforced: %d", response.Code)
	}
	current, _ := os.ReadFile(path)
	if string(current) != `{"permissions":{"keep":true}}` {
		t.Fatal("user configuration changed")
	}
	for _, body := range []string{`{"runtime":"codex","component":"skill","root":"/tmp"}`, `{"runtime":"shell","component":"skill"}`, `{"runtime":"claude","component":"../settings.json"}`, `{} {}`} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest("POST", endpoint, body, true))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid body accepted: %d %s", response.Code, body)
		}
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("GET", "/api/v1/projects/not-registered/native-setup", "", true))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown project: %d", response.Code)
	}
	// A registered directory whose Git identity disappeared must not be writable.
	if err := os.RemoveAll(filepath.Join(project.Root, ".git")); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("GET", endpoint, "", true))
	if response.Code != http.StatusConflict {
		t.Fatalf("changed project identity: %d", response.Code)
	}
}

func TestManagementProjectNativeSetupRequiresBrowserCSRF(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	bootstrap := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrap, managementRequest("POST", "/api/v1/session", "", true))
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap: %d", bootstrap.Code)
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	cookies := bootstrap.Result().Cookies()
	if len(cookies) != 1 || session.CSRF == "" {
		t.Fatal("missing browser session")
	}
	status, err := projectsetup.Inspect(project.Root)
	if err != nil {
		t.Fatal(err)
	}
	var revision string
	for _, runtime := range status.Runtimes {
		if runtime.Runtime == "codex" {
			revision = runtime.Skill.Revision
		}
	}
	body, _ := json.Marshal(projectsetup.Request{Runtime: "codex", Component: "skill", Revision: revision})
	endpoint := "/api/v1/projects/" + project.ID + "/native-setup"
	for _, token := range []string{"", "incorrect", session.CSRF} {
		request := managementRequest("POST", endpoint, string(body), false)
		request.AddCookie(cookies[0])
		request.Header.Set(websession.CSRFHeaderName, token)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		want := http.StatusForbidden
		if token == session.CSRF {
			want = http.StatusOK
		}
		if response.Code != want {
			t.Fatalf("CSRF status=%d want=%d body=%s", response.Code, want, response.Body.String())
		}
		if want == http.StatusForbidden {
			if _, err := os.Stat(filepath.Join(project.Root, ".agents")); !os.IsNotExist(err) {
				t.Fatal("rejected CSRF created project files")
			}
		}
	}
}
