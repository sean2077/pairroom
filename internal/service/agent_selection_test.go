package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/ccswitch"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/model"
)

func TestManagementCreatesImmutablePerRoomAgentSelections(t *testing.T) {
	repo := testGitRepo(t)
	root := t.TempDir()
	registry, project := testRegistryWithRoot(t, root, repo)
	defaults := config.Defaults()
	reader, err := ccswitch.NewReader(filepath.Join(t.TempDir(), "missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewAgentResolver(AgentResolverConfig{Defaults: defaults.DefaultSelections(), Runtimes: defaults.Runtimes, CCSwitch: reader, Mock: true})
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewRuntimeManager(registry, (&fakeRuntimeFactory{}).open, RuntimeManagerConfig{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	server, err := NewManagementServer(ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, AgentResolver: resolver, Token: "management-secret"})
	if err != nil {
		t.Fatal(err)
	}

	agents := map[model.ActorID]model.AgentSelection{
		model.ActorClaude: {Runtime: model.RuntimeGrok, Provider: model.NativeProviderRef(), Model: "grok-custom-a", PermissionMode: "default", OrdinaryReviewerPolicy: model.ReviewerEnforced},
		model.ActorCodex:  {Runtime: model.RuntimeGrok, Provider: model.NativeProviderRef(), Model: "grok-custom-b", PermissionMode: "always-approve", Sandbox: "workspace", OrdinaryReviewerPolicy: model.ReviewerExplicit},
	}
	body, _ := json.Marshal(map[string]any{"name": "two grok slots", "bindings": specs(BindingNew, BindingNew, ""), "agents": agents})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", string(body), true))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created Room
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.LegacyDefaults || created.Agents[model.ActorClaude].Runtime != model.RuntimeGrok || created.Agents[model.ActorCodex].Runtime != model.RuntimeGrok {
		t.Fatalf("Room selections = %#v", created)
	}
	if created.Agents[model.ActorCodex].OrdinaryReviewerPolicy != model.ReviewerExplicit || created.Agents[model.ActorCodex].Model != "grok-custom-b" {
		t.Fatalf("Agent 2 selection = %#v", created.Agents[model.ActorCodex])
	}

	reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	replayed, ok := reopened.Room(created.ID)
	if !ok || replayed.Agents[model.ActorClaude].Model != "grok-custom-a" || replayed.LegacyDefaults {
		t.Fatalf("replayed Room = %#v ok=%v", replayed, ok)
	}

	partial, _ := json.Marshal(map[string]any{"name": "partial", "bindings": specs(BindingNew, BindingNew, ""), "agents": map[model.ActorID]model.AgentSelection{model.ActorClaude: agents[model.ActorClaude]}})
	rejected := httptest.NewRecorder()
	server.Handler().ServeHTTP(rejected, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", string(partial), true))
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("partial status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	empty := httptest.NewRecorder()
	server.Handler().ServeHTTP(empty, managementRequest(http.MethodPost, "/api/v1/projects/"+project.ID+"/rooms", `{"name":"empty agents","bindings":{"claude":{"mode":"new"},"codex":{"mode":"new"}},"agents":{}}`, true))
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty agents status=%d body=%s", empty.Code, empty.Body.String())
	}

	mutation := httptest.NewRecorder()
	server.Handler().ServeHTTP(mutation, managementRequest(http.MethodPatch, "/api/v1/rooms/"+created.ID, `{"name":"changed","agents":{}}`, true))
	if mutation.Code != http.StatusBadRequest {
		t.Fatalf("Agent mutation status=%d body=%s", mutation.Code, mutation.Body.String())
	}
}

func TestAgentCatalogReturnsAllRuntimesAndSafeProviderFailure(t *testing.T) {
	registry, _ := testRegistry(t, testGitRepo(t))
	cfg := config.Defaults()
	reader, _ := ccswitch.NewReader(filepath.Join(t.TempDir(), "missing.db"))
	resolver, _ := NewAgentResolver(AgentResolverConfig{Defaults: cfg.DefaultSelections(), Runtimes: cfg.Runtimes, CCSwitch: reader, Mock: true})
	manager, _ := NewRuntimeManager(registry, (&fakeRuntimeFactory{}).open, RuntimeManagerConfig{Limit: 2})
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	server, err := NewManagementServer(ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: SyntheticProvisioner{}, AgentResolver: resolver, Token: "management-secret"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, managementRequest(http.MethodGet, "/api/v1/agent-catalog", "", true))
	if recorder.Code != http.StatusOK {
		t.Fatalf("catalog status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var catalog AgentCatalog
	if err := json.Unmarshal(recorder.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Runtimes) != 3 || catalog.ProviderError == nil || catalog.ProviderError.Code != ccswitch.CodeDatabaseMissing {
		t.Fatalf("catalog = %#v", catalog)
	}
	for _, runtime := range catalog.Runtimes {
		if !runtime.Available {
			t.Fatalf("mock runtime unavailable: %#v", runtime)
		}
	}
}
