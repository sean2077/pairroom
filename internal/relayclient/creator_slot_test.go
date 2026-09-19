package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// Exercise the public CLI through creation, binding, local persistence and
// resume. These are synthetic HTTP/harness fixtures, not real vendor E2E.
func TestNativeCreatorSlot(t *testing.T) {
	for _, tc := range []struct {
		name             string
		caller, peer     model.RuntimeKind
		flags            []string
		profile          bool
		creatorFirst     bool
		explicitRuntimes bool
		wantSlot         model.ActorID
		wantError        bool
	}{
		{name: "claude/catalog", caller: model.RuntimeClaude, peer: model.RuntimeCodex},
		{name: "codex/catalog", caller: model.RuntimeCodex, peer: model.RuntimeClaude},
		{name: "grok/catalog", caller: model.RuntimeGrok, peer: model.RuntimeClaude},
		{name: "codex/creator-first", caller: model.RuntimeCodex, peer: model.RuntimeClaude, creatorFirst: true},
		{name: "codex/profile", caller: model.RuntimeCodex, peer: model.RuntimeClaude, profile: true},
		{name: "codex/same-runtime", caller: model.RuntimeCodex, peer: model.RuntimeCodex, profile: true, creatorFirst: true},
		{name: "codex/peer-flag", caller: model.RuntimeCodex, peer: model.RuntimeClaude, flags: []string{"--peer-runtime", "claude"}, explicitRuntimes: true},
		{name: "grok/peer-flag", caller: model.RuntimeGrok, peer: model.RuntimeClaude, flags: []string{"--peer-runtime", "claude"}, explicitRuntimes: true},
		{name: "codex/runtime-flag", caller: model.RuntimeCodex, peer: model.RuntimeClaude, flags: []string{"--runtime", "codex"}, explicitRuntimes: true},
		{name: "codex/explicit-slot-2", caller: model.RuntimeCodex, peer: model.RuntimeClaude, flags: []string{"--slot", "2"}, wantSlot: model.ActorSlot2},
		{name: "codex/explicit-slot-2-and-peer", caller: model.RuntimeCodex, peer: model.RuntimeClaude, flags: []string{"--slot", "2", "--peer-runtime", "claude"}, wantSlot: model.ActorSlot2, explicitRuntimes: true},
		{name: "codex/explicit-mismatch", caller: model.RuntimeCodex, peer: model.RuntimeClaude, flags: []string{"--slot", "1"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			IsolateNativeCaller(t)
			t.Setenv(sessionEnvVars[tc.caller], "creator-session")
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
				t.Fatalf("git init: %v %s", err, output)
			}
			if err := editHooks(root, tc.caller, false); err != nil {
				t.Fatal(err)
			}
			selection := func(kind model.RuntimeKind, label string) model.AgentSelection {
				s := model.AgentSelection{
					Runtime:  kind,
					Provider: model.ProviderRef{Source: model.ProviderCCSwitch, AppType: string(kind), ProfileID: label},
					Model:    label + "-model", Effort: "high", Instructions: label + " instructions",
				}
				switch kind {
				case model.RuntimeClaude:
					s.PermissionMode = "acceptEdits"
				case model.RuntimeCodex:
					s.ApprovalPolicy, s.Sandbox = "on-request", "workspace-write"
				case model.RuntimeGrok:
					s.PermissionMode, s.Sandbox = "ask", "workspace"
				}
				return s
			}
			own, peer := selection(tc.caller, "creator"), selection(tc.peer, "peer")
			defaults := map[model.ActorID]model.AgentSelection{model.ActorSlot1: peer, model.ActorSlot2: own}
			if tc.creatorFirst {
				defaults[model.ActorSlot1], defaults[model.ActorSlot2] = own, peer
			}
			var mu sync.Mutex
			var created map[model.ActorID]model.AgentSelection
			var bound []model.ActorID
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/agent-pair-profiles", func(w http.ResponseWriter, r *http.Request) {
				if tc.profile {
					_ = json.NewEncoder(w).Encode(map[string]any{"default_profile_id": "preferred", "profiles": []any{map[string]any{"id": "preferred", "agents": defaults}}})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{"default_profile_id": ""})
				}
			})
			mux.HandleFunc("GET /api/v1/agent-catalog", func(w http.ResponseWriter, r *http.Request) {
				if tc.profile {
					t.Error("selected default profile must not fall back to the catalog")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"defaults": defaults})
			})
			mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				snapshot := serviceSnapshot{Projects: []serviceProject{{ID: "project", Root: root}}}
				if created != nil {
					snapshot.Rooms = []serviceRoom{{ID: "room", ProjectID: "project", HostMode: model.HostNative, Lifecycle: roomLifecycleActive, Agents: created}}
				}
				_ = json.NewEncoder(w).Encode(snapshot)
			})
			mux.HandleFunc("POST /api/v1/projects/project/rooms", func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					HostMode model.HostMode                         `json:"host_mode"`
					Agents   map[model.ActorID]model.AgentSelection `json:"agents"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if created != nil || request.HostMode != model.HostNative || len(request.Agents) != 2 {
					t.Error("creation must send exactly one native Room with both prepared selections")
				}
				created = request.Agents
				_ = json.NewEncoder(w).Encode(map[string]string{"id": "room"})
			})
			mux.HandleFunc("POST /api/v1/rooms/room/native-bindings/{slot}", func(w http.ResponseWriter, r *http.Request) {
				var request relay.BindRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				slot := model.ActorID(r.PathValue("slot"))
				mu.Lock()
				bound = append(bound, slot)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"binding": relay.Binding{BindID: request.BindID, Generation: 1, Slot: slot, Active: true, SessionID: request.SessionID}})
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			endpoint := filepath.Join(t.TempDir(), relay.EndpointFile)
			if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: server.URL, Token: "test-token"}); err != nil {
				t.Fatal(err)
			}
			base := []string{"bind", "--repo", root, "--service-file", endpoint}
			args := append(append([]string{}, base...), "--create")
			args = append(args, tc.flags...)
			var out bytes.Buffer
			err = Run(context.Background(), args, strings.NewReader(""), &out, io.Discard)
			if tc.wantError {
				mu.Lock()
				defer mu.Unlock()
				if err == nil || !strings.Contains(err.Error(), "selected creator slot does not match") || created != nil || len(bound) != 0 || out.Len() != 0 {
					t.Fatalf("explicit mismatch must fail before provisioning: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantSlot := tc.wantSlot
			if wantSlot == "" {
				wantSlot = model.ActorSlot1
			}
			if tc.explicitRuntimes {
				own = (model.AgentSelection{Runtime: tc.caller}).Normalized(wantSlot)
				peer = (model.AgentSelection{Runtime: tc.peer}).Normalized(peerSlot(wantSlot))
			}
			want := map[model.ActorID]model.AgentSelection{wantSlot: own, peerSlot(wantSlot): peer}
			mu.Lock()
			matches := reflect.DeepEqual(created, want) && len(bound) == 1 && bound[0] == wantSlot
			mu.Unlock()
			if !matches {
				t.Fatal("creation or binding changed the creator slot or detached runtime-specific settings")
			}
			var result struct {
				Binding       relay.Binding `json:"binding"`
				PeerJoin      string        `json:"peer_join"`
				PeerJoinLocal string        `json:"peer_join_local"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Binding.Slot != wantSlot || result.PeerJoin != bindCommand(root, endpoint, "room", peerSlot(wantSlot)) || result.PeerJoinLocal != localBindCommand(endpoint, "room", peerSlot(wantSlot)) {
				t.Fatal("bind output did not direct the peer to the other slot")
			}
			dir := filepath.Join(root, ".pairroom", "rooms", "room", "slots", string(wantSlot))
			var before, after State
			if err := readPrivate(filepath.Join(dir, "state.json"), &before); err != nil {
				t.Fatal(err)
			}
			if before.Slot != wantSlot || before.Runtime != tc.caller || before.SessionID != "creator-session" || before.Generation != 1 {
				t.Fatal("local state did not preserve the creator identity")
			}
			if _, err := os.Stat(filepath.Join(root, ".pairroom", "rooms", "room", "slots", string(peerSlot(wantSlot)), "state.json")); !os.IsNotExist(err) {
				t.Fatalf("creator also materialized the peer slot: %v", err)
			}
			if err := Run(context.Background(), base, strings.NewReader(""), io.Discard, io.Discard); err != nil {
				t.Fatalf("resume: %v", err)
			}
			if err := readPrivate(filepath.Join(dir, "state.json"), &after); err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("resume changed the existing slot or binding: %v", err)
			}
		})
	}
}
