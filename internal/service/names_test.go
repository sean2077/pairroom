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
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestTemporaryRoomNamesAndRenameSurviveRebuild(t *testing.T) {
	ctx := context.Background()
	registry, project := testRegistry(t, testGitRepo(t))
	var created []Room
	for _, name := range []string{"", "   "} {
		room, err := registry.ProvisionRoom(ctx, ProvisionRequest{ProjectID: project.ID, Name: name, Bindings: specs(BindingNew, BindingNew, "")}, SyntheticProvisioner{})
		if err != nil {
			t.Fatal(err)
		}
		if room.Name != model.TemporaryRoomName(room.ID) || len(room.RuntimeNames) != 2 {
			t.Fatalf("missing names: %+v", room)
		}
		created = append(created, room)
	}
	if created[0].Name == created[1].Name {
		t.Fatal("temporary names collided")
	}
	before := created[0]
	renamed, err := registry.RenameRoom(ctx, before.ID, "  新名字 $(display only)  ")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "新名字 $(display only)" {
		t.Fatalf("name=%q", renamed.Name)
	}
	if renamed.ID != before.ID || renamed.DataDir != before.DataDir || !reflect.DeepEqual(renamed.Bindings, before.Bindings) || !reflect.DeepEqual(renamed.Agents, before.Agents) || !reflect.DeepEqual(renamed.Collaboration, before.Collaboration) {
		t.Fatal("rename changed durable identity/config")
	}
	for actor, name := range renamed.RuntimeNames {
		if !strings.HasPrefix(name, renamed.Name+" · ") || name == before.RuntimeNames[actor] {
			t.Fatalf("mapping not updated: %s", name)
		}
	}
	path := filepath.Join(before.DataDir, "events.jsonl")
	log, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RenameRoom(ctx, before.ID, renamed.Name); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(log) != string(after) {
		t.Fatal("no-op rename appended another fact")
	}
	if err := os.Remove(filepath.Join(registry.Root(), "service-registry.json")); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	restored, ok := rebuilt.Room(before.ID)
	if !ok || !reflect.DeepEqual(restored, renamed) {
		t.Fatalf("lost renamed facts: got=%+v want=%+v", restored, renamed)
	}
	other, ok := rebuilt.Room(created[1].ID)
	if !ok || other.Name != created[1].Name {
		t.Fatal("unnamed Room changed name on restart")
	}
	// Public projections must never become a second name authority or expose a mutable map.
	restored.RuntimeNames[model.ActorClaude] = "tampered"
	again, _ := rebuilt.Room(before.ID)
	if again.RuntimeNames[model.ActorClaude] == "tampered" {
		t.Fatal("projection aliases registry")
	}
}

func TestRoomNameSubmissionValidation(t *testing.T) {
	for _, tc := range []struct {
		name            string
		optional, valid bool
	}{
		{"", true, true}, {"  ", true, true}, {"", false, false}, {"  Good  ", false, true},
		{strings.Repeat("a", 160), false, true}, {strings.Repeat("a", 161), false, false},
		{strings.Repeat("名", 53), false, true}, {strings.Repeat("名", 54), false, false},
		{"line\nnext", true, false}, {"tab\t", true, false}, {"\x1b[0m", false, false},
		{"\x85", false, false}, {string([]byte{0xff}), false, false},
	} {
		err := validateSubmittedRoomName(tc.name, tc.optional)
		if (err == nil) != tc.valid {
			t.Errorf("name=%q optional=%v err=%v", tc.name, tc.optional, err)
		}
	}
}

func TestRenameHTTPDoesNotDrainForInvalidNoOpOrUnauthenticatedNames(t *testing.T) {
	registry, rooms := provisionRuntimeRooms(t, 1)
	server, manager := newManagementTestServer(t, registry, SyntheticProvisioner{})
	live := activateRuntime(t, manager, rooms[0].ID).(*fakeRuntime)
	live.busy.Store(true)
	defer live.busy.Store(false)
	for _, tc := range []struct {
		name string
		auth bool
		code int
	}{
		{"", true, http.StatusBadRequest}, {strings.Repeat("名", 54), true, http.StatusBadRequest},
		{"bad\nname", true, http.StatusBadRequest}, {rooms[0].Name, true, http.StatusOK},
		{"not authorized", false, http.StatusUnauthorized},
	} {
		body, _ := json.Marshal(map[string]string{"name": tc.name})
		request := managementRequest(http.MethodPatch, "/api/v1/rooms/"+rooms[0].ID, string(body), tc.auth)
		ctx, cancel := context.WithTimeout(request.Context(), time.Second)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request.WithContext(ctx))
		cancel()
		if response.Code != tc.code {
			t.Fatalf("name=%q status=%d body=%s", tc.name, response.Code, response.Body.String())
		}
		if live.drainOnCount.Load() != 0 || live.closeCount.Load() != 0 || live.interruptCount.Load() != 0 {
			t.Fatal("invalid/no-op rename touched native runtime")
		}
	}
	live.busy.Store(false)
	body, _ := json.Marshal(map[string]string{"name": "Renamed safely"})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest(http.MethodPatch, "/api/v1/rooms/"+rooms[0].ID, string(body), true))
	if response.Code != http.StatusOK {
		t.Fatalf("rename=%d %s", response.Code, response.Body.String())
	}
	var room Room
	if err := json.Unmarshal(response.Body.Bytes(), &room); err != nil {
		t.Fatal(err)
	}
	if room.Name != "Renamed safely" || room.ID != rooms[0].ID || live.interruptCount.Load() != 0 || live.closeCount.Load() != 1 {
		t.Fatalf("rename changed ownership or interrupted: %+v", room)
	}
	if len(room.RuntimeNames) != 2 {
		t.Fatal("HTTP receipt omitted native names")
	}
}

func TestCreateHTTPAcceptsOmittedRoomName(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", `{"bindings":{"claude":{"mode":"new"},"codex":{"mode":"new"}},"collaboration":{"mode":"custom","instructions":"Agent 1 plans; Agent 2 executes and verifies."}}`, true))
	if response.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", response.Code, response.Body.String())
	}
	var room Room
	if err := json.Unmarshal(response.Body.Bytes(), &room); err != nil {
		t.Fatal(err)
	}
	if room.Name != model.TemporaryRoomName(room.ID) || len(room.RuntimeNames) != 2 || room.Collaboration.Mode != model.CollaborationCustom {
		t.Fatalf("bad default names or lost collaboration %+v", room)
	}
}
