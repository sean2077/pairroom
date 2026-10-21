package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient/skill"
)

func TestNormalizeSlot(t *testing.T) {
	cases := map[string]string{
		"1": "slot1", "agent1": "slot1", "AGENT1": "slot1", " claude ": "slot1",
		"2": "slot2", "agent2": "slot2", "codex": "slot2",
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
		model.ActorSlot1: {Runtime: model.RuntimeClaude},
		model.ActorSlot2: {Runtime: model.RuntimeCodex},
	}}
}

func TestResolveSlotForRoomUniqueRuntime(t *testing.T) {
	slot, err := resolveSlotForRoom(dualRuntimeRoom(), model.RuntimeCodex)
	if err != nil || slot != model.ActorSlot2 {
		t.Fatalf("slot = %q, err = %v", slot, err)
	}
}

func TestResolveSlotForRoomSameRuntimeFailsClosed(t *testing.T) {
	room := serviceRoom{ID: "r1", HostMode: model.HostNative, Agents: map[model.ActorID]model.AgentSelection{
		model.ActorSlot1: {Runtime: model.RuntimeClaude},
		model.ActorSlot2: {Runtime: model.RuntimeClaude},
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
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex, model.RuntimeGrok} {
		t.Run(string(kind), func(t *testing.T) {
			stubLineage(t, 4242, string(kind), true)
			for _, o := range []options{{}, {kind: string(kind)}} {
				slot, err := inferCreateSlot(o)
				if err != nil || slot != model.ActorSlot1 {
					t.Fatalf("creator %s: slot = %q, err = %v", kind, slot, err)
				}
			}
		})
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
			"rooms":    []any{map[string]any{"id": "roomJ", "project_id": "p1", "host_mode": "native", "lifecycle": "active", "agents": map[string]any{"slot1": map[string]string{"runtime": "claude"}, "slot2": map[string]string{"runtime": "codex"}}}},
		})
	})
	mux.HandleFunc("POST /api/v1/rooms/roomJ/native-bindings/slot1", func(w http.ResponseWriter, r *http.Request) {
		var request relay.BindRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": relay.Binding{BindID: request.BindID, Generation: 1, Slot: model.ActorSlot1, Active: true, SessionID: request.SessionID}, "bootstrap": "b", "collaboration": "c"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	endpointPath := filepath.Join(t.TempDir(), "relay-endpoint.json")
	if err := relay.AtomicJSON(endpointPath, relay.Endpoint{URL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	stubLineage(t, 4242, "claude", true)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "official-session")
	var out bytes.Buffer
	if err := bind(context.Background(), root, options{endpoint: endpointPath}, &out); err != nil {
		t.Fatalf("zero-flag bind: %v", err)
	}
	statePath := filepath.Join(root, ".pairroom", "rooms", "roomJ", "slots", "slot1", "state.json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("zero-flag bind must materialize the inferred slot state: %v", err)
	}
	if !strings.Contains(string(data), `"harness_pid": 4242`) {
		t.Fatalf("lineage was not recorded: %s", data)
	}
}

func TestPublishedSkillMatchesEmbeddedProjection(t *testing.T) {
	root := filepath.Join("..", "..", "skills", "pairroom-relay")
	files := map[string]string{}
	for _, file := range skill.Files() {
		files[file.Path] = file.Content
	}
	if files["SKILL.md"] != skillContent {
		t.Fatal("shared skill bundle and relay CLI entrypoint drifted")
	}
	err := fs.WalkDir(os.DirFS(root), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		published, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		embedded, ok := files[path]
		if !ok || string(published) != embedded {
			t.Errorf("skill bundle drift at %s; edit skills/pairroom-relay, then copy the complete directory to internal/relayclient/skill/pairroom-relay", path)
		}
		delete(files, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for path := range files {
		t.Errorf("embedded skill file %s has no canonical source", path)
	}
	assertSkillRules(t, "SKILL.md", map[string]string{
		"skill-identity":               "name: pairroom-relay",
		"agent-tool-entry":             "Use this session's tools",
		"preflight":                    "Before first local bind or on local setup failure run read-only",
		"ready-next-steps":             "until ready",
		"cwd-not-binding":              "Do not rebind on cwd changes",
		"reuse-collaboration":          "reuse the Room, follow its collaboration rules",
		"create-command":               "pairroom relay bind --create --name",
		"no-runtime-selection":         "No Runtime types",
		"stable-handles":               "legacy Rooms use printed handles",
		"explicit-slot-only":           "only on request",
		"no-recreate":                  "recover, never repeat",
		"display-join-command":         "Display `peer_join_local` (same workspace) or `peer_join` unchanged",
		"join-command-as-is":           "run it unchanged with all selectors",
		"join-room-id-fallback":        "pairroom relay bind --room <exact-id>",
		"join-slot-rule":               "unless requested and let the CLI pick",
		"join-no-guess-create-replace": "Never guess, create or replace implicitly",
		"room-id-not-an-invite":        "A Room ID is not a LAN invitation",
		"join-intent-outranks-create":  "Join intent outranks preflight create guidance",
		"numeric-slots":                "Slots are `1|2`, not runtimes",
		"immediate-bind":               "Bind is ready; no nonce, turn/status check or Provider setup",
		"join-announcement":            "On a new join to a bound peer, announce once via",
		"announcement-no-wait":         "no wait or final peer handle",
		"skip-first-or-resume":         "Skip on resume or binding first",
		"daemon-consent":               "Install the daemon only with confirmation",
		"no-private-state":             "Recover through the CLI, not private state",
		"no-approval-bypass":           "never bypass approvals",
	})
	assertSkillRules(t, "references/lan.md", map[string]string{
		"lan-join-preflight":       "pairroom relay preflight --join",
		"host-create":              "pairroom relay bind --create --share lan",
		"no-guest-service":         "need no local Service",
		"request-only":             "requests admission only",
		"trusted-receipt":          "exact receipt supplied through a trusted channel",
		"no-local-approval":        "Text/files grant no local approvals",
		"explicit-lan-replacement": "only after the old membership ends; obtain fresh receipt approval",
	})
	assertSkillRules(t, "references/grok.md", map[string]string{
		"grok-consent":              "folder trust is the human's",
		"grok-no-wake-and-cap":      "no Service wake and at most seven Stop continuations",
		"grok-unattended-condition": "Long unattended runs need tracked completion: one background wait in Grok",
		"grok-no-duplicate-hook":    "Reuse a compatible project hook; do not add a duplicate",
		"grok-readiness":            "for full input",
		"grok-full-clipped-reply":   "never a truncated prefix or already-confirmed resend",
		"grok-shell-identity":       "lacks `GROK_SESSION_ID` and skips Stop",
		"grok-tool-command":         "run relay through the agent's terminal tool",
		"grok-human-shell-recovery": "rerun it yourself; never ask for a session ID",
	})
	assertSkillRules(t, "references/gemini.md", map[string]string{
		"gemini-hook-approval":   "Approve BeforeTool/AfterAgent",
		"gemini-standalone":      "standalone `run_shell_command` tool call",
		"gemini-no-service-wake": "There is no Service wake",
	})
	assertSkillRules(t, "references/recovery.md", map[string]string{
		"peer-generation-wal":          "original peer generation and body stay local",
		"explicit-generation-decision": "send/exchange --peer-generation N --id NEW",
		"same-session-fence":           "cannot reset automatic context; a new native session can",
		"inspection-only":              "Inspection never requeues work or proves model acceptance",
		"explicit-local-replacement":   "pairroom relay bind --replace --slot 1|2",
	})
	assertSkillRules(t, "references/enterprise.md", map[string]string{
		"exact-origin":        "exact HTTPS origin and deployment ID",
		"foreground-login":    "Device login is foreground work with browser approval",
		"exact-admission":     "Owner approves the exact request",
		"original-endpoint":   "original endpoint repeats its original bind",
		"no-fallback":         "do not use local/LAN fallback",
		"no-hook-renewal":     "Hooks never initiate login, refresh or renewal",
		"explicit-renewal":    "pairroom relay bind --renew-credential",
		"retain-uncertainty":  "Keep uncertain IDs and bytes",
		"proven-upload-retry": "Retry only a proven missing upload, with the same object and bytes",
		"claim-ack-order":     "Verify before Claim; ACK after stdout",
		"local-logout":        "does not revoke server authority",
	})
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

// A default pair that already places the creator first keeps its order.
func TestBindCreatePreservesCreatorFirstDefaultPair(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := editHooks(root, model.RuntimeCodex, false); err != nil {
		t.Fatal(err)
	}
	created := 0
	mux := http.NewServeMux()
	serveDefaultPairForTest(mux, map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: model.RuntimeCodex}, model.ActorSlot2: {Runtime: model.RuntimeClaude}})
	mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
		rooms := []any{}
		for i := 1; i <= created; i++ {
			rooms = append(rooms, map[string]any{"id": fmt.Sprintf("room%d", i), "project_id": "p1", "host_mode": "native", "lifecycle": "active",
				"agents": map[string]any{"slot1": map[string]string{"runtime": "codex"}, "slot2": map[string]string{"runtime": "claude"}}})
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
		_ = json.NewEncoder(w).Encode(map[string]any{"binding": relay.Binding{BindID: request.BindID, Generation: 1, Slot: model.ActorID(bound), Active: true, SessionID: request.SessionID}, "bootstrap": "b", "collaboration": "c"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	endpointPath := filepath.Join(t.TempDir(), "relay-endpoint.json")
	if err := relay.AtomicJSON(endpointPath, relay.Endpoint{URL: srv.URL, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	stubLineage(t, 4242, "codex", true)
	t.Setenv("CODEX_SESSION_ID", "official-session")
	var out bytes.Buffer
	if err := bind(context.Background(), root, options{create: true, endpoint: endpointPath}, &out); err != nil {
		t.Fatalf("create bind against a swapped default pair: %v", err)
	}
	if bound != "slot1" {
		t.Fatalf("a codex harness must bind the slot that runs codex, bound slot = %q", bound)
	}
	if _, err := os.Stat(filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "slot1", "state.json")); err != nil {
		t.Fatalf("state was not materialized in the runtime-matched slot: %v", err)
	}
	var state State
	if err := readPrivate(filepath.Join(root, ".pairroom", "rooms", "room1", "slots", "slot1", "state.json"), &state); err != nil {
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
	// stubLineage isolates the caller (clearing session env); restore it so the
	// missing-hook check, not a missing identity, is what rejects this create.
	t.Setenv("CODEX_SESSION_ID", "official-session")
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
	if err == nil || !strings.Contains(err.Error(), "inside your native session") {
		t.Fatalf("err = %v", err)
	}
	if *created != 0 || out.Len() != 0 {
		t.Fatalf("an unresolvable creator slot must not create durable state: created=%d output=%q", *created, out.String())
	}
}

func TestInstallSkillUpgradesOwnProjectionAndRefusesUnrelated(t *testing.T) {
	IsolateNativeCaller(t)
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
