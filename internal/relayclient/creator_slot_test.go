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
		name     string
		caller   model.RuntimeKind
		flags    []string
		wantSlot model.ActorID
	}{
		{name: "claude", caller: model.RuntimeClaude},
		{name: "codex", caller: model.RuntimeCodex},
		{name: "grok", caller: model.RuntimeGrok},
		{name: "codex/assert-runtime", caller: model.RuntimeCodex, flags: []string{"--runtime", "codex"}},
		{name: "codex/explicit-slot2", caller: model.RuntimeCodex, flags: []string{"--slot", "2"}, wantSlot: model.ActorSlot2},
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

			var mu sync.Mutex
			created := false
			var bound []model.ActorID
			mux := http.NewServeMux()
			for _, path := range []string{"GET /api/v1/agent-pair-profiles", "GET /api/v1/agent-catalog"} {
				mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
					t.Error("Native creation must not consult Managed pair defaults")
					http.Error(w, "unused", 500)
				})
			}
			mux.HandleFunc("GET /api/v1/service", func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				snapshot := serviceSnapshot{Projects: []serviceProject{{ID: "project", Root: root}}}
				if created {
					snapshot.Rooms = []serviceRoom{{ID: "room", ProjectID: "project", HostMode: model.HostNative, Lifecycle: roomLifecycleActive, DynamicBindings: true}}
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
				if created || request.HostMode != model.HostNative || request.Agents != nil {
					t.Error("creation must send exactly one Native Room without runtime selections")
				}
				created = true
				_ = json.NewEncoder(w).Encode(map[string]string{"id": "room"})
			})
			mux.HandleFunc("POST /api/v1/rooms/room/native-bindings/{slot}", func(w http.ResponseWriter, r *http.Request) {
				var request relay.BindRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					http.Error(w, "bad request", http.StatusBadRequest)
					return
				}
				if request.Runtime != tc.caller || request.SessionID != "creator-session" {
					t.Error("bind lost actual caller identity")
				}
				slot := model.ActorID(r.PathValue("slot"))
				mu.Lock()
				bound = append(bound, slot)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]any{"binding": relay.Binding{BindID: request.BindID, Generation: 1, Slot: slot, Active: true, SessionID: request.SessionID, Runtime: request.Runtime}, "dynamic_bindings": true, "peer_generation": 1})
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

			if err != nil {
				t.Fatal(err)
			}
			wantSlot := tc.wantSlot
			if wantSlot == "" {
				wantSlot = model.ActorSlot1
			}

			mu.Lock()
			matches := created && len(bound) == 1 && bound[0] == wantSlot
			mu.Unlock()
			if !matches {
				t.Fatal("creation or binding changed the creator slot")
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
			if before.Slot != wantSlot || before.Runtime != tc.caller || before.SessionID != "creator-session" || before.Generation != 1 || !before.DynamicBindings || before.AutomaticPeerGeneration != 1 {
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
