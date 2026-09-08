package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/ccswitch"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/model"
)

func pairProfileInput(name string) AgentPairProfileInput {
	return AgentPairProfileInput{Name: name, IsDefault: true, Agents: map[model.ActorID]model.AgentSelection{
		model.ActorClaude: {Runtime: model.RuntimeCodex, Provider: model.NativeProviderRef(), Model: "planner", Effort: "high", Instructions: "Plan and review.", ApprovalPolicy: "on-request", Sandbox: "read-only"},
		model.ActorCodex:  {Runtime: model.RuntimeCodex, Provider: model.NativeProviderRef(), Model: "executor", Effort: "low", Instructions: "Implement and verify.", ApprovalPolicy: "yolo", Sandbox: "danger-full-access"},
	}}
}

func TestAgentPairProfilesPersistIndependentlyOfRoomsAndRegistryIndex(t *testing.T) {
	ctx := context.Background()
	registry, project := testRegistry(t, testGitRepo(t))
	catalog, err := registry.AgentPairProfiles()
	if err != nil || catalog.Schema != 1 || catalog.Profiles == nil || len(catalog.Profiles) != 0 {
		t.Fatalf("empty catalog = %#v, %v", catalog, err)
	}
	input := pairProfileInput(" Daily pair ")
	catalog, err = registry.SaveAgentPairProfile(ctx, "", input)
	if err != nil {
		t.Fatal(err)
	}
	id := catalog.DefaultProfileID
	if id == "" || catalog.Profiles[0].Name != "Daily pair" {
		t.Fatalf("catalog = %#v", catalog)
	}
	// Copies returned to callers are not shared mutable configuration.
	catalog.Profiles[0].Agents[model.ActorClaude] = model.AgentSelection{}
	room, err := registry.ProvisionRoom(ctx, ProvisionRequest{ProjectID: project.ID, Bindings: specs(BindingNew, BindingNew, "")}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(room.Agents, input.Agents) {
		t.Fatalf("default selections = %#v", room.Agents)
	}
	changed := pairProfileInput("Renamed pair")
	selection := changed.Agents[model.ActorClaude]
	selection.Model = "new-planner"
	changed.Agents[model.ActorClaude] = selection
	if _, err := registry.SaveAgentPairProfile(ctx, id, changed); err != nil {
		t.Fatal(err)
	}
	current, _ := registry.Room(room.ID)
	if current.Agents[model.ActorClaude].Model != "planner" {
		t.Fatal("profile update mutated existing Room")
	}
	if err := os.Remove(filepath.Join(registry.Root(), "service-registry.json")); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err = reopened.AgentPairProfiles()
	if err != nil || catalog.DefaultProfileID != id || catalog.Profiles[0].Name != "Renamed pair" {
		t.Fatalf("reopened = %#v, %v", catalog, err)
	}
	replayed, ok := reopened.Room(room.ID)
	if !ok || replayed.Agents[model.ActorClaude].Model != "planner" {
		t.Fatal("Room snapshot changed on replay")
	}
	if _, err := reopened.DeleteAgentPairProfile(ctx, id); err != nil {
		t.Fatal(err)
	}
	catalog, _ = reopened.AgentPairProfiles()
	if catalog.DefaultProfileID != "" || len(catalog.Profiles) != 0 {
		t.Fatalf("deleted = %#v", catalog)
	}
	if _, ok := reopened.Room(room.ID); !ok {
		t.Fatal("profile deletion deleted a Room")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(registry.Root(), agentPairProfilesFile))
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("profile file permissions: %v, %v", info, err)
		}
	}
}

func TestAgentPairProfilesValidateWithoutFreezingNativeDefaults(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	ctx := context.Background()
	input := AgentPairProfileInput{Name: "Inherited", Agents: map[model.ActorID]model.AgentSelection{
		model.ActorClaude: {Runtime: model.RuntimeGrok}, model.ActorCodex: {Runtime: model.RuntimeClaude},
	}}
	catalog, err := registry.SaveAgentPairProfile(ctx, "", input)
	if err != nil {
		t.Fatal(err)
	}
	for _, selection := range catalog.Profiles[0].Agents {
		if selection.Provider.Source != model.ProviderNative || selection.Model != "" || selection.Effort != "" || selection.PermissionMode != "" || selection.ApprovalPolicy != "" || selection.Sandbox != "" {
			t.Fatalf("inheritance changed: %#v", selection)
		}
	}
	if _, err := registry.SaveAgentPairProfile(ctx, "", AgentPairProfileInput{Name: " inherited ", Agents: input.Agents}); !errors.Is(err, errAgentPairProfileConflict) {
		t.Fatalf("duplicate: %v", err)
	}
	for _, name := range []string{"", " \t ", "bad\nname", strings.Repeat("x", 161)} {
		input.Name = name
		if _, err := registry.SaveAgentPairProfile(ctx, "", input); err == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
	for _, agents := range []map[model.ActorID]model.AgentSelection{nil, {}, {model.ActorClaude: {}}, {model.ActorClaude: {}, model.ActorID("unexpected"): {}}} {
		if _, err := registry.SaveAgentPairProfile(ctx, "", AgentPairProfileInput{Name: "invalid", Agents: agents}); err == nil {
			t.Fatalf("accepted selections %#v", agents)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := registry.SaveAgentPairProfile(cancelled, "", pairProfileInput("Cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestAgentPairProfilesFailClosedWithoutOverwritingDamagedFile(t *testing.T) {
	for _, damaged := range []string{"{}", `{"schema":2,"profiles":[]}`, `{"schema":1,"profiles":[],"default_profile_id":"missing"}`, `{"schema":1,"profiles":[]}{}`, `{"schema":1,"profiles":null}`, `{"schema":1,"profiles":[],"token":"do-not-echo"}`, "{"} {
		t.Run(damaged, func(t *testing.T) {
			registry, _ := testRegistry(t, testGitRepo(t))
			path := filepath.Join(registry.Root(), agentPairProfilesFile)
			if err := os.WriteFile(path, []byte(damaged), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := registry.AgentPairProfiles(); !errors.Is(err, errAgentPairProfilesStore) {
				t.Fatalf("read: %v", err)
			}
			if _, err := registry.SaveAgentPairProfile(context.Background(), "", pairProfileInput("new")); !errors.Is(err, errAgentPairProfilesStore) {
				t.Fatalf("save: %v", err)
			}
			data, _ := os.ReadFile(path)
			if string(data) != damaged {
				t.Fatal("damaged file was overwritten")
			}
		})
	}
	registry, _ := testRegistry(t, testGitRepo(t))
	path := filepath.Join(registry.Root(), agentPairProfilesFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SaveAgentPairProfile(context.Background(), "", pairProfileInput("new")); !errors.Is(err, errAgentPairProfilesStore) {
		t.Fatalf("directory accepted: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(t.TempDir(), "external.json")
		if err := os.WriteFile(external, []byte(`{"schema":1,"profiles":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, path); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.SaveAgentPairProfile(context.Background(), "", pairProfileInput("new")); !errors.Is(err, errAgentPairProfilesStore) {
			t.Fatalf("symlink accepted: %v", err)
		}
	}
}

func TestAgentPairProfilesConcurrentWritesKeepOtherProfiles(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := registry.SaveAgentPairProfile(context.Background(), "", pairProfileInput(fmt.Sprintf("Pair %02d", i))); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	catalog, err := registry.AgentPairProfiles()
	if err != nil || len(catalog.Profiles) != 12 || catalog.DefaultProfileID == "" {
		t.Fatalf("catalog: %#v, %v", catalog, err)
	}
}

func pairProfileRequest(t *testing.T, server *ManagementServer, method, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest(method, path, string(encoded), true))
	if response.Code != status {
		t.Fatalf("%s %s = %d: %s", method, path, response.Code, response.Body.String())
	}
	return response
}

func TestManagementAgentPairProfileCRUDAndCreation(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	input := pairProfileInput("Daily pair")
	response := pairProfileRequest(t, server, "POST", agentPairProfilesPath, input, 201)
	var catalog AgentPairProfileCatalog
	if err := json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	id := catalog.DefaultProfileID
	pairProfileRequest(t, server, "GET", agentPairProfilesPath, nil, 200)
	createPath := "/api/v1/projects/" + project.ID + "/rooms"
	for _, override := range []string{"default", "explicit-profile", "explicit-agents"} {
		body := map[string]any{"bindings": specs(BindingNew, BindingNew, "")}
		if override == "explicit-profile" {
			body["agent_pair_profile_id"] = id
		}
		if override == "explicit-agents" {
			body["agents"] = defaultAgentSelections()
		}
		response := pairProfileRequest(t, server, "POST", createPath, body, 201)
		var room Room
		if err := json.Unmarshal(response.Body.Bytes(), &room); err != nil {
			t.Fatal(err)
		}
		if override != "explicit-agents" && !reflect.DeepEqual(room.Agents, input.Agents) {
			t.Fatalf("%s = %#v", override, room.Agents)
		}
		if override == "explicit-agents" && room.Agents[model.ActorClaude].Model == "planner" {
			t.Fatal("default overrode explicit selections")
		}
	}
	pairProfileRequest(t, server, "POST", createPath, map[string]any{"bindings": specs(BindingNew, BindingNew, ""), "agent_pair_profile_id": "missing"}, 404)
	pairProfileRequest(t, server, "POST", createPath, map[string]any{"bindings": specs(BindingNew, BindingNew, ""), "agent_pair_profile_id": id, "agents": input.Agents}, 400)
	input.Name = "Renamed"
	input.IsDefault = false
	pairProfileRequest(t, server, "PUT", agentPairProfilesPath+"/"+id, input, 200)
	catalog, _ = registry.AgentPairProfiles()
	if catalog.DefaultProfileID != "" {
		t.Fatal("unchecking default did not clear it")
	}
	pairProfileRequest(t, server, "PATCH", agentPairProfilesPath+"/default", map[string]string{"profile_id": id}, 200)
	pairProfileRequest(t, server, "PATCH", agentPairProfilesPath+"/default", map[string]string{"profile_id": "missing"}, 404)
	pairProfileRequest(t, server, "PATCH", agentPairProfilesPath+"/default", map[string]string{}, 400)
	pairProfileRequest(t, server, "DELETE", agentPairProfilesPath+"/"+id, nil, 200)
	pairProfileRequest(t, server, "DELETE", agentPairProfilesPath+"/"+id, nil, 404)
	catalog, _ = registry.AgentPairProfiles()
	if len(catalog.Profiles) != 0 || catalog.DefaultProfileID != "" {
		t.Fatalf("delete = %#v", catalog)
	}
}

func TestManagementAgentPairProfilesProtectAuthenticationAndSecretBoundary(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	server, _ := newManagementTestServer(t, registry, SyntheticProvisioner{})
	for _, route := range []struct{ method, path string }{{"GET", agentPairProfilesPath}, {"POST", agentPairProfilesPath}, {"PUT", agentPairProfilesPath + "/p"}, {"DELETE", agentPairProfilesPath + "/p"}, {"PATCH", agentPairProfilesPath + "/default"}} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest(route.method, route.path, "{}", false))
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d", route.method, response.Code)
		}
	}
	for _, body := range []string{
		`{"name":"secret","agents":{"claude":{"runtime":"claude","provider":{"source":"native","api_key":"secret"}},"codex":{"runtime":"codex"}}}`,
		`{"name":"pair","agents":{},"bindings":{}}`,
		`{"name":"pair","agents":{},"collaboration":{}}`,
		`{"name":"pair","agents":null}`,
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest("POST", agentPairProfilesPath, body, true))
		if response.Code != 400 {
			t.Fatalf("unexpected fields accepted: %s = %d", body, response.Code)
		}
	}
	bootstrap := httptest.NewRecorder()
	server.Handler().ServeHTTP(bootstrap, managementRequest("POST", "/api/v1/session", "", true))
	request := managementRequest("POST", agentPairProfilesPath, `{}`, false)
	for _, cookie := range bootstrap.Result().Cookies() {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF = %d", response.Code)
	}
}

func TestManagementAgentPairProfilesRevalidateProvidersAtCreation(t *testing.T) {
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
	input := pairProfileInput("Offline Provider")
	selection := input.Agents[model.ActorCodex]
	selection.Provider = model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "removed"}
	input.Agents[model.ActorCodex] = selection
	pairProfileRequest(t, server, "POST", agentPairProfilesPath, input, 201)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest("POST", "/api/v1/projects/"+project.ID+"/rooms", `{"bindings":{"claude":{"mode":"new"},"codex":{"mode":"new"}}}`, true))
	if response.Code < 400 {
		t.Fatalf("stale Provider was accepted: %s", response.Body.String())
	}
	if len(registry.Snapshot(true).Rooms) != 0 {
		t.Fatal("failed Provider validation provisioned a Room")
	}
}

func TestAgentPairProfileLimitAndIndependentExplicitRoomSelection(t *testing.T) {
	ctx := context.Background()
	registry, project := testRegistry(t, testGitRepo(t))
	input := pairProfileInput("Replacement")
	catalog := AgentPairProfileCatalog{Schema: 1, Profiles: make([]AgentPairProfile, maxAgentPairProfiles)}
	for i := range catalog.Profiles {
		catalog.Profiles[i] = AgentPairProfile{ID: fmt.Sprintf("pair-%d", i), Name: fmt.Sprintf("Pair %d", i), Agents: input.Agents}
	}
	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(registry.Root(), agentPairProfilesFile)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SaveAgentPairProfile(ctx, "", input); !errors.Is(err, errAgentPairProfilesLimit) {
		t.Fatalf("limit = %v", err)
	}
	if _, err := registry.SaveAgentPairProfile(ctx, "pair-0", input); err != nil {
		t.Fatalf("update at limit: %v", err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ProvisionRoom(ctx, ProvisionRequest{ProjectID: project.ID, Bindings: specs(BindingNew, BindingNew, "")}, SyntheticProvisioner{}); !errors.Is(err, errAgentPairProfilesStore) {
		t.Fatalf("implicit profile fallback = %v", err)
	}
	// Broken profile configuration must not affect explicitly supplied pairs.
	if _, err := registry.ProvisionRoom(ctx, ProvisionRequest{ProjectID: project.ID, Bindings: specs(BindingNew, BindingNew, ""), Agents: input.Agents}, SyntheticProvisioner{}); err != nil {
		t.Fatal(err)
	}
}
