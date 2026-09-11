package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestNormalizeSlot(t *testing.T) {
	cases := map[string]string{
		"1": "claude", "agent1": "claude", "AGENT1": "claude", " claude ": "claude",
		"2": "codex", "agent2": "codex", "codex": "codex",
		"3": "3", "": "", "grok": "grok",
	}
	for in, want := range cases {
		if got := normalizeSlot(in); got != want {
			t.Errorf("normalizeSlot(%q) = %q, want %q", in, got, want)
		}
	}
}

func testSnapshot(rooms ...serviceRoom) serviceSnapshot {
	return serviceSnapshot{
		Projects: []serviceProject{{ID: "p1", Root: "/ws"}},
		Rooms:    rooms,
	}
}

func TestResolveNativeRoomUnique(t *testing.T) {
	snap := testSnapshot(
		serviceRoom{ID: "embedded1", ProjectID: "p1", HostMode: model.HostEmbedded, Lifecycle: "active"},
		serviceRoom{ID: "native1", ProjectID: "p1", HostMode: model.HostNative, Lifecycle: "active"},
		serviceRoom{ID: "otherws", ProjectID: "px", HostMode: model.HostNative, Lifecycle: "active"},
	)
	id, err := resolveNativeRoom(snap, "/ws")
	if err != nil || id != "native1" {
		t.Fatalf("id = %q, err = %v", id, err)
	}
}

func TestResolveNativeRoomNoneGuidesCreate(t *testing.T) {
	_, err := resolveNativeRoom(testSnapshot(), "/ws")
	if err == nil || !strings.Contains(err.Error(), "bind --create") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveNativeRoomMultipleListsCandidates(t *testing.T) {
	snap := testSnapshot(
		serviceRoom{ID: "roomB", ProjectID: "p1", HostMode: model.HostNative, Lifecycle: "active"},
		serviceRoom{ID: "roomA", ProjectID: "p1", HostMode: model.HostNative, Lifecycle: "active"},
	)
	_, err := resolveNativeRoom(snap, "/ws")
	if err == nil || !strings.Contains(err.Error(), "roomA, roomB") {
		t.Fatalf("err = %v", err)
	}
}

func dualRuntimeRoom() serviceRoom {
	return serviceRoom{ID: "r1", ProjectID: "p1", HostMode: model.HostNative, Agents: map[model.ActorID]model.AgentSelection{
		model.ActorClaude: {Runtime: model.RuntimeClaude},
		model.ActorCodex:  {Runtime: model.RuntimeCodex},
	}}
}

func TestResolveSlotForRoomUniqueRuntime(t *testing.T) {
	slot, err := resolveSlotForRoom(dualRuntimeRoom(), model.RuntimeCodex)
	if err != nil || slot != model.ActorCodex {
		t.Fatalf("slot = %q, err = %v", slot, err)
	}
}

func TestResolveSlotForRoomSameRuntimeFailsClosed(t *testing.T) {
	room := serviceRoom{ID: "r1", HostMode: model.HostNative, Agents: map[model.ActorID]model.AgentSelection{
		model.ActorClaude: {Runtime: model.RuntimeClaude},
		model.ActorCodex:  {Runtime: model.RuntimeClaude},
	}}
	_, err := resolveSlotForRoom(room, model.RuntimeClaude)
	if err == nil || !strings.Contains(err.Error(), "--slot 1") || !strings.Contains(err.Error(), "--slot 2") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolveSlotForRoomUnrecognizedCallerFailsClosed(t *testing.T) {
	_, err := resolveSlotForRoom(dualRuntimeRoom(), "")
	if err == nil || !strings.Contains(err.Error(), "--slot 1|2") {
		t.Fatalf("err = %v", err)
	}
}

func TestInferCreateSlot(t *testing.T) {
	stubLineage(t, 4242, "claude", true)
	slot, err := inferCreateSlot(options{})
	if err != nil || slot != model.ActorClaude {
		t.Fatalf("slot = %q, err = %v", slot, err)
	}
	slot, err = inferCreateSlot(options{kind: "codex"})
	if err != nil || slot != model.ActorCodex {
		t.Fatalf("explicit runtime slot = %q, err = %v", slot, err)
	}
	stubLineage(t, 0, "", false)
	if _, err := inferCreateSlot(options{}); err == nil {
		t.Fatal("unrecognized caller must not guess a create slot")
	}
}

func TestBindZeroFlagResolvesRoomAndSlot(t *testing.T) {
	root := t.TempDir()
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"projects": []any{map[string]string{"id": "p1", "root": root}},
			"rooms":    []any{map[string]any{"id": "roomJ", "project_id": "p1", "host_mode": "native", "lifecycle": "active", "agents": map[string]any{"claude": map[string]string{"runtime": "claude"}, "codex": map[string]string{"runtime": "codex"}}}},
		})
	})
	mux.HandleFunc("POST /api/v1/rooms/roomJ/native-bindings/claude", func(w http.ResponseWriter, r *http.Request) {
		var request relay.BindRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": relay.Binding{BindID: request.BindID, Generation: 1, Slot: model.ActorClaude, Active: true}, "bootstrap": "b", "collaboration": "c"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	endpointPath := filepath.Join(t.TempDir(), "relay-endpoint.json")
	if err := relay.AtomicJSON(endpointPath, relay.Endpoint{URL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	stubLineage(t, 4242, "claude", true)
	var out bytes.Buffer
	if err := bind(context.Background(), root, options{endpoint: endpointPath}, &out); err != nil {
		t.Fatalf("zero-flag bind: %v", err)
	}
	statePath := filepath.Join(root, ".pairroom", "rooms", "roomJ", "slots", "claude", "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("zero-flag bind must materialize the inferred slot state: %v", err)
	}
	if !strings.Contains(string(data), `"harness_pid": 4242`) {
		t.Fatalf("lineage was not recorded: %s", data)
	}
}

func TestPublishedSkillMatchesEmbeddedProjection(t *testing.T) {
	published, err := os.ReadFile(filepath.Join("..", "..", "skills", "pairroom-relay", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(published) != skillContent {
		t.Fatal("skills/pairroom-relay/SKILL.md drifted from the embedded projection; edit the published skills/pairroom-relay/SKILL.md, then copy it over internal/relayclient/skill/pairroom-relay/SKILL.md")
	}
	if !strings.Contains(skillContent, "name: pairroom-relay") || !strings.Contains(skillContent, "bind --create --name") {
		t.Fatal("skill lost its identity or create flow")
	}
}

func TestResolveNativeRoomIgnoresArchived(t *testing.T) {
	snap := testSnapshot(
		serviceRoom{ID: "archived1", ProjectID: "p1", HostMode: model.HostNative, Lifecycle: "archived"},
		serviceRoom{ID: "active1", ProjectID: "p1", HostMode: model.HostNative, Lifecycle: "active"},
	)
	id, err := resolveNativeRoom(snap, "/ws")
	if err != nil || id != "active1" {
		t.Fatalf("an archived Room must not create ambiguity: id = %q, err = %v", id, err)
	}
	archivedOnly := testSnapshot(serviceRoom{ID: "archived1", ProjectID: "p1", HostMode: model.HostNative, Lifecycle: "archived"})
	if _, err := resolveNativeRoom(archivedOnly, "/ws"); err == nil || !strings.Contains(err.Error(), "bind --create") {
		t.Fatalf("an archived-only workspace must guide creation: err = %v", err)
	}
}

// The Service-owned default pair is user configuration and need not match slot
// order, so a creator slot may never be inferred from the harness alone.
func TestBindCreateResolvesSlotFromCreatedRoomSelections(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := editHooks(root, model.RuntimeCodex, false); err != nil {
		t.Fatal(err)
	}
	created := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		rooms := []any{}
		for i := 1; i <= created; i++ {
			rooms = append(rooms, map[string]any{"id": fmt.Sprintf("room%d", i), "project_id": "p1", "host_mode": "native", "lifecycle": "active",
				"agents": map[string]any{"claude": map[string]string{"runtime": "codex"}, "codex": map[string]string{"runtime": "claude"}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"projects": []any{map[string]string{"id": "p1", "root": root}}, "rooms": rooms})
	})
	mux.HandleFunc("POST /api/v1/projects/p1/rooms", func(w http.ResponseWriter, r *http.Request) {
		created++
		_ = json.NewEncoder(w).Encode(map[string]string{"id": fmt.Sprintf("room%d", created)})
	})
	bound := ""
	mux.HandleFunc("POST /api/v1/rooms/room1/native-bindings/{slot}", func(w http.ResponseWriter, r *http.Request) {
		bound = r.PathValue("slot")
		var request relay.BindRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": relay.Binding{BindID: request.BindID, Generation: 1, Slot: model.ActorID(bound), Active: true}, "bootstrap": "b", "collaboration": "c"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	endpointPath := filepath.Join(t.TempDir(), "relay-endpoint.json")
	if err := relay.AtomicJSON(endpointPath, relay.Endpoint{URL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	stubLineage(t, 4242, "codex", true)
	var out bytes.Buffer
	if err := bind(context.Background(), root, options{create: true, endpoint: endpointPath}, &out); err != nil {
		t.Fatalf("create bind against a swapped default pair: %v", err)
	}
	if bound != "claude" {
		t.Fatalf("a codex harness must bind the slot that runs codex, bound slot = %q", bound)
	}
	if _, err := os.Stat(filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "claude", "state.json")); err != nil {
		t.Fatalf("state was not materialized in the runtime-matched slot: %v", err)
	}
	var state State
	if err := readPrivate(filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "claude", "state.json"), &state); err != nil {
		t.Fatal(err)
	}
	if state.Runtime != model.RuntimeCodex {
		t.Fatalf("recorded runtime = %q, want the slot's real selection codex", state.Runtime)
	}
}

func TestBindCreateRejectsMissingCallerHookBeforeCreating(t *testing.T) {
	root, endpoint, created := createBindFixture(t, model.RuntimeCodex)
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	stubLineage(t, 4242, "codex", true)
	var out bytes.Buffer
	err := bind(context.Background(), root, options{create: true, endpoint: endpoint}, &out)
	if err == nil || !strings.Contains(err.Error(), "relay install") {
		t.Fatalf("err = %v", err)
	}
	if *created != 0 || out.Len() != 0 {
		t.Fatalf("a caller harness without an approved hook must not create durable state: created=%d output=%q", *created, out.String())
	}
}

func TestBindCreateUnrecognizedCallerFailsBeforeCreating(t *testing.T) {
	root, endpoint, created := createBindFixture(t, model.RuntimeClaude)
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	stubLineage(t, 0, "", false)
	var out bytes.Buffer
	err := bind(context.Background(), root, options{create: true, endpoint: endpoint}, &out)
	if err == nil || !strings.Contains(err.Error(), "--slot 1|2") {
		t.Fatalf("err = %v", err)
	}
	if *created != 0 || out.Len() != 0 {
		t.Fatalf("an unresolvable creator slot must not create durable state: created=%d output=%q", *created, out.String())
	}
}

func TestInstallSkillUpgradesOwnProjectionAndRefusesUnrelated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := filepath.Join(home, ".claude", "skills", "pairroom-relay", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	previous := "---\nname: pairroom-relay\n---\n\n# PairRoom Native relay\n\nsuperseded body\n"
	if err := os.WriteFile(path, []byte(previous), 0600); err != nil {
		t.Fatal(err)
	}
	if err := installSkill(model.RuntimeClaude); err != nil {
		t.Fatalf("a projection written by an earlier release must stay upgradable: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != skillContent {
		t.Fatal("upgrade did not write the canonical skill")
	}
	if err := os.WriteFile(path, []byte("# an unrelated skill that happens to share the directory name\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := installSkill(model.RuntimeClaude); err == nil || !strings.Contains(err.Error(), "unrelated") {
		t.Fatalf("unrelated content must still be refused: %v", err)
	}
}
