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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

func lanJoinTestInvite(t *testing.T) lanshare.Invite {
	t.Helper()
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	pin, err := identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	return lanshare.Invite{Version: 1, Endpoint: "https://127.0.0.1:44991", HostPin: pin, RoomID: "remote-room", InviteID: "invite-one", ExpiresAt: time.Now().Add(time.Minute)}
}

func TestLANJoinLostResponseRetainsDirectIdentityAndPublicationWAL(t *testing.T) {
	isolateCaller(t)
	stubLineage(t, 4242, "codex", true)
	t.Setenv("CODEX_SESSION_ID", "official-guest-session")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := editHooks(root, model.RuntimeCodex, false); err != nil {
		t.Fatal(err)
	}
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
	var mu sync.Mutex
	calls := 0
	var original lanshare.JoinRequest
	var fingerprint string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 || r.Header.Get("Authorization") != "" || r.Header.Get("X-PairRoom-Session") != "" {
			t.Error("direct join failed its certificate/local-secret boundary")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		key := lanshare.Fingerprint(r.TLS.PeerCertificates[0])
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		for _, forbidden := range []string{"workspace", "session_id", "bind_id", "credential_hash", "transcript_path"} {
			if _, ok := body[forbidden]; ok {
				t.Errorf("native local metadata crossed LAN: %s", forbidden)
			}
		}
		encoded, _ := json.Marshal(body)
		switch r.URL.Path {
		case "/lan/v1/rooms/remote-room/join":
			var request lanshare.JoinRequest
			if err := json.Unmarshal(encoded, &request); err != nil {
				t.Error(err)
				return
			}
			if calls == 1 {
				original, fingerprint = request, key
				connection, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = connection.Close()
				return
			}
			if request != original || key != fingerprint {
				t.Error("retry rotated the persisted request or room key")
			}
		case "/lan/v1/rooms/remote-room/join-status":
			var request lanshare.JoinStatusRequest
			if err := json.Unmarshal(encoded, &request); err != nil || request.RequestID != original.RequestID || key != fingerprint {
				t.Error("resume lost its original request/key")
			}
		default:
			t.Errorf("unexpected direct route %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		response := lanshare.JoinResponse{Status: "accepted", Receipt: lanshare.EncodeReceipt(lanshare.Receipt{RoomID: "remote-room", RequestID: original.RequestID, Fingerprint: fingerprint}), Room: &lanshare.RoomInfo{RoomID: "remote-room", Slot: model.ActorSlot2, Generation: 3, BindID: "host-association", Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex}, Collaboration: &policy}}
		_ = json.NewEncoder(w).Encode(response)
	}))
	server.TLS, err = lanshare.ServerTLS(identity)
	if err != nil {
		t.Fatal(err)
	}
	server.StartTLS()
	defer server.Close()
	invite := lanshare.Invite{Version: 1, Endpoint: server.URL, HostPin: pin, RoomID: "remote-room", InviteID: "invite-one", ExpiresAt: time.Now().Add(time.Minute)}
	localID := lanRoutingID(invite)
	o := options{invitation: lanshare.EncodeInvite(invite)}
	var output bytes.Buffer
	if err := joinLAN(context.Background(), root, o, &output); err == nil {
		t.Fatal("lost response was not reported as uncertain")
	}
	var attempt lanJoinAttempt
	if err := privatefile.ReadJSON(filepath.Join(root, ".pairroom", "lan-joins", localID, "join-attempt.json"), maxPrivateFileBytes, &attempt); err != nil {
		t.Fatal(err)
	}
	states, err := statePaths(root)
	if err != nil || len(states) != 0 {
		t.Fatal("unconfirmed LAN admission entered active session discovery")
	}
	if err := joinLAN(context.Background(), root, o, &output); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".pairroom", "rooms", localID, "slots", "slot2", "state.json")
	var state State
	if err := readPrivate(path, &state); err != nil {
		t.Fatal(err)
	}
	if state.Schema != 3 || state.LAN == nil || state.EndpointPath != "" || state.LAN.Endpoint != server.URL || state.LAN.HostPin != pin {
		t.Fatal("direct state acquired a local Service dependency or lost its pinned target")
	}
	state.LastSeq = 7
	state.LastConfirmedSeq = 6
	state.Pending = &Pending{Seq: 7, Text: "@claude original uncertain publication", Unknown: true}
	if err := privatefile.WriteJSON(path, state); err != nil {
		t.Fatal(err)
	}
	if err := resumeLANBinding(context.Background(), root, options{room: localID}, &output); err != nil {
		t.Fatal(err)
	}
	var resumed State
	if err := readPrivate(path, &resumed); err != nil || resumed.BindID != state.BindID || resumed.Generation != 3 || resumed.LastSeq != 7 || resumed.LastConfirmedSeq != 6 || resumed.Pending == nil || !resumed.Pending.Unknown || resumed.Pending.Text != state.Pending.Text {
		t.Fatalf("join recovery changed confirmed identity or outbox: %+v %v", resumed, err)
	}
	if strings.Contains(output.String(), attempt.Credentials.Secret) || strings.Contains(output.String(), "PRIVATE KEY") {
		t.Fatal("LAN join stdout disclosed a credential")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("got %d remote setup calls, want one attempt and two identity-preserving resumptions", calls)
	}
}

func TestLANJoinMissingNativeIdentityHasNoLocalAttempt(t *testing.T) {
	isolateCaller(t)
	root := t.TempDir()
	invite := lanJoinTestInvite(t)
	var output bytes.Buffer
	err := joinLAN(context.Background(), root, options{invitation: lanshare.EncodeInvite(invite)}, &output)
	if err == nil {
		t.Fatal("plain terminal created a native join association")
	}
	if _, err := os.Stat(filepath.Join(root, ".pairroom")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("identity rejection created a private join attempt")
	}
}

func TestLANCreatePreflightUsesActualCreatorWithoutDefaultPeer(t *testing.T) {
	root, _, _ := createBindFixture(t, model.RuntimeClaude)
	if err := editHooks(root, model.RuntimeClaude, false); err != nil {
		t.Fatal(err)
	}
	// A LAN creator has no reason to query a local saved pair. The guest's
	// official Runtime is selected only during exact-key admission.
	o, slot, err := prepareNativeCreation(context.Background(), relay.Endpoint{URL: "invalid-no-network"}, root, options{share: "lan"}, "")
	if err != nil || slot != model.ActorSlot1 || len(o.preparedAgents) != 1 || o.preparedAgents[slot].Runtime != model.RuntimeClaude {
		t.Fatalf("LAN preflight guessed a peer: %+v %s %v", o.preparedAgents, slot, err)
	}
}
