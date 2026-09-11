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

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func TestCollaborationIsCreatedOnceAndRecoveredFromRoomFacts(t *testing.T) {
	root := t.TempDir()
	registry, project := testRegistryWithRoot(t, root, testGitRepo(t))
	for _, mode := range []string{model.CollaborationDefault, model.CollaborationCustom} {
		t.Run(mode, func(t *testing.T) {
			spec := &model.Collaboration{Mode: mode}
			if mode == model.CollaborationCustom {
				spec.Instructions = "Agent 2 plans; Agent 1 tests. 保留中文规则。"
			}
			created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: mode, Collaboration: spec, Bindings: specs(BindingNew, BindingNew, mode)}, SyntheticProvisioner{})
			if err != nil {
				t.Fatal(err)
			}
			want := *created.Collaboration
			spec.Instructions = "caller mutation"
			created.Collaboration.Instructions = "response mutation"
			saved, ok := registry.Room(created.ID)
			if !ok || *saved.Collaboration != want {
				t.Fatal("immutable spec aliases its caller")
			}
			st, err := store.Open(saved.DataDir)
			if err != nil {
				t.Fatal(err)
			}
			events, err := st.Load()
			_ = st.Close()
			if err != nil {
				t.Fatal(err)
			}
			var meta model.RoomMeta
			var provisioned roomProvisionedPayload
			for _, event := range events {
				switch event.Kind {
				case "room.created":
					_ = json.Unmarshal(event.Data, &meta)
				case EventRoomProvisioned:
					_ = json.Unmarshal(event.Data, &provisioned)
				}
			}
			if meta.Collaboration == nil || *meta.Collaboration != want || provisioned.Schema != 4 || provisioned.Collaboration == nil || *provisioned.Collaboration != want {
				t.Fatal("Room and service authorities disagree")
			}
			reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			got, ok := reopened.Room(saved.ID)
			if !ok || got.Collaboration == nil || *got.Collaboration != want {
				t.Fatalf("recovered=%+v", got)
			}
		})
	}
}

func TestInvalidModeFailsBeforeNativeBindingValidation(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	calls := 0
	provisioner := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		calls++
		return Binding{}, nil, nil
	})
	for _, spec := range []model.Collaboration{{Mode: "discussion"}, {Mode: model.CollaborationCustom}, {Mode: model.CollaborationCustom, Instructions: strings.Repeat("x", model.MaxCollaborationInstructionsBytes+1)}} {
		if _, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "invalid", Collaboration: &spec, Bindings: specs(BindingNew, BindingNew, "")}, provisioner); err == nil {
			t.Fatal("invalid creation succeeded")
		}
	}
	if calls != 0 {
		t.Fatal("invalid mode launched a validation runtime")
	}
}

func TestManagementHTTPAcceptsCreationCollaboration(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	manager, _ := NewRuntimeManager(registry, (&fakeRuntimeFactory{}).open, RuntimeManagerConfig{Limit: 2})
	defer manager.Shutdown(context.Background())
	server, err := NewManagementServer(ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, Token: "management-secret"})
	if err != nil {
		t.Fatal(err)
	}
	bindings := `{"claude":{"mode":"new"},"codex":{"mode":"new"}}`
	defaultBody := `{"name":"ui-default","bindings":` + bindings + `,"collaboration":{"mode":"default"}}`
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", defaultBody, true))
	if response.Code != http.StatusCreated {
		t.Fatalf("default create status=%d body=%s", response.Code, response.Body.String())
	}
	var created Room
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	want, _ := (model.Collaboration{}).ForCreation()
	if created.Collaboration == nil || *created.Collaboration != want {
		t.Fatalf("default collaboration=%+v", created.Collaboration)
	}

	custom := "Agent 2 plans; Agent 1 implements. 保留中文规则。"
	customBody, _ := json.Marshal(map[string]any{
		"name": "ui-custom", "bindings": specs(BindingNew, BindingNew, ""),
		"collaboration": map[string]string{"mode": model.CollaborationCustom, "instructions": custom},
	})
	customResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(customResponse, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", string(customBody), true))
	if customResponse.Code != http.StatusCreated {
		t.Fatalf("custom create status=%d body=%s", customResponse.Code, customResponse.Body.String())
	}
	if err := json.Unmarshal(customResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Collaboration == nil || created.Collaboration.Mode != model.CollaborationCustom || created.Collaboration.Instructions != custom {
		t.Fatalf("custom collaboration=%+v", created.Collaboration)
	}

	rejected := httptest.NewRecorder()
	server.Handler().ServeHTTP(rejected, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", `{"name":"bad","bindings":`+bindings+`,"collaboration":{"mode":"discussion"}}`, true))
	if rejected.Code != http.StatusBadRequest || !strings.Contains(rejected.Body.String(), "invalid collaboration mode") {
		t.Fatalf("invalid mode status=%d body=%s", rejected.Code, rejected.Body.String())
	}
}

func TestManagementRejectsChangingCollaborationAfterCreation(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "fixed", Bindings: specs(BindingNew, BindingNew, "")}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := NewRuntimeManager(registry, (&fakeRuntimeFactory{}).open, RuntimeManagerConfig{Limit: 2})
	defer manager.Shutdown(context.Background())
	server, err := NewManagementServer(ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, Token: "management-secret"})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"name":"changed","collaboration":{"mode":"custom","instructions":"override"}}`, `{"name":"changed","mode":"custom"}`} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest(http.MethodPatch, "/api/v1/rooms/"+created.ID, body, true))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("mutation status=%d body=%s", response.Code, response.Body.String())
		}
	}
	after, _ := registry.Room(created.ID)
	if *after.Collaboration != *created.Collaboration || after.Name != created.Name {
		t.Fatal("rejected mutation changed state")
	}
	// New Rooms use schema 11; existing schema-10 Rooms remain byte-identical.
	data, err := os.ReadFile(filepath.Join(created.DataDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Schema int `json:"schema_version"`
	}
	if json.Unmarshal(data, &meta) != nil || meta.Schema != 11 {
		t.Fatalf("new-mode metadata=%s", data)
	}
}
