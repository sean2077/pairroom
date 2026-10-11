package relayclient

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
)

func TestDirectStateRequiresExplicitSchemaAndExactHostScopedRoute(t *testing.T) {
	invite := lanJoinTestInvite(t)
	id := lanRoutingID(invite)
	base := State{Schema: 3, Room: id, Slot: model.ActorSlot2, Runtime: model.RuntimeCodex, BindID: "binding", Generation: 1, SessionID: "native-session", LAN: &LANTransport{ClientID: id, Endpoint: invite.Endpoint, HostPin: invite.HostPin, RoomID: invite.RoomID}}
	if !validStateFormat(base) || !validStateFormat(State{Schema: 2}) {
		t.Fatal("current direct or existing local state format was rejected")
	}
	cases := map[string]func(*State){
		"disguised-as-local":      func(s *State) { s.Schema = 2 },
		"missing-direct-route":    func(s *State) { s.LAN = nil },
		"global-service-fallback": func(s *State) { s.EndpointPath = filepath.Join(t.TempDir(), "endpoint.json") },
		"different-host":          func(s *State) { s.LAN.HostPin = lanJoinTestInvite(t).HostPin },
		"different-room":          func(s *State) { s.LAN.RoomID = "another-room" },
		"public-target":           func(s *State) { s.LAN.Endpoint = "https://8.8.8.8:443" },
		"unscoped-client-id":      func(s *State) { s.LAN.ClientID = "lan_other" },
		"future-format":           func(s *State) { s.Schema = 4 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			state := base
			route := *base.LAN
			state.LAN = &route
			change(&state)
			if validStateFormat(state) {
				t.Fatal("invalid direct route was accepted")
			}
			dir := filepath.Join(t.TempDir(), "slot")
			if err := privatefile.Mkdir(dir); err != nil {
				t.Fatal(err)
			}
			if err := privatefile.WriteJSON(filepath.Join(dir, "state.json"), state); err != nil {
				t.Fatal(err)
			}
			// No credential file: validation must reject the format before it
			// can read a credential or consult any default Service.
			if _, err := loadLocal(dir); err == nil || !strings.Contains(err.Error(), "state format") {
				t.Fatal("invalid routing state reached transport setup")
			}
		})
	}
	other := lanshare.Invite{HostPin: lanJoinTestInvite(t).HostPin, RoomID: invite.RoomID}
	if lanRoutingID(other) == id {
		t.Fatal("equal Room IDs on different hosts share a local route")
	}
}

// LAN workspace directories hold transport credentials. Whichever CLI path
// reaches them first must create them owner-private, so a later LAN slot lock
// and private state write cannot fail forever on a directory this CLI made.
func TestLANWorkspaceDirectoriesAreCreatedOwnerPrivate(t *testing.T) {
	root := t.TempDir()
	for _, parts := range [][]string{
		{".pairroom", "rooms", "lan_fixture", "slots", "slot1"},
		{".pairroom", "lan-joins", "lan_fixture"},
	} {
		dir, err := secureDir(root, parts...)
		if err != nil {
			t.Fatal(err)
		}
		if err := privatefile.CheckDirectory(dir); err != nil {
			t.Fatalf("LAN state directory %v was not owner-private: %v", parts, err)
		}
	}
	if _, err := secureDir(root, ".pairroom", "rooms", "room-1", "slots", "slot1"); err != nil {
		t.Fatalf("ordinary slot directory: %v", err)
	}
	// A pre-existing shared LAN directory fails closed instead of being silently
	// re-owner-ed.
	shared := filepath.Join(root, ".pairroom", "rooms", "lan_shared", "slots", "slot1")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := secureDir(root, ".pairroom", "rooms", "lan_shared", "slots", "slot1"); !errors.Is(err, privatefile.ErrPrivate) {
		t.Fatalf("shared LAN directory was accepted: %v", err)
	}
}

func TestLANWorkspacePrivacyRecognitionKeepsOrdinaryProjectNames(t *testing.T) {
	for _, parent := range []string{"rooms", "lan-joins"} {
		t.Run(parent, func(t *testing.T) {
			// A user's checkout can have the same names as LAN state without
			// being below .pairroom. Its ordinary local Room paths keep their
			// existing directory policy, including pre-existing shared parents.
			root := filepath.Join(t.TempDir(), parent, "lan_project")
			stateRoot := filepath.Join(root, ".pairroom")
			if err := os.MkdirAll(stateRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(stateRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := secureDir(root, ".pairroom", "rooms", "room-local", "slots", "slot1"); err != nil {
				t.Fatalf("ordinary local Room inherited LAN directory rules from the checkout name: %v", err)
			}
		})
	}
}

// The offline recovery reader exists for bindings whose owner-only boundary was
// lost: a workspace restored from a backup, copied from another machine, or an
// inherited Windows DACL. It keeps the structural bounds and refuses anything
// that is not a plain bounded file.
func TestOfflineRecoveryReadsLostBoundaryButNotUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	state := State{Schema: 3, Room: "lan_room", Slot: model.ActorSlot2, Runtime: model.RuntimeCodex, BindID: "binding", Generation: 1, SessionID: "native-session", LAN: &LANTransport{ClientID: "lan_room", Endpoint: "https://192.168.1.2:8877", HostPin: strings.Repeat("a", 64), RoomID: "shared"}}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	var strict State
	if err := readPrivate(path, &strict); err == nil {
		t.Fatal("strict private read accepted a state file without the owner-only boundary")
	}
	var recovered State
	if err := readPrivateRecovery(path, &recovered); err != nil || recovered.BindID != state.BindID || recovered.LAN == nil {
		t.Fatalf("recovery read: %+v, %v", recovered, err)
	}
	link := filepath.Join(dir, "linked.json")
	if err := os.Symlink(path, link); err == nil {
		if err := readPrivateRecovery(link, &recovered); err == nil {
			t.Fatal("recovery followed a symlink")
		}
	}
	big := filepath.Join(dir, "big.json")
	if err := os.WriteFile(big, make([]byte, maxPrivateFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := readPrivateRecovery(big, &recovered); err == nil {
		t.Fatal("recovery accepted an oversized file")
	}
}

// Owner-boundary recovery must not relax the direct format decoder: an unknown
// publication or credential field may belong to a newer writer and is retained
// for inspection rather than silently discarded by local retirement.
func TestOfflineRecoveryKeepsStrictBindingFileFormat(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  string
		value any
	}{
		{"state.json", `{"schema":3,"future_publication":"retain for inspection"}`, &State{}},
		{"credentials", `{"bind_id":"original","secret":"fixture","future_capability":"retain for inspection"}`, &credentials{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.name)
			if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
				t.Fatal(err)
			}
			breakOwnerBoundary(t, path)
			strict := readPrivate
			if tc.name == "credentials" {
				strict = func(path string, value any) error { return privatefile.ReadJSON(path, maxPrivateFileBytes, value) }
			}
			if _, err := readBindingFile(path, tc.value, strict, true); err == nil {
				t.Fatal("owner-boundary recovery accepted an unknown binding field")
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != tc.data {
				t.Fatalf("rejected binding data changed: %v", err)
			}
		})
	}
}
