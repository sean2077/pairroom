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

func TestNativePendingBindCannotRediscloseNonce(t *testing.T) {
	f := nativeHTTP(t)
	a, nonce := f.bind(t, model.ActorClaude)
	dir := filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", "claude")
	state, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	credentials, _ := os.ReadFile(filepath.Join(dir, "credentials"))
	args := []string{"bind", "--room", f.room.ID, "--slot", "claude", "--service-file", f.endpoint}
	for _, extra := range [][]string{nil, {"--continue", "--session-id", a.SessionID}} {
		out, err := f.run(t, append(append([]string{}, args...), extra...), nil)
		if err == nil || !strings.Contains(err.Error(), "pending binding") || len(out) != 0 {
			t.Fatalf("pending bind must fail without output: %q %v", out, err)
		}
	}
	for name, before := range map[string][]byte{"state.json": state, "credentials": credentials} {
		after, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || !bytes.Equal(before, after) {
			t.Fatalf("rejected bind changed %s: %v", name, err)
		}
	}
	// The original nonce still completes the association, and an associated
	// session can explicitly resume without replacing its generation.
	_ = f.native.engine.Park(a.Slot, false)
	if _, err := f.hook(t, a, nonce, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, append(args, "--continue", "--session-id", a.SessionID), nil); err != nil {
		t.Fatal(err)
	}
	b, err := f.native.engine.Inspect(a)
	if err != nil || b.Generation != a.Generation || b.SessionID != a.SessionID {
		t.Fatalf("original binding did not survive: %+v %v", b, err)
	}
}

func TestNativePendingBindRecoveryRequiresReplace(t *testing.T) {
	f := nativeHTTP(t)
	a, firstNonce := f.bind(t, model.ActorClaude)
	out, err := f.run(t, []string{"bind", "--room", f.room.ID, "--slot", "claude", "--service-file", f.endpoint, "--replace"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var replacement struct {
		Binding relay.Binding `json:"binding"`
		Nonce   string        `json:"bind_nonce"`
	}
	if err := json.Unmarshal(out, &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Nonce == "" || replacement.Nonce == firstNonce || replacement.Binding.Generation != a.Generation+1 {
		t.Fatalf("replacement did not rotate identity: %+v", replacement.Binding)
	}
	if _, err := f.native.engine.Inspect(a); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("old pending credentials remain usable: %v", err)
	}
	_ = f.native.engine.Park(a.Slot, false)
	if out, err := f.hook(t, a, firstNonce, false); err != nil || strings.TrimSpace(string(out)) != "{}" {
		t.Fatalf("revoked nonce was not ignored: %q %v", out, err)
	}
	if _, err := f.hook(t, a, replacement.Nonce, false); err != nil {
		t.Fatal(err)
	}
	if got := f.native.engine.Snapshot().Bindings[a.Slot]; got.SessionID != a.SessionID {
		t.Fatal("replacement nonce did not associate")
	}
}

func TestNativeHTTPAckSettlesDrainingRuntime(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			f := nativeHTTP(t)
			a := associateCLI(t, f, model.ActorClaude)
			m, err := f.native.engine.SendUser(relay.SendRequest{ID: "before-drain", To: a.Slot, Text: "accepted work"})
			if err != nil {
				t.Fatal(err)
			}
			claim, err := f.native.engine.Claim(context.Background(), a, false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if shutdown {
					done <- f.manager.Shutdown(ctx)
				} else {
					done <- f.manager.WaitAndSuspend(ctx, f.room.ID)
				}
			}()
			for {
				f.manager.mu.Lock()
				draining := f.manager.entries[f.room.ID].drainRequested
				changed := f.manager.changed
				f.manager.mu.Unlock()
				if draining {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("drain did not start")
				case <-changed:
				}
			}
			if _, err := f.native.engine.SendUser(relay.SendRequest{ID: "after-drain", To: a.Slot, Text: "must not start"}); !errors.Is(err, relay.ErrClosed) {
				t.Fatalf("drain admitted new work: %v", err)
			}
			body, _ := json.Marshal(map[string]string{"id": claim.ID, "receipt": claim.Receipt})
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/api/v1/relay/"+f.room.ID+"/claude/ack", bytes.NewReader(body))
			req.Header.Set("Authorization", "Relay "+a.Secret)
			req.Header.Set("X-PairRoom-Bind", a.BindID)
			req.Header.Set("X-PairRoom-Generation", fmt.Sprint(a.Generation))
			req.Header.Set("X-PairRoom-Session", a.SessionID)
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("draining ack status=%d", res.StatusCode)
			}
			if err := <-done; err != nil {
				t.Fatalf("drain failed to settle before lease expiry: %v", err)
			}
			if got := f.native.engine.Snapshot().Messages[0]; got.ID != m.ID || got.State != "handed_off" {
				t.Fatalf("drain manufactured uncertainty: %+v", got)
			}
			if _, err := f.manager.runtimeForCompletion(f.room.ID); !errors.Is(err, ErrRuntimeNotReady) {
				t.Fatalf("completion reopened suspended runtime: %v", err)
			}
		})
	}
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
