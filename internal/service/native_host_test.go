package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
	"github.com/sean2077/pairroom/internal/store"
)

type nativeFixture struct {
	registry *Registry
	room     Room
	project  Project
	manager  *RuntimeManager
	server   *httptest.Server
	native   *nativeHostRuntime
	endpoint string
}

func nativeHTTP(t *testing.T) *nativeFixture {
	t.Helper()
	registry, project := testRegistry(t, testGitRepo(t))
	noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		t.Error("native provisioning spawned an adapter")
		return Binding{}, nil, errors.New("must not spawn")
	})
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "Native test", HostMode: model.HostNative}, noSpawn)
	if err != nil {
		t.Fatal(err)
	}
	factory := EmbeddedRuntimeFactory(registry, EmbeddedRuntimeConfig{Claude: agent.Config{Command: "missing-do-not-spawn-claude"}, Codex: agent.Config{Command: "missing-do-not-spawn-codex"}})
	manager, err := NewRuntimeManager(registry, factory, RuntimeManagerConfig{Limit: 5, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	management, err := NewManagementServer(ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: noSpawn, Token: "management-secret"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(management.Handler())
	t.Cleanup(func() {
		server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = manager.Shutdown(ctx)
	})
	endpoint := filepath.Join(registry.Root(), relay.EndpointFile)
	if err := relay.WriteEndpoint(registry.Root(), relay.Endpoint{URL: server.URL, Token: "management-secret"}); err != nil {
		t.Fatal(err)
	}
	runtime, _, err := manager.Activate(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	native, ok := runtime.(*nativeHostRuntime)
	if !ok {
		t.Fatal("native Room became embedded runtime")
	}
	return &nativeFixture{registry: registry, room: room, project: project, manager: manager, server: server, native: native, endpoint: endpoint}
}
func (f *nativeFixture) run(t *testing.T, args []string, input any) ([]byte, error) {
	t.Helper()
	var in []byte
	if input != nil {
		in, _ = json.Marshal(input)
	}
	var out, diagnostic bytes.Buffer
	args = append(args, "--repo", f.project.Root)
	err := relayclient.Run(context.Background(), args, bytes.NewReader(in), &out, &diagnostic)
	return out.Bytes(), err
}
func (f *nativeFixture) bind(t *testing.T, slot model.ActorID) (relay.Auth, string) {
	t.Helper()
	kind := f.room.Agents[slot].Runtime
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if _, err := f.run(t, []string{"install", "--runtime", string(kind)}, nil); err != nil {
		t.Fatal(err)
	}
	output, err := f.run(t, []string{"bind", "--room", f.room.ID, "--slot", string(slot), "--service-file", f.endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Binding relay.Binding `json:"binding"`
		Nonce   string        `json:"bind_nonce"`
	}
	if json.Unmarshal(output, &result) != nil || result.Nonce == "" {
		t.Fatal("nonce missing")
	}
	data, err := os.ReadFile(filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", string(slot), "credentials"))
	if err != nil {
		t.Fatal(err)
	}
	var cred struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(data, &cred)
	if strings.Contains(string(output), cred.Secret) || strings.Contains(string(output), "management-secret") {
		t.Fatal("bind stdout exposed long-lived credentials")
	}
	return relay.Auth{Slot: slot, BindID: result.Binding.BindID, Generation: result.Binding.Generation, Secret: cred.Secret, SessionID: "official-session-" + string(slot)}, result.Nonce
}
func (f *nativeFixture) hook(t *testing.T, a relay.Auth, text string, active bool) ([]byte, error) {
	t.Helper()
	return f.run(t, []string{"hook", "--runtime", string(f.room.Agents[a.Slot].Runtime)}, map[string]any{"hook_event_name": "Stop", "session_id": a.SessionID, "cwd": f.project.Root, "last_assistant_message": text, "stop_hook_active": active, "transcript_path": "/unavailable/optional/transcript"})
}
func associateCLI(t *testing.T, f *nativeFixture, slot model.ActorID) relay.Auth {
	t.Helper()
	a, nonce := f.bind(t, slot)
	if err := f.native.engine.Park(slot, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hook(t, a, nonce, false); err != nil {
		t.Fatal(err)
	}
	b, err := f.native.engine.Inspect(a)
	if err != nil || b.SessionID != a.SessionID {
		t.Fatal("official Stop nonce did not associate")
	}
	return a
}
func TestNativeCLIHookAssociationRoutingAndEightBlockBudget(t *testing.T) {
	f := nativeHTTP(t)
	a, nonce := f.bind(t, model.ActorClaude)
	_ = f.native.engine.Park(a.Slot, false)
	impostor := a
	impostor.SessionID = "another-session"
	output, err := f.hook(t, impostor, "no nonce", false)
	if err != nil || strings.TrimSpace(string(output)) != "{}" {
		t.Fatalf("unrelated session should ignore hook: %s %v", output, err)
	}
	if b, _ := f.native.engine.Inspect(a); b.SessionID != "" {
		t.Fatal("competing hook stole pending association")
	}
	if _, err := f.hook(t, a, nonce, false); err != nil {
		t.Fatal(err)
	}
	b := associateCLI(t, f, model.ActorCodex)
	// Native input is mocked; every CLI call below traverses real authenticated
	// HTTP, durable Room events and the same Stop continuation serializer.
	if _, err := f.hook(t, a, "@codex discuss this whole reply", false); err != nil {
		t.Fatal(err)
	}
	if err := f.native.engine.Park(b.Slot, true); err != nil {
		t.Fatal(err)
	}
	output, err = f.hook(t, b, "@claude contribution before collection", false)
	if err != nil {
		t.Fatal(err)
	}
	var block map[string]string
	if json.Unmarshal(output, &block) != nil || block["decision"] != "block" || !strings.Contains(block["reason"], "discuss this whole reply") {
		t.Fatalf("Stop did not wake with envelope: %s", output)
	}
	for i := 1; i < 8; i++ {
		if _, err := f.native.engine.Send(a, relay.SendRequest{ID: fmt.Sprintf("budget-%d", i), Text: "next peer input"}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.hook(t, b, "completed current contribution", true); err != nil {
			t.Fatal(err)
		}
	}
	ninth, err := f.native.engine.Send(a, relay.SendRequest{ID: "budget-nine", Text: "must remain queued"})
	if err != nil {
		t.Fatal(err)
	}
	output, err = f.hook(t, b, "eighth continuation completed", true)
	if err != nil || strings.TrimSpace(string(output)) != "{}" {
		t.Fatalf("budget exhausted should not block again: %s %v", output, err)
	}
	state := f.native.engine.Snapshot()
	found := false
	for _, m := range state.Messages {
		if m.ID == ninth.ID {
			found = m.State == "queued"
		}
	}
	if !found {
		t.Fatal("budget exhaustion consumed inbox")
	}
	before := len(state.Messages)
	if _, err := f.run(t, []string{"hook", "--runtime", "codex"}, map[string]any{"hook_event_name": "StopFailure", "session_id": b.SessionID, "cwd": f.project.Root, "error": "API key must never enter log", "last_assistant_message": "@claude partial content"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(f.room.DataDir, "events.jsonl"))
	if len(f.native.engine.Snapshot().Messages) != before || strings.Contains(string(data), "API key must") || strings.Contains(string(data), "partial content") {
		t.Fatal("StopFailure relayed content or secret error")
	}
	peer, err := f.run(t, []string{"peer", "--room", f.room.ID, "--slot", "claude"}, nil)
	if err != nil || !strings.Contains(string(peer), b.SessionID) {
		t.Fatal("peer metadata unavailable")
	}
	if strings.Contains(string(peer), b.Secret) {
		t.Fatal("peer reveals credential")
	}
}
func TestNativeHTTPAuthorizationAndPublicSurface(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorClaude)
	_ = associateCLI(t, f, model.ActorCodex)
	call := func(path, body string, auth relay.Auth, bearer bool) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if bearer {
			req.Header.Set("Authorization", "Bearer management-secret")
		} else {
			req.Header.Set("Authorization", "Relay "+auth.Secret)
			req.Header.Set("X-PairRoom-Bind", auth.BindID)
			req.Header.Set("X-PairRoom-Generation", fmt.Sprint(auth.Generation))
			req.Header.Set("X-PairRoom-Session", auth.SessionID)
		}
		res, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var data bytes.Buffer
		_, _ = data.ReadFrom(res.Body)
		return res.StatusCode, data.Bytes()
	}
	path := "/api/v1/relay/" + f.room.ID + "/claude/send"
	for _, test := range []struct {
		a      relay.Auth
		bearer bool
		path   string
	}{
		{a, true, path}, {relay.Auth{Slot: a.Slot, BindID: a.BindID, Generation: a.Generation, SessionID: "wrong", Secret: a.Secret}, false, path},
		{a, false, "/api/v1/relay/" + f.room.ID + "/codex/send"}, {a, false, path + "?token=bad"},
	} {
		if code, body := call(test.path, `{"id":"denied","text":"must not publish"}`, test.a, test.bearer); code != 401 {
			t.Fatalf("unauthorized code=%d body=%s", code, body)
		}
	}
	body, _ := json.Marshal(map[string]any{"id": "large-valid", "text": strings.Repeat("x", 100<<10)})
	if code, data := call(path, string(body), a, false); code != 200 {
		t.Fatalf("valid response above old Management 64k limit rejected: %d %s", code, data)
	}
	base := "/api/v1/rooms/" + f.room.ID + "/surface"
	for _, action := range []string{"interrupt", "stop", "restart"} {
		if code, data := call(base+"/api/v1/participants/claude/"+action, `{}`, a, true); code != 404 {
			t.Fatalf("native process control offered %s: %d %s", action, code, data)
		}
	}
	if code, data := call(base+"/api/v1/participants/claude/park", `{"enabled":false}`, a, true); code != 200 {
		t.Fatalf("park gateway failed: %d %s", code, data)
	}
	for _, path := range []string{"/", "/app.js", "/styles.css", "/_pairroom/i18n.js", "/api/v1/snapshot"} {
		req, _ := http.NewRequest(http.MethodGet, f.server.URL+base+path, nil)
		req.Header.Set("Authorization", "Bearer management-secret")
		res, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var content bytes.Buffer
		_, _ = content.ReadFrom(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("asset/API %s=%d %s", path, res.StatusCode, content.String())
		}
		if strings.Contains(content.String(), a.Secret) || strings.Contains(content.String(), relay.Digest(a.Secret)) {
			t.Fatal("public surface leaks secret")
		}
	}
}
func TestNativeSchemaCompatibilityAndCheckpointShape(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	old, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "legacy era", Bindings: specs(BindingNew, BindingNew, "")}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	// Convert only this test fixture into the exact prior era: 10/prov3, without
	// host_mode anywhere. Production has no rewriting/migration operation.
	path := filepath.Join(old.DataDir, "events.jsonl")
	data, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	var ev model.Event
	_ = json.Unmarshal(lines[1], &ev)
	var payload map[string]any
	_ = json.Unmarshal(ev.Data, &payload)
	payload["schema"] = 3
	delete(payload, "host_mode")
	ev.Data, _ = json.Marshal(payload)
	lines[1], _ = json.Marshal(ev)
	data = append(bytes.Join(lines, []byte("\n")), '\n')
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(old.DataDir, "metadata.json")
	metadata := []byte("{\"format\":\"pairroom-jsonl\",\"schema_version\":10,\"app_version\":\"4.0.0\"}\n")
	if err := os.WriteFile(metaPath, metadata, 0600); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	recovered, ok := reopened.Room(old.ID)
	if !ok || recovered.HostMode != model.HostEmbedded {
		t.Fatal("prov3 not interpreted as embedded")
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, data) {
		t.Fatal("reading old era rewrote event bytes")
	}
	log, err := store.OpenExistingForRoom(old.DataDir, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	event, _ := model.NewEvent(old.ID, "room.diagnostic", model.ActorSystem, map[string]bool{"test": true})
	if err := log.Append(&event); err != nil {
		t.Fatal(err)
	}
	_ = log.Close()
	after, _ := os.ReadFile(path)
	if !bytes.HasPrefix(after, data) {
		t.Fatal("old era append rewrote prefix")
	}
	if after, _ := os.ReadFile(metaPath); !bytes.Equal(after, metadata) {
		t.Fatal("old era metadata relabeled")
	}
	for _, mode := range []model.HostMode{model.HostEmbedded, model.HostNative} {
		created, err := reopened.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: string(mode), HostMode: mode, Bindings: specs(BindingNew, BindingNew, "")}, SyntheticProvisioner{})
		if err != nil {
			t.Fatal(err)
		}
		schema, err := readRoomStoreSchema(created.DataDir)
		if err != nil || schema != 11 {
			t.Fatal("new Room not schema11")
		}
		facts, _, found, err := reopened.readRoomFacts(context.Background(), created.DataDir)
		if err != nil || !found || facts.HostMode != mode {
			t.Fatal("host_mode not recovered")
		}
	}
	// The strict old checkpoint reader accepts the unchanged shape. Check the
	// nested room objects with the new field explicitly excluded from the schema.
	checkpoint, err := os.ReadFile(filepath.Join(reopened.Root(), "service-registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(checkpoint), "host_mode") {
		t.Fatal("host_mode contaminated schema2 checkpoint")
	}
	var raw struct {
		Schema int               `json:"schema"`
		Rooms  []json.RawMessage `json:"rooms"`
	}
	if json.Unmarshal(checkpoint, &raw) != nil || raw.Schema != 2 {
		t.Fatal("checkpoint schema changed")
	}
	for _, entry := range raw.Rooms {
		var oldShape map[string]any
		_ = json.Unmarshal(entry, &oldShape)
		for key := range oldShape {
			if key == "host_mode" {
				t.Fatal("unknown old reader field")
			}
		}
	}
	// Downgrade only metadata of a native fixture: the new reader must reject
	// mismatched data rather than accepting native facts in an old era.
	for _, room := range reopened.Snapshot(true).Rooms {
		if room.HostMode == model.HostNative {
			if err := os.WriteFile(filepath.Join(room.DataDir, "metadata.json"), metadata, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := reopened.readRoomFacts(context.Background(), room.DataDir); err == nil {
				t.Fatal("schema10/prov4 native accepted")
			}
			break
		}
	}
}
func TestNativeGlobalSessionOwnershipIncludingEmbeddedAndArchive(t *testing.T) {
	f := nativeHTTP(t)
	a := associateCLI(t, f, model.ActorClaude)
	// Same runtime in a different stable slot still conflicts globally.
	pair := defaultAgentSelections()
	pair[model.ActorCodex] = pair[model.ActorClaude]
	other, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: f.project.ID, Name: "other native", HostMode: model.HostNative, Agents: pair}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := f.manager.Activate(context.Background(), other.ID)
	if err != nil {
		t.Fatal(err)
	}
	native := rt.(*nativeHostRuntime)
	b, err := native.engine.Bind(model.ActorCodex, relay.BindRequest{BindID: "other-bind", CredentialHash: relay.Digest("secret"), NonceHash: relay.Digest("nonce")})
	if err != nil {
		t.Fatal(err)
	}
	otherAuth := relay.Auth{Slot: model.ActorCodex, BindID: b.BindID, Generation: b.Generation, Secret: "secret", SessionID: a.SessionID}
	if _, err := native.engine.Associate(otherAuth, "nonce", a.SessionID, ""); !errors.Is(err, ErrBindingOwned) {
		t.Fatalf("duplicate runtime identity accepted: %v", err)
	}
	embedded, err := f.registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: f.project.ID, Name: "embedded", Bindings: specs(BindingNew, BindingNew, "")}, deferredNewProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if _, err := f.registry.MaterializeBinding(context.Background(), embedded.ID, model.ActorClaude, a.SessionID, func(string, any) error { called = true; return nil }); !errors.Is(err, ErrBindingOwned) || called {
		t.Fatal("embedded materialization ignored native owner")
	}
	if _, err := f.registry.MaterializeBinding(context.Background(), f.room.ID, model.ActorClaude, "bypass", func(string, any) error { t.Fatal("native bypass wrote"); return nil }); err == nil {
		t.Fatal("native association bypassed hooks")
	}
	if err := f.manager.Suspend(context.Background(), f.room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.ArchiveRoom(context.Background(), f.room.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := native.engine.Associate(otherAuth, "nonce", a.SessionID, ""); !errors.Is(err, ErrBindingOwned) {
		t.Fatal("archive released binding ownership")
	}
}

func TestNativeCloseCancelsSSEWithoutShutdownTimeout(t *testing.T) {
	f := nativeHTTP(t)
	req, _ := http.NewRequest(http.MethodGet, f.native.baseURL+"/api/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+f.native.token)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := f.native.Close(ctx); err != nil {
		t.Fatalf("SSE prevented native shutdown: %v", err)
	}
}

func TestNativeArchiveMissingDataFailsClosed(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "native missing data", HostMode: model.HostNative}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(room.DataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ArchiveRoom(context.Background(), room.ID); err == nil {
		t.Fatal("native archive used checkpoint-only fallback")
	}
	if after, _ := registry.Room(room.ID); after.Archived() {
		t.Fatal("failed native archive changed lifecycle")
	}
}
