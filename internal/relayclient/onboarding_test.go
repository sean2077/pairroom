package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
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
		serviceRoom{ID: "embedded1", ProjectID: "p1", HostMode: model.HostEmbedded},
		serviceRoom{ID: "native1", ProjectID: "p1", HostMode: model.HostNative},
		serviceRoom{ID: "otherws", ProjectID: "px", HostMode: model.HostNative},
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
		serviceRoom{ID: "roomB", ProjectID: "p1", HostMode: model.HostNative},
		serviceRoom{ID: "roomA", ProjectID: "p1", HostMode: model.HostNative},
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
			"rooms":    []any{map[string]any{"id": "roomJ", "project_id": "p1", "host_mode": "native", "agents": map[string]any{"claude": map[string]string{"runtime": "claude"}, "codex": map[string]string{"runtime": "codex"}}}},
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
		t.Fatal("skills/pairroom-relay/SKILL.md drifted from the embedded projection; edit assets/ then copy it over internal/relayclient/skill/pairroom-relay/SKILL.md")
	}
	if !strings.Contains(skillContent, "name: pairroom-relay") || !strings.Contains(skillContent, "bind --create --name") {
		t.Fatal("skill lost its identity or create flow")
	}
}
