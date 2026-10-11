package relayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

// The real admission Engine backs a deliberately small TLS wire fixture.
// These tests exercise CLI/private-workspace cutovers; the Service package's
// separate end-to-end tests verify the actual host router and evidence API.
type lanCLIWire struct {
	engine      *relay.Engine
	server      *httptest.Server
	root, pin   string
	policy      model.Collaboration
	requests    atomic.Int64
	connections atomic.Int64
}

func newLANCLIWire(t *testing.T) *lanCLIWire {
	t.Helper()
	isolateCaller(t)
	stubLineage(t, 4242, "codex", true)
	f := &lanCLIWire{root: sessionGitRoot(t)}
	var err error
	f.policy, err = (model.Collaboration{}).ForCreation()
	if err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.engine, err = relay.Open(relay.Config{RoomID: "cli-lifecycle-room", Store: log, SharedSlot: model.ActorSlot2, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeAwaitingPeer}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.engine.Close() })
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	f.pin, err = identity.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		key := lanshare.Fingerprint(r.TLS.PeerCertificates[0])
		action := strings.TrimPrefix(r.URL.Path, "/lan/v1/rooms/cli-lifecycle-room/")
		var value any
		var callErr error
		if action == "join" || action == "join-status" {
			var request lanshare.JoinRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			var status string
			if action == "join" {
				_, status, callErr = f.engine.RequestLANJoin(relay.LANJoinRequest{InviteID: request.InviteID, RequestID: request.RequestID, Key: key, Runtime: request.Runtime, Label: request.Label})
			} else {
				_, status, callErr = f.engine.LANJoinStatus(request.RequestID, key)
			}
			response := lanshare.JoinResponse{Status: status, Receipt: lanshare.EncodeReceipt(lanshare.Receipt{RoomID: "cli-lifecycle-room", RequestID: request.RequestID, Fingerprint: key})}
			if status == "accepted" {
				auth, err := f.engine.LANAuth(key)
				if err != nil {
					t.Error(err)
					w.WriteHeader(403)
					return
				}
				binding, err := f.engine.Inspect(auth)
				if err != nil {
					t.Error(err)
					w.WriteHeader(403)
					return
				}
				response.Room = &lanshare.RoomInfo{RoomID: "cli-lifecycle-room", Slot: binding.Slot, Generation: binding.Generation, BindID: binding.BindID, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: binding.Runtime}, Collaboration: &f.policy}
			}
			value = response
		} else {
			auth, err := f.engine.LANAuth(key)
			if err != nil || r.Header.Get("X-PairRoom-LAN-Bind") != auth.BindID || r.Header.Get("X-PairRoom-LAN-Generation") != strconv.FormatUint(auth.Generation, 10) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			switch action {
			case "summary":
				value, callErr = f.engine.AuthSummary(auth)
			case "unbind":
				callErr = f.engine.UnbindAs(auth)
				value = map[string]bool{"unbound": callErr == nil}
			default:
				t.Errorf("unexpected lifecycle wire action: %s", action)
				w.WriteHeader(404)
				return
			}
		}
		if callErr != nil {
			w.WriteHeader(403)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "fixture rejected admission"})
			return
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	f.server.TLS, err = lanshare.ServerTLS(identity)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			f.connections.Add(1)
		}
	}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	if err := editHooks(f.root, model.RuntimeCodex, false); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *lanCLIWire) invite(t *testing.T) lanshare.Invite {
	t.Helper()
	v, err := f.engine.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	return lanshare.Invite{Version: 1, Endpoint: f.server.URL, HostPin: f.pin, RoomID: "cli-lifecycle-room", InviteID: v.ID, ExpiresAt: v.ExpiresAt}
}

func (f *lanCLIWire) run(t *testing.T, session string, withRepo bool, args ...string) ([]byte, error) {
	t.Helper()
	t.Setenv("CODEX_SESSION_ID", session)
	if withRepo {
		args = append(args, "--repo", f.root)
	}
	var out, diagnostic bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := Run(ctx, args, bytes.NewReader(nil), &out, &diagnostic)
	return out.Bytes(), err
}

func (f *lanCLIWire) approve(t *testing.T, output []byte) {
	t.Helper()
	var pending struct{ Status, Receipt string }
	if json.Unmarshal(output, &pending) != nil || pending.Status != "pending" {
		t.Fatalf("not a pending join: %s", output)
	}
	receipt, err := lanshare.ParseReceipt(pending.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.AcceptLANJoin(receipt.RequestID, receipt.Fingerprint); err != nil {
		t.Fatal(err)
	}
}

func (f *lanCLIWire) bind(t *testing.T, session string) (lanshare.Invite, State) {
	t.Helper()
	invite := f.invite(t)
	output, err := f.run(t, session, true, "join", lanshare.EncodeInvite(invite))
	if err != nil {
		t.Fatalf("pending join: %s, %v", output, err)
	}
	f.approve(t, output)
	if output, err := f.run(t, session, true, "join", lanshare.EncodeInvite(invite)); err != nil {
		t.Fatalf("accepted join: %s, %v", output, err)
	}
	var state State
	if err := readPrivate(filepath.Join(f.root, ".pairroom", "rooms", lanRoutingID(invite), "slots", "slot2", "state.json"), &state); err != nil {
		t.Fatal(err)
	}
	return invite, state
}

func TestLANCLIAbandonsPendingOfflineWithoutSlotOrLocator(t *testing.T) {
	f := newLANCLIWire(t)
	invite := f.invite(t)
	if _, err := f.run(t, "pending-session", true, "join", lanshare.EncodeInvite(invite)); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	room := lanRoutingID(invite)
	if _, err := f.run(t, "foreign-session", true, "unbind", "--local-only", "--room", room); err == nil {
		t.Fatal("foreign session detached pending admission")
	}
	t.Chdir(sessionGitRoot(t))
	if output, err := f.run(t, "pending-session", false, "unbind", "--local-only", "--purge-hooks"); err != nil || !bytes.Contains(output, []byte("local-only")) {
		t.Fatalf("pending offline detach: %s, %v", output, err)
	}
	clients, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()
	client, err := clients.Get(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := client.Metadata(context.Background())
	if err != nil || meta.Status != "detached" {
		t.Fatalf("pending identity was not retained as detached: %+v, %v", meta, err)
	}
	if _, err := client.Resume(context.Background()); !errors.Is(err, lanclient.ErrInactive) {
		t.Fatal("detached request reconnected")
	}
	identities, _ := nativeidentity.Open()
	if err := identities.Check(context.Background(), nativeidentity.Claim{Runtime: meta.Runtime, SessionID: meta.SessionID, Association: nativeidentity.Remote(invite.HostPin, invite.RoomID), BindID: meta.BindID}); !errors.Is(err, nativeidentity.ErrUnowned) {
		t.Fatalf("pending native identity was not released: %v", err)
	}
}

func TestLANCLIRecoversReplacementBetweenPrivateAndWorkspaceCutover(t *testing.T) {
	f := newLANCLIWire(t)
	invite, original := f.bind(t, "original-session")
	statePath := filepath.Join(f.root, ".pairroom", "rooms", original.Room, "slots", "slot2", "state.json")
	// Disposable locators and an unrelated local Service file cannot redirect
	// a cold preflight in a different checkout.
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(base, "pairroom", "relay-sessions")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "pairroom", relay.EndpointFile), []byte("unrelated broken endpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sessionGitRoot(t))
	output, _ := f.run(t, "original-session", false, "preflight")
	var report preflightReport
	if json.Unmarshal(output, &report) != nil || report.Mode != "lan_direct" || report.Service.Status != checkPass || report.Workspace.Root != f.root {
		t.Fatalf("cold direct preflight: %s", output)
	}
	if output, err := f.run(t, "original-session", false, "bind"); err != nil {
		t.Fatalf("cold direct resume: %s, %v", output, err)
	}
	original.LastSeq = 1
	original.Pending = &Pending{Seq: 1, Text: "retired uncertain publication", Unknown: true}
	if err := privatefile.WriteJSON(statePath, original); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(statePath)
	unissued := invite
	unissued.InviteID = "unissued-new-invite"
	if _, err := f.run(t, "replacement-session", true, "join", lanshare.EncodeInvite(unissued), "--replace"); err == nil {
		t.Fatal("replaced active membership")
	}
	if after, _ := os.ReadFile(statePath); !bytes.Equal(before, after) {
		t.Fatal("rejected replacement changed the WAL")
	}
	if err := f.engine.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	fresh := f.invite(t)
	dir := filepath.Join(f.root, ".pairroom", "lan-joins", original.Room)
	o := options{invitation: lanshare.EncodeInvite(fresh), replace: true}
	// Persist the real new client request, then stop before the CLI moves any
	// workspace files. This is the actual cross-store interruption boundary.
	attempt, err := prepareLANReplacement(context.Background(), f.root, dir, original.Room, o, model.RuntimeCodex, "replacement-session")
	if err != nil {
		t.Fatal(err)
	}
	clients, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()
	_, pending, err := clients.Join(context.Background(), lanclient.JoinOptions{Invite: attempt.Invitation, Workspace: f.root, Runtime: attempt.Runtime, SessionID: attempt.SessionID, BindID: attempt.Credentials.BindID, CredentialHash: relay.Digest(attempt.Credentials.Secret), Replace: true})
	if err != nil || pending.Status != "pending" {
		t.Fatalf("private cutover: %+v, %v", pending, err)
	}
	if after, _ := os.ReadFile(statePath); !bytes.Equal(before, after) {
		t.Fatal("private cutover rewrote workspace state before recovery")
	}
	// An earlier archive write can also exist when that process stops.
	if err := archiveLANWorkspaceState(f.root, filepath.Dir(statePath), original); err != nil {
		t.Fatal(err)
	}
	output, err = f.run(t, "replacement-session", true, "join", attempt.Invitation, "--replace")
	if err != nil {
		t.Fatalf("recover staged replacement: %s, %v", output, err)
	}
	var recovered struct{ Status, Receipt string }
	if json.Unmarshal(output, &recovered) != nil || recovered.Receipt != pending.Receipt {
		t.Fatal("workspace recovery rotated the original new request/key")
	}
	archive := filepath.Join(f.root, ".pairroom", "retired", original.BindID)
	var retired State
	if err := privatefile.ReadJSON(filepath.Join(archive, "state.json"), maxPrivateFileBytes, &retired); err != nil || retired.Pending == nil || !retired.Pending.Unknown || retired.Pending.Text != original.Pending.Text {
		t.Fatalf("unknown WAL archive: %+v, %v", retired, err)
	}
	if _, err := os.Stat(filepath.Join(archive, "join-attempt.json")); err != nil {
		t.Fatal("original join identity was not archived")
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("retired slot remained active during pending replacement")
	}
	f.approve(t, output)
	for _, args := range [][]string{{"join", attempt.Invitation, "--replace"}, {"bind"}} {
		if output, err := f.run(t, "replacement-session", true, args...); err != nil {
			t.Fatalf("same replacement resume %v: %s, %v", args, output, err)
		}
	}
	var current State
	if err := readPrivate(statePath, &current); err != nil || current.BindID != attempt.Credentials.BindID || current.Generation <= original.Generation || current.Pending != nil || current.LastSeq != 0 {
		t.Fatalf("replacement reused old WAL or identity: %+v, %v", current, err)
	}
	if _, err := f.run(t, "original-session", true, "unbind", "--local-only", "--room", original.Room); err == nil {
		t.Fatal("old session detached the new membership")
	}
	if output, err := f.run(t, "replacement-session", true, "unbind"); err != nil {
		t.Fatalf("direct confirmed leave: %s, %v", output, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, ".pairroom", "retired", current.BindID, "state.json")); err != nil {
		t.Fatal("confirmed leave discarded its workspace state")
	}
	page, err := f.engine.History(relay.HistoryQuery{})
	if err != nil || len(page.Messages) != 0 {
		t.Fatalf("recovery replayed retired publication: %s, %v", fmt.Sprint(page.Messages), err)
	}
}

func TestLANCLIDetachesInstalledReplacementBeforeWorkspacePromotionOffline(t *testing.T) {
	f := newLANCLIWire(t)
	_, original := f.bind(t, "old-cutover-session")
	if err := f.engine.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	fresh := f.invite(t)
	dir := filepath.Join(f.root, ".pairroom", "lan-joins", original.Room)
	attempt, err := prepareLANReplacement(context.Background(), f.root, dir, original.Room, options{invitation: lanshare.EncodeInvite(fresh), replace: true}, model.RuntimeCodex, "new-cutover-session")
	if err != nil {
		t.Fatal(err)
	}
	clients, err := lanclient.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer clients.Close()
	client, pending, err := clients.Join(context.Background(), lanclient.JoinOptions{Invite: attempt.Invitation, Workspace: f.root, Runtime: attempt.Runtime, SessionID: attempt.SessionID, BindID: attempt.Credentials.BindID, CredentialHash: relay.Digest(attempt.Credentials.Secret), Replace: true})
	if err != nil || pending.Status != "pending" {
		t.Fatalf("private replacement cutover: %+v, %v", pending, err)
	}
	f.server.Close()
	t.Chdir(sessionGitRoot(t))
	if output, err := f.run(t, attempt.SessionID, false, "unbind", "--local-only"); err != nil || !bytes.Contains(output, []byte("local-only")) {
		t.Fatalf("offline cutover detach: %s, %v", output, err)
	}
	meta, err := client.Metadata(context.Background())
	if err != nil || meta.Status != "detached" || meta.BindID != attempt.Credentials.BindID {
		t.Fatalf("wrong cutover identity detached: %+v, %v", meta, err)
	}
	var retired State
	if err := privatefile.ReadJSON(filepath.Join(f.root, ".pairroom", "retired", original.BindID, "state.json"), maxPrivateFileBytes, &retired); err != nil || retired.BindID != original.BindID {
		t.Fatalf("offline cutover discarded the prior workspace: %+v, %v", retired, err)
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := identities.Check(context.Background(), nativeidentity.Claim{Runtime: attempt.Runtime, SessionID: attempt.SessionID, Association: nativeidentity.Remote(fresh.HostPin, fresh.RoomID), BindID: attempt.Credentials.BindID}); !errors.Is(err, nativeidentity.ErrUnowned) {
		t.Fatalf("offline cutover did not release the exact candidate: %v", err)
	}
}

// Repeating the documented offline detach after another binding adopted the same
// (runtime, session) identity must still finish the local cleanup: the record is
// already retired, the replacement claim is not this command's to release, and
// failing an idempotent command would skip the notice and --purge.
func TestLANCLIOfflineDetachConvergesAfterItsIdentityIsReused(t *testing.T) {
	f := newLANCLIWire(t)
	_, original := f.bind(t, "shared-native-session")
	f.server.Close()
	t.Chdir(sessionGitRoot(t))
	if output, err := f.run(t, "shared-native-session", false, "unbind", "--local-only", "--room", original.Room); err != nil || !bytes.Contains(output, []byte("local-only")) {
		t.Fatalf("first offline detach: %s, %v", output, err)
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		t.Fatal(err)
	}
	claim := nativeidentity.Claim{Runtime: original.Runtime, SessionID: "shared-native-session", Association: nativeidentity.Hosted(f.root, "local-room", model.ActorSlot1), BindID: "replacement-binding"}
	if err := identities.Reserve(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if output, err := f.run(t, "shared-native-session", false, "unbind", "--local-only", "--room", original.Room); err != nil || !bytes.Contains(output, []byte("local-only")) {
		t.Fatalf("repeated offline detach: %s, %v", output, err)
	}
	if err := identities.Check(context.Background(), claim); err != nil {
		t.Fatalf("repeated offline detach disturbed the replacement identity: %v", err)
	}
}

// Either private workspace file can independently lose its owner boundary.
// The offline escape retires only the matching protected client identity and
// preserves the original publication WAL in a private archive.
func TestLANCLIOfflineDetachRetiresBindingWithLostOwnerBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
	}{
		{"state-only", []string{"state.json"}},
		{"credentials-only", []string{"credentials"}},
		{"both", []string{"state.json", "credentials"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLANCLIWire(t)
			invite, original := f.bind(t, "damaged-boundary-session")
			slotDir := filepath.Join(f.root, ".pairroom", "rooms", original.Room, "slots", string(original.Slot))
			original.Pending = &Pending{Seq: 7, Text: "@claude original pending reply", Unknown: true}
			original.Held = []Pending{{Seq: 8, Text: "@claude retained next reply"}}
			original.LastSeq = 8
			if err := privatefile.WriteJSON(filepath.Join(slotDir, "state.json"), original); err != nil {
				t.Fatal(err)
			}
			var cred credentials
			if err := privatefile.ReadJSON(filepath.Join(slotDir, "credentials"), maxPrivateFileBytes, &cred); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.files {
				breakOwnerBoundary(t, filepath.Join(slotDir, name))
			}
			t.Chdir(sessionGitRoot(t))
			requests, connections := f.requests.Load(), f.connections.Load()
			// Ordinary commands keep strict file ownership even when this
			// same session is eligible for the explicit offline escape.
			if _, err := f.run(t, original.SessionID, false, "status"); err == nil {
				t.Fatal("ordinary status accepted a damaged owner boundary")
			}
			output, err := f.run(t, original.SessionID, false, "unbind", "--local-only", "--room", original.Room)
			if err != nil || !bytes.Contains(output, []byte("local-only")) || !bytes.Contains(output, []byte("not owner-only")) {
				t.Fatalf("damaged-boundary offline detach: %s, %v", output, err)
			}
			if got := f.requests.Load(); got != requests || f.connections.Load() != connections {
				t.Fatalf("offline retirement contacted the host: requests=%d connections=%d", got-requests, f.connections.Load()-connections)
			}
			for _, name := range []string{"state.json", "credentials"} {
				if _, err := os.Stat(filepath.Join(slotDir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("retired %s survived: %v", name, err)
				}
			}
			archiveDir := filepath.Join(f.root, ".pairroom", "retired", original.BindID)
			var archived State
			if err := privatefile.ReadJSON(filepath.Join(archiveDir, "state.json"), maxPrivateFileBytes, &archived); err != nil || !reflect.DeepEqual(archived, original) {
				t.Fatalf("offline retirement did not privately preserve the complete original WAL: %v", err)
			}
			var archivedCredential credentials
			if err := privatefile.ReadJSON(filepath.Join(archiveDir, "credentials"), maxPrivateFileBytes, &archivedCredential); err != nil || archivedCredential != cred {
				t.Fatalf("offline retirement did not privately preserve the original credential: %v", err)
			}
			clients, err := lanclient.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer clients.Close()
			client, err := clients.Get(context.Background(), original.Room)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := client.Metadata(context.Background())
			if err != nil || meta.Status != "detached" || meta.BindID != original.BindID {
				t.Fatalf("offline retirement did not durably retire the exact admission: %+v, %v", meta, err)
			}
			identities, err := nativeidentity.Open()
			if err != nil {
				t.Fatal(err)
			}
			claim := nativeidentity.Claim{Runtime: original.Runtime, SessionID: original.SessionID, Association: nativeidentity.Remote(invite.HostPin, invite.RoomID), BindID: original.BindID, Generation: original.Generation}
			if err := identities.Check(context.Background(), claim); !errors.Is(err, nativeidentity.ErrUnowned) {
				t.Fatalf("offline retirement did not release its exact native identity: %v", err)
			}
		})
	}
}

// A stale or mislabelled workspace file can still be found from the protected
// client catalog after cwd changes. Lost file permissions must not turn those
// bytes into authority to detach, drop the WAL, or purge the runtime's hooks.
func TestLANCLIOfflineRecoveryRejectsStateThatDiffersFromClientRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*State)
	}{
		{"stale-generation", func(s *State) { s.Generation++ }},
		{"disguised-local-state", func(s *State) { s.Schema, s.LAN = 2, nil }},
		{"different-host-route", func(s *State) { s.LAN.Endpoint = "https://192.168.1.99:8877" }},
		{"different-native-session", func(s *State) { s.SessionID = "another-native-session" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLANCLIWire(t)
			invite, original := f.bind(t, "stale-boundary-session")
			slotDir := filepath.Join(f.root, ".pairroom", "rooms", original.Room, "slots", string(original.Slot))
			statePath := filepath.Join(slotDir, "state.json")
			stale := original
			route := *original.LAN
			stale.LAN = &route
			stale.Pending = &Pending{Seq: 4, Text: "@claude uncertain original publication", Unknown: true}
			stale.Held = []Pending{{Seq: 5, Text: "@claude retained follow-up"}}
			stale.LastSeq = 5
			tc.change(&stale)
			_, localConnections := directTransportLocalTrap(t)
			if stale.LAN == nil {
				endpoint, err := defaultEndpoint()
				if err != nil {
					t.Fatal(err)
				}
				stale.EndpointPath = endpoint
			}
			if err := privatefile.WriteJSON(statePath, stale); err != nil {
				t.Fatal(err)
			}
			if stale.LAN == nil {
				// A LAN directory cannot become a local Service binding even
				// before its otherwise valid state's owner boundary is lost.
				if output, err := f.run(t, original.SessionID, true, "status"); err == nil {
					t.Fatalf("disguised direct state became a local Service binding: %s", output)
				}
				if localConnections.Load() != 0 {
					t.Fatal("disguised direct state contacted the local Service")
				}
			}
			breakOwnerBoundary(t, statePath)
			temporary := filepath.Join(slotDir, ".state.json-interrupted")
			if err := privatefile.WriteJSON(temporary, map[string]string{"pending": "original crash evidence"}); err != nil {
				t.Fatal(err)
			}
			clientRoot, err := lanclient.DefaultRoot()
			if err != nil {
				t.Fatal(err)
			}
			locatorDir, err := locatorDirectory(nativeCaller{runtime: original.Runtime, session: original.SessionID}, false)
			if err != nil {
				t.Fatal(err)
			}
			hooks, err := hookPath(f.root, original.Runtime)
			if err != nil {
				t.Fatal(err)
			}
			before := make(map[string][]byte)
			for _, path := range []string{
				statePath,
				filepath.Join(slotDir, "credentials"),
				filepath.Join(clientRoot, original.Room, "client.json"),
				filepath.Join(f.root, ".pairroom", "lan-joins", original.Room, "join-attempt.json"),
				filepath.Join(locatorDir, locatorFilename(original)),
				hooks,
				temporary,
			} {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				before[path] = data
			}
			t.Chdir(sessionGitRoot(t))
			requests, connections := f.requests.Load(), f.connections.Load()
			output, err := f.run(t, original.SessionID, false, "unbind", "--local-only", "--room", original.Room, "--purge-hooks")
			if err == nil || bytes.Contains(output, []byte("local-only")) {
				t.Fatalf("mismatched recovery was not rejected: %s, %v", output, err)
			}
			if got := f.requests.Load(); got != requests || f.connections.Load() != connections {
				t.Fatalf("mismatched offline recovery contacted the host: requests=%d connections=%d", got-requests, f.connections.Load()-connections)
			}
			if localConnections.Load() != 0 {
				t.Fatal("mismatched offline recovery contacted the local Service")
			}
			for path, want := range before {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("rejected recovery changed %s: %v", filepath.Base(path), err)
				}
			}
			if _, err := os.Stat(filepath.Join(f.root, ".pairroom", "retired", original.BindID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected recovery created a retirement archive: %v", err)
			}
			identities, err := nativeidentity.Open()
			if err != nil {
				t.Fatal(err)
			}
			claim := nativeidentity.Claim{Runtime: original.Runtime, SessionID: original.SessionID, Association: nativeidentity.Remote(invite.HostPin, invite.RoomID), BindID: original.BindID, Generation: original.Generation}
			if err := identities.Check(context.Background(), claim); err != nil {
				t.Fatalf("rejected recovery disturbed the original native identity: %v", err)
			}
		})
	}
}

// Older detach ordering could release the claim before persisting retirement.
// A still-active record must not be reported detached after another binding
// owns that native session, even when its disposable slot state is gone.
func TestLANCLIOfflineDetachDoesNotHideActiveIdentityConflict(t *testing.T) {
	f := newLANCLIWire(t)
	invite, original := f.bind(t, "conflicted-native-session")
	identities, err := nativeidentity.Open()
	if err != nil {
		t.Fatal(err)
	}
	originalClaim := nativeidentity.Claim{Runtime: original.Runtime, SessionID: original.SessionID, Association: nativeidentity.Remote(invite.HostPin, invite.RoomID), BindID: original.BindID, Generation: original.Generation}
	if err := identities.Release(context.Background(), originalClaim); err != nil {
		t.Fatal(err)
	}
	replacement := nativeidentity.Claim{Runtime: original.Runtime, SessionID: original.SessionID, Association: nativeidentity.Hosted(f.root, "replacement-room", model.ActorSlot1), BindID: "newer-binding"}
	if err := identities.Reserve(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	slotDir := filepath.Join(f.root, ".pairroom", "rooms", original.Room, "slots", string(original.Slot))
	if err := os.Remove(filepath.Join(slotDir, "state.json")); err != nil {
		t.Fatal(err)
	}
	clientRoot, err := lanclient.DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	hooks, err := hookPath(f.root, original.Runtime)
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[string][]byte)
	for _, path := range []string{filepath.Join(clientRoot, original.Room, "client.json"), filepath.Join(slotDir, "credentials"), hooks} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = data
	}
	t.Chdir(sessionGitRoot(t))
	requests, connections := f.requests.Load(), f.connections.Load()
	output, err := f.run(t, original.SessionID, false, "unbind", "--local-only", "--room", original.Room, "--purge-hooks")
	if !errors.Is(err, nativeidentity.ErrOwned) || bytes.Contains(output, []byte("local-only")) {
		t.Fatalf("active ownership conflict was reported as retired: %s, %v", output, err)
	}
	if got := f.requests.Load(); got != requests || f.connections.Load() != connections {
		t.Fatalf("conflicted offline detach contacted the host: requests=%d connections=%d", got-requests, f.connections.Load()-connections)
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("failed detach changed %s: %v", filepath.Base(path), err)
		}
	}
	if err := identities.Check(context.Background(), replacement); err != nil {
		t.Fatalf("failed detach disturbed the newer native owner: %v", err)
	}
}

// A joined workspace copied to another machine keeps inherited ACEs, so its
// directories are no longer owner-private either. The documented offline detach
// must still retire it instead of failing on a directory boundary.
func TestLANCLIOfflineDetachRetiresCopiedWorkspace(t *testing.T) {
	f := newLANCLIWire(t)
	invite, _ := f.bind(t, "copied-workspace-session")
	f.server.Close()
	room := lanRoutingID(invite)
	joinDir := filepath.Join(f.root, ".pairroom", "lan-joins", room)
	slotDir := filepath.Join(f.root, ".pairroom", "rooms", room, "slots", "slot2")
	for _, path := range []string{
		filepath.Join(f.root, ".pairroom", "lan-joins"), joinDir, filepath.Join(joinDir, "join-attempt.json"),
		slotDir, filepath.Join(slotDir, "state.json"), filepath.Join(slotDir, "credentials"),
	} {
		breakOwnerBoundary(t, path)
	}
	t.Chdir(sessionGitRoot(t))
	if output, err := f.run(t, "copied-workspace-session", false, "unbind", "--local-only", "--room", room); err != nil || !bytes.Contains(output, []byte("local-only")) {
		t.Fatalf("copied-workspace offline detach: %s, %v", output, err)
	}
	if _, err := os.Stat(filepath.Join(slotDir, "state.json")); !os.IsNotExist(err) {
		t.Fatalf("retired binding files survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(slotDir, "credentials")); !os.IsNotExist(err) {
		t.Fatalf("retired credential file survived: %v", err)
	}
}

// A session that already holds an accepted joined Room must not be told it is
// ready to join another one: preflight has to check the same session
// eligibility that join enforces.
func TestLANCLIPreflightJoinRejectsAnAlreadyJoinedSession(t *testing.T) {
	f := newLANCLIWire(t)
	f.bind(t, "joined-session")
	f.server.Close()
	t.Chdir(sessionGitRoot(t))
	output, err := f.run(t, "joined-session", false, "preflight", "--join")
	if err == nil {
		t.Fatalf("preflight promised a join for an already joined session: %s", output)
	}
	if !bytes.Contains(output, []byte("already belongs to a joined LAN Room")) {
		t.Fatalf("preflight hint did not name the existing association: %s", output)
	}
}
