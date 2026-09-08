package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/ccswitch"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/model"
)

func diagnosticTestServer(t *testing.T) (*ManagementServer, Project) {
	t.Helper()
	registry, project := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	cfg := config.Defaults()
	reader, err := ccswitch.NewReader(filepath.Join(t.TempDir(), "missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	server.agentResolver, err = NewAgentResolver(AgentResolverConfig{Defaults: cfg.DefaultSelections(), Runtimes: cfg.Runtimes, CCSwitch: reader, Mock: true})
	if err != nil {
		t.Fatal(err)
	}
	return server, project
}

func TestManagementDiagnosticsBoundaries(t *testing.T) {
	server, _ := diagnosticTestServer(t)
	for _, test := range []struct {
		method, body string
		auth         bool
		status       int
	}{
		{"POST", `{"mode":"environment"}`, false, 401},
		{"GET", "", true, 405},
		{"POST", `{"mode":"runtime","actor":"claude"}`, true, 400},
		{"POST", `{"mode":"runtime","actor":"invalid","confirm":true}`, true, 400},
		{"POST", `{"mode":"unknown"}`, true, 400},
		{"POST", `{"mode":"environment","command":"bad-command"}`, true, 400},
		{"POST", `{"mode":"environment"} {}`, true, 400},
		{"POST", `{"mode":"runtime","actor":"claude","confirm":true,"room_id":"missing"}`, true, 404},
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest(test.method, diagnosticsPath, test.body, test.auth))
		if response.Code != test.status {
			t.Fatalf("%s %s: got %d want %d: %s", test.method, test.body, response.Code, test.status, response.Body.String())
		}
	}
	bootstrap := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrap, managementRequest("POST", "/api/v1/session", "", true))
	request := managementRequest("POST", diagnosticsPath, `{"mode":"environment"}`, false)
	for _, cookie := range bootstrap.Result().Cookies() {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("CSRF not enforced: %d", response.Code)
	}
	server.diagnosticsMu.Lock()
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("POST", diagnosticsPath, `{"mode":"environment"}`, true))
	server.diagnosticsMu.Unlock()
	if response.Code != http.StatusConflict {
		t.Fatalf("concurrent checks admitted: %d", response.Code)
	}
}

func TestManagementDiagnosticsMockAndExport(t *testing.T) {
	server, project := diagnosticTestServer(t)
	room, err := server.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "private-room-name", Bindings: specs(BindingNew, BindingNew, "private"), Agents: defaultAgentSelections()}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	before := server.registry.Snapshot(true)
	for _, body := range []string{`{"mode":"environment"}`, `{"mode":"runtime","actor":"codex","confirm":true,"room_id":"` + room.ID + `"}`} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest("POST", diagnosticsPath, body, true))
		if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("diagnostics failed: %d %s", response.Code, response.Body.String())
		}
		var report DiagnosticReport
		if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		mock := false
		for _, check := range report.Checks {
			mock = mock || (check.Status == "skipped" && check.Code == "mock")
			if check.Code == "responded" || check.Code == "started" {
				t.Fatal("Mock falsely reported native verification")
			}
		}
		if !mock {
			t.Fatalf("Mock not explicitly labelled: %s", response.Body.String())
		}
		for _, value := range []string{server.Token(), project.Root, project.ID, room.ID, room.Name, "private"} {
			if strings.Contains(response.Body.String(), value) {
				t.Fatalf("export leaked %q", value)
			}
		}
	}
	after := server.registry.Snapshot(true)
	if !reflect.DeepEqual(before.Rooms, after.Rooms) || len(server.runtimes.Statuses()) != 0 {
		t.Fatal("diagnostic mutated or activated Room state")
	}
	entries, err := os.ReadDir(server.registry.Root())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".diagnostic-") {
			t.Fatal("storage check left a marker")
		}
	}
}

func TestManagementDiagnosticsHonorsDefaultProfileAndStoredRoom(t *testing.T) {
	server, project := diagnosticTestServer(t)
	room, err := server.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "native-room", Bindings: specs(BindingNew, BindingNew, "stored"), Agents: defaultAgentSelections()}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	input := pairProfileInput("private-default-profile")
	selection := input.Agents[model.ActorCodex]
	selection.Provider = model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "removed"}
	input.Agents[model.ActorCodex] = selection
	input.IsDefault = true
	if _, err := server.registry.SaveAgentPairProfile(context.Background(), "", input); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ roomID, scope, code string }{
		{"", "default_profile", "provider_unavailable"},
		{room.ID, "room", "mock"},
	} {
		body, _ := json.Marshal(diagnosticRequest{Mode: "runtime", Actor: model.ActorCodex, Confirm: true, RoomID: test.roomID})
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest("POST", diagnosticsPath, string(body), true))
		var report DiagnosticReport
		if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, check := range report.Checks {
			found = found || check.Code == test.code
		}
		if response.Code != 200 || report.Scope != test.scope || !found {
			t.Fatalf("wrong selection or fallback: %s", response.Body.String())
		}
	}
}
