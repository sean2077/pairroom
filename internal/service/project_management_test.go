package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestRemoveProjectPersistsEmptyRemovalWithoutTouchingWorktree(t *testing.T) {
	ctx := context.Background()
	dataRoot := t.TempDir()
	repo := testGitRepo(t)
	marker := filepath.Join(repo, "keep-me.txt")
	if err := os.WriteFile(marker, []byte("worktree data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	removed, err := registry.RemoveProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if removed != project {
		t.Fatalf("removed project=%#v want=%#v", removed, project)
	}
	if _, ok := registry.Project(project.ID); ok {
		t.Fatalf("project %s remained registered", project.ID)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "worktree data\n" {
		t.Fatalf("source worktree changed: data=%q err=%v", data, err)
	}

	checkpoint, err := os.ReadFile(filepath.Join(dataRoot, "service-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot RegistrySnapshot
	if err := json.Unmarshal(checkpoint, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Projects) != 0 || len(snapshot.Rooms) != 0 {
		t.Fatalf("removal checkpoint retained state: %#v", snapshot)
	}

	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reopened.Project(project.ID); ok {
		t.Fatalf("project %s returned after Registry restart", project.ID)
	}
	if _, err := reopened.RegisterProject(ctx, repo); err != nil {
		t.Fatalf("worktree could not be registered again: %v", err)
	}
}

func TestRemoveProjectRollsBackWhenCheckpointCannotBePublished(t *testing.T) {
	ctx := context.Background()
	dataRoot := t.TempDir()
	registry, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(ctx, testGitRepo(t))
	if err != nil {
		t.Fatal(err)
	}

	checkpoint := filepath.Join(dataRoot, "service-registry.json")
	if err := os.Remove(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(checkpoint, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.RemoveProject(ctx, project.ID); err == nil || !strings.Contains(err.Error(), "persist project removal") {
		t.Fatalf("RemoveProject error=%v want pre-publish persistence failure", err)
	}
	if projected, ok := registry.Project(project.ID); !ok || projected != project {
		t.Fatalf("failed removal was not rolled back: %#v ok=%v", projected, ok)
	}
	if err := registry.Healthy(); err != nil {
		t.Fatalf("pre-publish checkpoint failure poisoned Registry: %v", err)
	}

	if err := os.Remove(checkpoint); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RemoveProject(ctx, project.ID); err != nil {
		t.Fatalf("removal did not recover after checkpoint path repair: %v", err)
	}
}

func TestRemoveProjectRejectsActiveAndArchivedRooms(t *testing.T) {
	for _, archive := range []bool{false, true} {
		name := "active"
		if archive {
			name = "archived"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dataRoot := t.TempDir()
			registry, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
			if err != nil {
				t.Fatal(err)
			}
			project, err := registry.RegisterProject(ctx, testGitRepo(t))
			if err != nil {
				t.Fatal(err)
			}
			room, err := registry.ProvisionRoom(ctx, ProvisionRequest{
				ProjectID: project.ID,
				Name:      "Retained Room",
				Bindings:  specs(BindingNew, BindingNew, name),
			}, SyntheticProvisioner{})
			if err != nil {
				t.Fatal(err)
			}
			if archive {
				if _, err := registry.ArchiveRoom(ctx, room.ID); err != nil {
					t.Fatal(err)
				}
			}

			_, err = registry.RemoveProject(ctx, project.ID)
			if !errors.Is(err, ErrProjectHasRooms) {
				t.Fatalf("RemoveProject error=%v want ErrProjectHasRooms", err)
			}
			var retained *ProjectHasRoomsError
			if !errors.As(err, &retained) || retained.ProjectID != project.ID || len(retained.RoomIDs) != 1 || retained.RoomIDs[0] != room.ID {
				t.Fatalf("unexpected retained-room diagnostic: %#v (%v)", retained, err)
			}
			if _, ok := registry.Project(project.ID); !ok {
				t.Fatal("failed removal unregistered the Project")
			}
			if _, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot}); err != nil {
				t.Fatalf("Registry did not remain restartable: %v", err)
			}
		})
	}
}

func TestRemoveProjectSerializesWithRoomProvisioning(t *testing.T) {
	ctx := context.Background()
	registry, project := testRegistry(t, testGitRepo(t))
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	provisioner := ProvisionerFunc(func(ctx context.Context, project Project, actor model.ActorID, spec BindingSpec, dataDir string) (Binding, func(context.Context) error, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return Binding{}, nil, ctx.Err()
		}
		return SyntheticProvisioner{}.Provision(ctx, project, actor, spec, dataDir)
	})

	provisioned := make(chan error, 1)
	go func() {
		_, err := registry.ProvisionRoom(ctx, ProvisionRequest{
			ProjectID: project.ID,
			Name:      "Concurrent Room",
			Bindings:  specs(BindingNew, BindingNew, "concurrent-remove"),
		}, provisioner)
		provisioned <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("provisioner did not enter")
	}

	removed := make(chan error, 1)
	go func() {
		_, err := registry.RemoveProject(ctx, project.ID)
		removed <- err
	}()
	close(release)
	if err := <-provisioned; err != nil {
		t.Fatalf("provisioning failed: %v", err)
	}
	if err := <-removed; !errors.Is(err, ErrProjectHasRooms) {
		t.Fatalf("concurrent removal error=%v want ErrProjectHasRooms", err)
	}
	if snapshot := registry.Snapshot(true); len(snapshot.Projects) != 1 || len(snapshot.Rooms) != 1 {
		t.Fatalf("concurrent result is not atomic: %#v", snapshot)
	}
}

func TestRefreshProjectPersistsAvailability(t *testing.T) {
	ctx := context.Background()
	dataRoot := t.TempDir()
	repo := testGitRepo(t)
	registry, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	moved := repo + "-moved"
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	unavailable, err := registry.RefreshProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unavailable.Available || strings.TrimSpace(unavailable.Diagnostic) == "" {
		t.Fatalf("missing unavailable diagnostic: %#v", unavailable)
	}
	if projected, ok := registry.Project(project.ID); !ok || projected.Available || projected.Diagnostic != unavailable.Diagnostic {
		t.Fatalf("unavailable state was not projected: %#v ok=%v", projected, ok)
	}

	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	if persisted, ok := reopened.Project(project.ID); !ok || persisted.Available || persisted.Diagnostic == "" {
		t.Fatalf("unavailable state was not durable: %#v ok=%v", persisted, ok)
	}

	if err := os.Rename(moved, repo); err != nil {
		t.Fatal(err)
	}
	available, err := reopened.RefreshProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !available.Available || available.Diagnostic != "" || available.Root != project.Root {
		t.Fatalf("Project did not recover: %#v", available)
	}
}

func TestRefreshProjectRollsBackWhenCheckpointCannotBePublished(t *testing.T) {
	ctx := context.Background()
	dataRoot := t.TempDir()
	repo := testGitRepo(t)
	registry, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	moved := repo + "-moved"
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}

	checkpoint := filepath.Join(dataRoot, "service-registry.json")
	if err := os.Remove(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(checkpoint, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := registry.RefreshProject(ctx, project.ID); err == nil || !strings.Contains(err.Error(), "persist project refresh") {
		t.Fatalf("RefreshProject error=%v want pre-publish persistence failure", err)
	}
	if projected, ok := registry.Project(project.ID); !ok || projected != project {
		t.Fatalf("failed refresh was not rolled back: %#v ok=%v", projected, ok)
	}
	if err := registry.Healthy(); err != nil {
		t.Fatalf("pre-publish checkpoint failure poisoned Registry: %v", err)
	}

	if err := os.Remove(checkpoint); err != nil {
		t.Fatal(err)
	}
	unavailable, err := registry.RefreshProject(ctx, project.ID)
	if err != nil {
		t.Fatalf("refresh did not recover after checkpoint path repair: %v", err)
	}
	if unavailable.Available || unavailable.Diagnostic == "" {
		t.Fatalf("recovered refresh did not persist unavailable state: %#v", unavailable)
	}
}

func TestManagementProjectRefreshAndRemovalEndpoints(t *testing.T) {
	t.Run("confirmation and success", func(t *testing.T) {
		registry, project := testRegistry(t, testGitRepo(t))
		server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})

		mismatch := httptest.NewRecorder()
		server.Handler().ServeHTTP(mismatch, managementRequest(http.MethodDelete, "/api/v1/projects/"+project.ID, `{"confirm_project_id":"wrong"}`, true))
		if mismatch.Code != http.StatusBadRequest || !strings.Contains(mismatch.Body.String(), "exactly match") {
			t.Fatalf("confirmation mismatch status=%d body=%s", mismatch.Code, mismatch.Body.String())
		}
		if _, ok := registry.Project(project.ID); !ok {
			t.Fatal("confirmation mismatch removed Project")
		}

		refresh := httptest.NewRecorder()
		server.Handler().ServeHTTP(refresh, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/refresh", "", true))
		if refresh.Code != http.StatusOK {
			t.Fatalf("refresh status=%d body=%s", refresh.Code, refresh.Body.String())
		}

		removed := httptest.NewRecorder()
		body := `{"confirm_project_id":"` + project.ID + `"}`
		server.Handler().ServeHTTP(removed, managementRequest(http.MethodDelete, "/api/v1/projects/"+project.ID, body, true))
		if removed.Code != http.StatusNoContent || removed.Body.Len() != 0 {
			t.Fatalf("remove status=%d body=%s", removed.Code, removed.Body.String())
		}
		if _, ok := registry.Project(project.ID); ok {
			t.Fatal("successful DELETE left Project registered")
		}
	})

	t.Run("missing worktree remains removable", func(t *testing.T) {
		ctx := context.Background()
		dataRoot := t.TempDir()
		repo := testGitRepo(t)
		registry, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
		if err != nil {
			t.Fatal(err)
		}
		project, err := registry.RegisterProject(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})

		if err := os.RemoveAll(repo); err != nil {
			t.Fatal(err)
		}
		refresh := httptest.NewRecorder()
		server.Handler().ServeHTTP(refresh, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/refresh", "", true))
		if refresh.Code != http.StatusOK {
			t.Fatalf("refresh missing worktree status=%d body=%s", refresh.Code, refresh.Body.String())
		}
		var unavailable Project
		if err := json.Unmarshal(refresh.Body.Bytes(), &unavailable); err != nil {
			t.Fatal(err)
		}
		if unavailable.Available || unavailable.Diagnostic == "" {
			t.Fatalf("missing worktree was not reported unavailable: %#v", unavailable)
		}
		if _, err := os.Stat(repo); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("refresh recreated or changed missing worktree: %v", err)
		}

		removed := httptest.NewRecorder()
		body := `{"confirm_project_id":"` + project.ID + `"}`
		server.Handler().ServeHTTP(removed, managementRequest(http.MethodDelete, "/api/v1/projects/"+project.ID, body, true))
		if removed.Code != http.StatusNoContent || removed.Body.Len() != 0 {
			t.Fatalf("remove missing-worktree Project status=%d body=%s", removed.Code, removed.Body.String())
		}
		if _, ok := registry.Project(project.ID); ok {
			t.Fatal("missing-worktree Project remained registered")
		}
		if _, err := os.Stat(repo); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removal recreated or changed missing worktree: %v", err)
		}

		reopened, err := OpenRegistry(ctx, RegistryConfig{Root: dataRoot})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reopened.Project(project.ID); ok {
			t.Fatal("missing-worktree Project returned after Registry restart")
		}
	})

	t.Run("retained Room conflict is structured", func(t *testing.T) {
		registry, project := testRegistry(t, testGitRepo(t))
		room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
			ProjectID: project.ID,
			Name:      "API retained Room",
			Bindings:  specs(BindingNew, BindingNew, "api-remove"),
		}, SyntheticProvisioner{})
		if err != nil {
			t.Fatal(err)
		}
		server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
		recorder := httptest.NewRecorder()
		body := `{"confirm_project_id":"` + project.ID + `"}`
		server.Handler().ServeHTTP(recorder, managementRequest(http.MethodDelete, "/api/v1/projects/"+project.ID, body, true))
		if recorder.Code != http.StatusConflict {
			t.Fatalf("remove in-use status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Error   string `json:"error"`
			Code    string `json:"code"`
			Details struct {
				ProjectID string   `json:"project_id"`
				RoomCount int      `json:"room_count"`
				RoomIDs   []string `json:"room_ids"`
				Truncated bool     `json:"truncated"`
			} `json:"details"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Code != "project_has_rooms" || response.Details.ProjectID != project.ID || response.Details.RoomCount != 1 ||
			len(response.Details.RoomIDs) != 1 || response.Details.RoomIDs[0] != room.ID || response.Details.Truncated || response.Error == "" {
			t.Fatalf("unexpected conflict response: %#v", response)
		}
	})
}
