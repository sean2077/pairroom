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
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestNativeDynamicCreationDoesNotReadEmbeddedProfiles(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	registry, project := testRegistry(t, testGitRepo(t))
	profilePath := filepath.Join(registry.Root(), agentPairProfilesFile)
	damaged := []byte(`{"schema":2,"profiles":`)
	if err := os.WriteFile(profilePath, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.AgentPairProfiles(); err == nil {
		t.Fatal("fixture must make Embedded profile resolution fail")
	}
	provisioner := &recordingProvisioner{}
	server, _ := newManagementTestServer(t, registry, provisioner)
	path := "/api/v1/projects/" + project.ID + "/rooms"
	for _, body := range []string{
		`{"name":"obsolete selector","agents":{}}`,
		`{"name":"obsolete selector","agents":null}`,
		`{"name":"obsolete selector","agent_pair_profile_id":"old-profile"}`,
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, managementRequest(http.MethodPost, path, body, true))
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "omit agents and agent_pair_profile_id") {
			t.Fatalf("Native selector was not rejected clearly: %d %s", response.Code, response.Body.String())
		}
		if len(registry.Snapshot(true).Rooms) != 0 || len(provisioner.calls) != 0 {
			t.Fatal("rejected selection created Room facts or started a Runtime")
		}
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, managementRequest(http.MethodPost, path, `{"name":"untyped Native"}`, true))
	if response.Code != http.StatusCreated {
		t.Fatalf("Native creation depended on Embedded profile availability: %d %s", response.Code, response.Body.String())
	}
	var room Room
	if err := json.Unmarshal(response.Body.Bytes(), &room); err != nil {
		t.Fatal(err)
	}
	if room.HostMode != model.HostNative || !room.DynamicBindings || len(room.Agents) != 2 {
		t.Fatalf("new Room did not use dynamic Native: %+v", room)
	}
	for _, slot := range model.SlotActors() {
		if room.Agents[slot] != (model.AgentSelection{AwaitingBinding: true}) || !room.Bindings[slot].Pending || room.Bindings[slot].Runtime != "" || room.RuntimeNames[slot] != "" {
			t.Fatalf("vacant slot acquired a native identity: %s %+v", slot, room)
		}
	}
	if len(provisioner.calls) != 0 {
		t.Fatal("Native creation spawned an Embedded Runtime")
	}
	if got, err := os.ReadFile(profilePath); err != nil || !bytes.Equal(got, damaged) {
		t.Fatal("Native creation repaired or overwrote an unrelated profile file")
	}
	events, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var provision roomProvisionedPayload
	for _, event := range events {
		if event.Kind == EventRoomProvisioned {
			if err := json.Unmarshal(event.Data, &provision); err != nil {
				t.Fatal(err)
			}
		}
	}
	if provision.Schema != 7 || !provision.DynamicBindings {
		t.Fatalf("creation did not durably select the v9 format: %+v", provision)
	}
}

func TestNativeDynamicScopedAutobindIsAtomicThroughManagement(t *testing.T) {
	f := nativeHTTP(t)
	type result struct {
		request relay.BindRequest
		code    int
		body    []byte
	}
	call := func(request relay.BindRequest, path string) result {
		data, err := json.Marshal(request)
		if err != nil {
			panic(err)
		}
		req := managementRequest(http.MethodPost, path, string(data), false)
		req.Header.Set("Authorization", "Bearer "+f.management.cliToken)
		response := httptest.NewRecorder()
		f.management.Handler().ServeHTTP(response, req)
		return result{request, response.Code, append([]byte(nil), response.Body.Bytes()...)}
	}
	path := "/api/v1/rooms/" + f.room.ID + "/native-bindings"
	start := make(chan struct{})
	results := make(chan result, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- call(relay.BindRequest{Runtime: model.RuntimeCodex, BindID: fmt.Sprintf("automatic-%d", i), SessionID: fmt.Sprintf("actual-session-%d", i), CredentialHash: relay.Digest(fmt.Sprintf("private-secret-%d", i))}, path)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	seen := map[model.ActorID]bool{}
	refused := 0
	for result := range results {
		if result.code == http.StatusConflict {
			refused++
			continue
		}
		if result.code != http.StatusOK {
			t.Fatalf("scoped automatic bind failed: %d %s", result.code, result.body)
		}
		var bound struct {
			Binding        relay.Binding `json:"binding"`
			Dynamic        bool          `json:"dynamic_bindings"`
			PeerGeneration uint64        `json:"peer_generation"`
			Bootstrap      string        `json:"bootstrap"`
		}
		if err := json.Unmarshal(result.body, &bound); err != nil {
			t.Fatal(err)
		}
		if !bound.Dynamic || bound.PeerGeneration != 1 || !bound.Binding.Slot.ValidParticipant() || seen[bound.Binding.Slot] || bound.Binding.Runtime != model.RuntimeCodex || bound.Binding.Generation != 1 || !strings.Contains(bound.Bootstrap, "@agent") {
			t.Fatalf("concurrent binding returned an incoherent slot/context: %s", result.body)
		}
		seen[bound.Binding.Slot] = true
		again := call(result.request, path)
		var recovered struct {
			Binding relay.Binding `json:"binding"`
		}
		if err := json.Unmarshal(again.body, &recovered); err != nil || again.code != http.StatusOK || recovered.Binding.BindID != bound.Binding.BindID || recovered.Binding.Slot != bound.Binding.Slot || recovered.Binding.Generation != 1 {
			t.Fatalf("retry did not recover its original binding in a full Room: %d %s", again.code, again.body)
		}
	}
	if len(seen) != 2 || refused != 1 {
		t.Fatalf("three competing sessions did not acquire exactly two slots: %v, refused=%d", seen, refused)
	}
	if got := f.native.engine.Snapshot().Protocol; got != protocol.NativeDynamicVersion {
		t.Fatalf("automatic binding selected protocol %s", got)
	}
}

func TestNativeDynamicRuntimeIdentitySurvivesReplacementAndCheckpoint(t *testing.T) {
	f := nativeHTTP(t)
	ctx := context.Background()
	request := relay.BindRequest{Runtime: model.RuntimeClaude, BindID: "original-claude", SessionID: "same-vendor-session-text", CredentialHash: relay.Digest("original-secret")}
	if _, err := f.native.engine.Bind(model.ActorSlot1, request); err != nil {
		t.Fatal(err)
	}
	original := f.registry.nativeClaims[nativeClaimKey(f.room.ID, model.ActorSlot1)]
	other, err := f.registry.ProvisionRoom(ctx, ProvisionRequest{ProjectID: f.project.ID, Name: "Different Runtime same session text"}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	runtime, _, err := f.manager.Activate(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherNative := runtime.(*nativeHostRuntime)
	request.Runtime, request.BindID = model.RuntimeCodex, "independent-codex"
	if _, err := otherNative.engine.Bind(model.ActorSlot1, request); err != nil {
		t.Fatalf("different Runtime incorrectly shared session ownership: %v", err)
	}
	codex := f.registry.nativeClaims[nativeClaimKey(other.ID, model.ActorSlot1)]
	duplicate := request
	duplicate.BindID = "duplicate-codex"
	if _, err := f.native.engine.Bind(model.ActorSlot2, duplicate); !errors.Is(err, ErrBindingOwned) {
		t.Fatalf("same actual Runtime/session was allowed in another Room: %v", err)
	}
	request.Runtime, request.BindID, request.Replace = model.RuntimeGemini, "replacement-gemini", true
	if binding, err := f.native.engine.Bind(model.ActorSlot1, request); err != nil || binding.Generation != 2 || binding.Runtime != model.RuntimeGemini {
		t.Fatalf("cross-Runtime replacement failed: %+v %v", binding, err)
	}
	gemini := f.registry.nativeClaims[nativeClaimKey(f.room.ID, model.ActorSlot1)]
	// Reproduce a crash after the replacement fact committed but before the
	// previous Runtime/session reservation was released. Runtime must be read
	// from each original fact, not from the latest displayed selection.
	if err := f.registry.identities.Reserve(ctx, original); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{f.room.ID, other.ID} {
		if err := f.manager.Suspend(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	restarted, err := OpenRegistry(ctx, RegistryConfig{Root: f.registry.Root()})
	if err != nil {
		t.Fatalf("checkpoint confused different Runtime identities: %v", err)
	}
	for _, claim := range []nativeidentity.Claim{codex, gemini} {
		if err := restarted.identities.Check(ctx, claim); err != nil {
			t.Fatalf("restart lost a current actual Runtime identity: %+v %v", claim, err)
		}
	}
	if err := restarted.identities.Check(ctx, original); !errors.Is(err, nativeidentity.ErrUnowned) {
		t.Fatalf("old Runtime reservation was relabeled or retained: %v", err)
	}
	for id, want := range map[string]model.RuntimeKind{f.room.ID: model.RuntimeGemini, other.ID: model.RuntimeCodex} {
		room, ok := restarted.Room(id)
		if !ok || !room.DynamicBindings || room.Agents[model.ActorSlot1].Runtime != want || room.Bindings[model.ActorSlot1].Runtime != want {
			t.Fatalf("replayed Room lost actual Runtime: %+v", room)
		}
	}
}

func TestNativeDynamicStartupRejectsInvalidIdentityHistoryBeforeReservations(t *testing.T) {
	for _, mutation := range []string{"session", "credential", "generation-skip", "reactivate"} {
		t.Run(mutation, func(t *testing.T) {
			f := nativeHTTP(t)
			ctx := context.Background()
			if _, err := f.native.engine.Bind(model.ActorSlot1, relay.BindRequest{Runtime: model.RuntimeClaude, BindID: "original", SessionID: "original-session", CredentialHash: relay.Digest("original-secret")}); err != nil {
				t.Fatal(err)
			}
			original := f.registry.nativeClaims[nativeClaimKey(f.room.ID, model.ActorSlot1)]
			eventPath := filepath.Join(f.room.DataDir, "events.jsonl")
			events, err := readEventsReadOnly(eventPath)
			if err != nil {
				t.Fatal(err)
			}
			forged := events[len(events)-1]
			if forged.Kind != relay.EventBinding {
				t.Fatal("fixture lacks its original binding fact")
			}
			var binding map[string]any
			if err := json.Unmarshal(forged.Data, &binding); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "session":
				binding["session_id"] = "unproven-replacement-session"
			case "credential":
				binding["credential_hash"] = relay.Digest("unproven-replacement-secret")
			case "generation-skip":
				binding["generation"], binding["bind_id"] = 3, "skipped-generation"
			case "reactivate":
				if err := f.native.engine.Unbind(model.ActorSlot1); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.manager.Suspend(ctx, f.room.ID); err != nil {
				t.Fatal(err)
			}
			events, err = readEventsReadOnly(eventPath)
			if err != nil {
				t.Fatal(err)
			}
			forged.Seq = events[len(events)-1].Seq + 1
			forged.ID = "forged-identity-" + mutation
			forged.Data = mustJSON(t, binding)
			file, err := os.OpenFile(eventPath, os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write(append(mustJSON(t, forged), '\n')); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(eventPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRegistry(ctx, RegistryConfig{Root: f.registry.Root()}); err == nil || !strings.Contains(err.Error(), "invalid Native history") {
				t.Fatalf("invalid binding history escaped startup validation: %v", err)
			}
			if got, err := os.ReadFile(eventPath); err != nil || !bytes.Equal(got, before) {
				t.Fatal("invalid binding history was rewritten during startup")
			}
			err = f.registry.identities.Check(ctx, original)
			if mutation == "reactivate" {
				if !errors.Is(err, nativeidentity.ErrUnowned) {
					t.Fatalf("invalid reactivation reacquired a retired session: %v", err)
				}
			} else if err != nil {
				t.Fatalf("invalid replacement released the original identity: %v", err)
			}
		})
	}
}

func TestNativeDynamicFormatMismatchFailsBeforeRootCleanup(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	for _, test := range []struct {
		store   int
		payload string
	}{
		{12, `{"schema":5,"host_mode":"native","agents":{"slot1":{"awaiting_binding":true}}}`},
		{13, `{"schema":6,"host_mode":"native","agents":{"slot1":{"awaiting_binding":true}}}`},
		{13, `{"schema":6,"host_mode":"native","dynamic_bindings":true}`},
		{14, `{"schema":7,"host_mode":"native"}`},
		{14, `{"schema":7,"host_mode":"embedded","dynamic_bindings":true}`},
		{14, `{"schema":6,"host_mode":"native"}`},
	} {
		t.Run(fmt.Sprint(test.store)+test.payload, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "rooms", ".provision-incompatible")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			metadata := mustJSON(t, map[string]any{"format": "pairroom-jsonl", "schema_version": test.store})
			events := []byte("{\"kind\":\"room.created\"}\n{\"kind\":\"service.room.provisioned\",\"data\":" + test.payload + "}\n")
			files := map[string][]byte{
				filepath.Join(dir, "metadata.json"):                 metadata,
				filepath.Join(dir, "events.jsonl"):                  events,
				filepath.Join(root, ".service-registry-prefix.tmp"): []byte("preserve before format validation"),
			}
			for path, data := range files {
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: root}); err == nil || !strings.Contains(err.Error(), "data was not modified") {
				t.Fatalf("invalid format did not fail before cleanup: %v", err)
			}
			for path, want := range files {
				if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("startup modified an unsupported format or staging prefix: %s %v", filepath.Base(path), err)
				}
			}
		})
	}
}

func TestNativeDynamicArchivedCheckpointRejectsRuntimeIdentityAliases(t *testing.T) {
	f := nativeHTTP(t)
	ctx := context.Background()
	if _, err := f.native.engine.Bind(model.ActorSlot1, relay.BindRequest{Runtime: model.RuntimeClaude, BindID: "archived-binding", SessionID: "archived-native-session", CredentialHash: relay.Digest("archived-secret")}); err != nil {
		t.Fatal(err)
	}
	claim := f.registry.nativeClaims[nativeClaimKey(f.room.ID, model.ActorSlot1)]
	if err := f.manager.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	room, err := f.registry.ArchiveRoom(ctx, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Archived Rooms may legitimately retain only their checkpoint after a
	// lost directory. That checkpoint must not create a new identity namespace
	// by accepting an alias for the canonical Runtime originally bound.
	if err := os.RemoveAll(room.DataDir); err != nil {
		t.Fatal(err)
	}
	snapshot := f.registry.Snapshot(true)
	for i := range snapshot.Rooms {
		if snapshot.Rooms[i].ID != room.ID {
			continue
		}
		binding := snapshot.Rooms[i].Bindings[model.ActorSlot1]
		binding.Runtime = "claude-code"
		snapshot.Rooms[i].Bindings[model.ActorSlot1] = binding
		selection := snapshot.Rooms[i].Agents[model.ActorSlot1]
		selection.Runtime = "claude-code"
		snapshot.Rooms[i].Agents[model.ActorSlot1] = selection
	}
	before := mustJSON(t, snapshot)
	if err := os.WriteFile(f.registry.checkpoint, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(ctx, RegistryConfig{Root: f.registry.Root()}); err == nil || !strings.Contains(err.Error(), "canonical runtime identity") {
		t.Fatalf("checkpoint alias bypassed canonical Native identity: %v", err)
	}
	if err := f.registry.identities.Check(ctx, claim); err != nil {
		t.Fatal("invalid checkpoint changed the original Native session reservation")
	}
	if got, err := os.ReadFile(f.registry.checkpoint); err != nil || !bytes.Equal(got, before) {
		t.Fatal("invalid archived checkpoint was rewritten")
	}
}
