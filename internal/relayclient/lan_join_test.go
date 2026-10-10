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

func TestLANJoinLostResponseRetainsLocalIdentityAndPublicationWAL(t *testing.T) {
	isolateCaller(t)
	stubLineage(t, 4242, "codex", true)
	t.Setenv("CODEX_SESSION_ID", "official-guest-session")
	t.Setenv("HOME", t.TempDir())
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := editHooks(root, model.RuntimeCodex, false); err != nil {
		t.Fatal(err)
	}
	invite := lanJoinTestInvite(t)
	localID := lanRoutingID(invite)
	var mu sync.Mutex
	calls := 0
	var original map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/lan/join" {
			t.Errorf("unexpected local setup route %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		calls++
		if payload["workspace"] != root || payload["runtime"] != "codex" || payload["session_id"] != "official-guest-session" {
			t.Error("local setup did not associate exact guest workspace and official session")
		}
		if calls == 1 {
			original = payload
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		for _, key := range []string{"invite", "bind_id", "credential_hash", "session_id", "workspace"} {
			if payload[key] != original[key] {
				t.Errorf("retry changed durable local join %s", key)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "accepted", "room_id": localID, "binding": relay.Binding{Slot: model.ActorSlot2, Runtime: model.RuntimeCodex, BindID: payload["bind_id"].(string), Generation: 3, Active: true, SessionID: "official-guest-session"}, "bootstrap": "peer is @claude"})
	}))
	defer server.Close()
	endpoint := filepath.Join(t.TempDir(), "endpoint.json")
	if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: server.URL, Token: "local-setup-secret"}); err != nil {
		t.Fatal(err)
	}
	o := options{invitation: lanshare.EncodeInvite(invite), endpoint: endpoint}
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
	state.LastSeq = 7
	state.LastConfirmedSeq = 6
	state.Pending = &Pending{Seq: 7, Text: "@claude original uncertain publication", Unknown: true}
	if err := relay.AtomicJSON(path, state); err != nil {
		t.Fatal(err)
	}
	if err := resumeLANBinding(context.Background(), root, options{room: localID}, &output); err != nil {
		t.Fatal(err)
	}
	var resumed State
	if err := readPrivate(path, &resumed); err != nil || resumed.BindID != state.BindID || resumed.Generation != 3 || resumed.LastSeq != 7 || resumed.LastConfirmedSeq != 6 || resumed.Pending == nil || !resumed.Pending.Unknown || resumed.Pending.Text != state.Pending.Text {
		t.Fatalf("join recovery changed confirmed identity or outbox: %+v %v", resumed, err)
	}
	if strings.Contains(output.String(), attempt.Credentials.Secret) || strings.Contains(output.String(), "local-setup-secret") || strings.Contains(output.String(), "PRIVATE KEY") {
		t.Fatal("LAN join stdout disclosed a credential")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 3 {
		t.Fatalf("got %d local setup calls, want one attempt and two identity-preserving resumptions", calls)
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
