package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

// Use the real private client store and pinned TLS admission. The host stops
// after admission: every discovery and local retirement below must stay local.
func directDiscoveryFixture(t *testing.T) (State, *lanclient.Client) {
	t.Helper()
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := (model.Collaboration{}).ForCreation()
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/lan/v1/rooms/discovery-room/join" || r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			t.Error("unexpected discovery fixture request")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var request lanshare.JoinRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		response := lanshare.JoinResponse{Status: "accepted", Receipt: lanshare.EncodeReceipt(lanshare.Receipt{RoomID: "discovery-room", RequestID: request.RequestID, Fingerprint: lanshare.Fingerprint(r.TLS.PeerCertificates[0])}), Room: &lanshare.RoomInfo{RoomID: "discovery-room", Slot: model.ActorSlot2, Generation: 3, BindID: "host-binding", Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}, Collaboration: &policy}}
		_ = json.NewEncoder(w).Encode(response)
	}))
	server.TLS, err = lanshare.ServerTLS(identity)
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	t.Cleanup(server.Close)
	invite := lanshare.Invite{Version: 1, Endpoint: server.URL, HostPin: pin, RoomID: "discovery-room", InviteID: "discovery-invitation", ExpiresAt: time.Now().Add(time.Minute)}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	client, joined, err := store.Join(context.Background(), lanclient.JoinOptions{Invite: lanshare.EncodeInvite(invite), Workspace: root, Runtime: model.RuntimeCodex, SessionID: "discovery-session", BindID: "local-binding", CredentialHash: relay.Digest("local-secret")})
	if err != nil || joined.Status != "accepted" {
		t.Fatalf("direct fixture admission: %+v %v", joined, err)
	}
	server.Close()
	s := State{Schema: 3, Room: joined.ID, Slot: model.ActorSlot2, Runtime: model.RuntimeCodex, Workspace: root, LAN: &LANTransport{ClientID: joined.ID, Endpoint: invite.Endpoint, HostPin: invite.HostPin, RoomID: invite.RoomID}, BindID: "local-binding", Generation: 3, SessionID: "discovery-session", LastSeq: 2, Pending: &Pending{Seq: 2, Text: "original uncertain reply", Unknown: true}}
	slotDir, err := secureLANJoinDir(root, "rooms", s.Room, "slots", string(s.Slot))
	if err != nil {
		t.Fatal(err)
	}
	if err := privatefile.WriteJSON(filepath.Join(slotDir, "state.json"), s); err != nil {
		t.Fatal(err)
	}
	if err := rememberSession(s); err != nil {
		t.Fatal(err)
	}
	return s, client
}

func TestDirectDiscoveryRetiresOptionalDashboardDetachWithoutRewritingWAL(t *testing.T) {
	isolateCaller(t)
	t.Setenv("CODEX_SESSION_ID", "discovery-session")
	s, client := directDiscoveryFixture(t)
	caller := nativeCaller{runtime: s.Runtime, session: s.SessionID}
	path := filepath.Join(s.Workspace, ".pairroom", "rooms", s.Room, "slots", string(s.Slot), "state.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		t.Helper()
		if states, err := indexedSessions(caller); err != nil || len(states) != want {
			t.Fatalf("indexed discovery: %d states, want %d: %v", len(states), want, err)
		}
		for _, passive := range []bool{false, true} {
			if states, err := scanMatchingSessions(s.Workspace, caller, passive); err != nil || len(states) != want {
				t.Fatalf("workspace discovery (passive=%v): %d states, want %d: %v", passive, len(states), want, err)
			}
		}
		if paths, err := boundHookCandidates([]string{path}, s.Runtime, s.SessionID); err != nil || len(paths) != want {
			t.Fatalf("hook discovery: %d paths, want %d: %v", len(paths), want, err)
		}
		o := options{}
		err := applyCallerDefaults(s.Workspace, "status", &o)
		if want == 1 && (err != nil || o.room != s.Room) || want == 0 && !errors.Is(err, errNoAssociatedBinding) {
			t.Fatalf("foreground defaults: room=%q error=%v", o.room, err)
		}
		if err := applyCallerDefaults(s.Workspace, "bind", &options{create: true}); (want == 1) != (err != nil) {
			t.Fatalf("create still selected a retired association: %v", err)
		}
	}
	check(1)
	if err := client.Owner(context.Background(), "detach", struct{}{}, nil); err != nil {
		t.Fatal(err)
	}
	check(0)
	if paths, err := boundHookCandidates([]string{path}, s.Runtime, "another-session"); err != nil || len(paths) != 0 {
		t.Fatalf("retired env-session binding triggered a hook mismatch: %v", err)
	}
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, original) {
		t.Fatal("discovery rewrote or removed the retired publication WAL")
	}
}

func TestDirectDiscoveryFailsClosedOnMissingOrChangedOriginalAuthority(t *testing.T) {
	isolateCaller(t)
	t.Setenv("CODEX_SESSION_ID", "discovery-session")
	s, _ := directDiscoveryFixture(t)
	for name, mutate := range map[string]func(*State){
		"workspace":  func(s *State) { s.Workspace = t.TempDir() },
		"runtime":    func(s *State) { s.Runtime = model.RuntimeClaude },
		"session":    func(s *State) { s.SessionID = "changed-session" },
		"generation": func(s *State) { s.Generation++ },
		"slot":       func(s *State) { s.Slot = model.ActorSlot1 },
		"endpoint":   func(s *State) { s.LAN.Endpoint = "https://127.0.0.1:44991" },
	} {
		t.Run(name, func(t *testing.T) {
			changed, route := s, *s.LAN
			changed.LAN = &route
			mutate(&changed)
			if current, err := currentDirectBinding(changed); err == nil || current {
				t.Fatal("changed binding still matched the authoritative original client")
			}
		})
	}
	older := s
	older.BindID = "retired-local-binding"
	if current, err := currentDirectBinding(older); err != nil || current {
		t.Fatalf("superseded workspace binding was not inert: %v", err)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	clientPath := filepath.Join(base, "pairroom", "lan-clients", s.Room, "client.json")
	if err := os.Remove(clientPath); err != nil {
		t.Fatal(err)
	}
	caller := nativeCaller{runtime: s.Runtime, session: s.SessionID}
	if _, err := indexedSessions(caller); err == nil {
		t.Fatal("locator silently bypassed a missing authoritative client")
	}
	if _, err := matchingSessions(s.Workspace, caller); err == nil {
		t.Fatal("foreground workspace scan silently bypassed a missing authoritative client")
	}
	if err := applyCallerDefaults(s.Workspace, "bind", &options{create: true}); err == nil {
		t.Fatal("missing client authority released session ownership for create")
	}
	path := filepath.Join(s.Workspace, ".pairroom", "rooms", s.Room, "slots", string(s.Slot), "state.json")
	if _, err := boundHookCandidates([]string{path}, s.Runtime, s.SessionID); err == nil {
		t.Fatal("an exact bound hook silently bypassed missing authority")
	}
	if states, err := scanMatchingSessions(s.Workspace, caller, true); err != nil || len(states) != 0 {
		t.Fatalf("unassociated passive hook repaired broken authority: %v", err)
	}
	if _, err := os.Stat(clientPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discovery recreated a missing authoritative client record")
	}
}
