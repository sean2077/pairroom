package relayclient

import (
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
