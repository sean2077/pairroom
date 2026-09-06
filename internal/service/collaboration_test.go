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
			if meta.Collaboration == nil || *meta.Collaboration != want || provisioned.Schema != 3 || provisioned.Collaboration == nil || *provisioned.Collaboration != want {
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
	// Metadata 10 fails closed in older builds; this release does not relabel
	// existing schema-9 logs, but creates new-mode Rooms with a new boundary.
	data, err := os.ReadFile(filepath.Join(created.DataDir, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Schema int `json:"schema_version"`
	}
	if json.Unmarshal(data, &meta) != nil || meta.Schema != 10 {
		t.Fatalf("new-mode metadata=%s", data)
	}
}
