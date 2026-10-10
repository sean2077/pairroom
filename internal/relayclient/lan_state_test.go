package relayclient

import (
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
