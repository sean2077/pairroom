package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredManagementRoutesAreAbsentAndCannotMutateRoom(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Current Room", Bindings: specs(BindingNew, BindingNew, "removed-api"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &recordingProvisioner{}
	server, _ := newManagementTestServer(t, registry, provisioner)
	path := filepath.Join(room.DataDir, "events.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/import", "/api/v1/rooms/" + room.ID + "/bindings"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest(http.MethodPost, path, `{}`, true))
		if response.Code != http.StatusNotFound {
			t.Errorf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("removed endpoint changed log: %v", err)
	}
	provisioner.mu.Lock()
	defer provisioner.mu.Unlock()
	if len(provisioner.calls) != 0 {
		t.Fatalf("removed endpoint called provisioner: %v", provisioner.calls)
	}
	if len(registry.Snapshot(true).Rooms) != 1 {
		t.Fatal("removed endpoint changed registry")
	}
	for _, asset := range []string{"/", "/management.js"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest(http.MethodGet, asset, "", false))
		for _, removed := range []string{"binding-dialog", "project-mode-import", "completeBindings", "ui.importLegacyRoom", "legacy_defaults"} {
			if strings.Contains(response.Body.String(), removed) {
				t.Errorf("%s still advertises %s", asset, removed)
			}
		}
	}
}

func TestRoomCreationRejectsRetiredRolePolicyBeforeProvisioning(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	provisioner := &recordingProvisioner{}
	server, _ := newManagementTestServer(t, registry, provisioner)
	agents := map[string]any{}
	data, err := json.Marshal(defaultAgentSelections())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &agents); err != nil {
		t.Fatal(err)
	}
	agents["claude"].(map[string]any)["ordinary_reviewer_policy"] = "explicit"
	body, err := json.Marshal(map[string]any{"name": "invalid", "agents": agents, "bindings": specs(BindingNew, BindingNew, "removed-policy")})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", string(body), true))
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "ordinary_reviewer_policy") {
		t.Fatalf("retired field accepted: status=%d body=%s", response.Code, response.Body.String())
	}
	if len(registry.Snapshot(true).Rooms) != 0 || len(provisioner.calls) != 0 {
		t.Fatal("invalid selection reached provisioning")
	}
}
