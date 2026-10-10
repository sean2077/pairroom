package relayclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

func directTransportCredentials(t *testing.T, state State) string {
	t.Helper()
	dir := filepath.Join(state.Workspace, ".pairroom", "rooms", state.Room, "slots", string(state.Slot))
	if err := privatefile.WriteJSON(filepath.Join(dir, "credentials"), credentials{BindID: state.BindID, Secret: "local-secret"}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A usable local Service is a stronger fallback trap than a missing endpoint.
// Count connections as well as requests so even a mistaken TLS dial is caught.
func directTransportLocalTrap(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	connections := &atomic.Int32{}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rooms":[],"projects":[]}`))
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)
	endpoint, err := defaultEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.AtomicJSON(endpoint, relay.Endpoint{URL: server.URL, Token: "local-service-must-stay-unused"}); err != nil {
		t.Fatal(err)
	}
	return strings.Replace(server.URL, "http://", "https://", 1), connections
}

func TestDirectTransportLoadsOnlyTheOriginalPrivateRoute(t *testing.T) {
	isolateCaller(t)
	state, direct := directDiscoveryFixture(t)
	dir := directTransportCredentials(t, state)
	trap, connections := directTransportLocalTrap(t)
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	if client.LAN == nil || client.Endpoint.URL != "" || client.State.EndpointPath != "" || client.localAuth() != (relay.Auth{Slot: state.Slot, BindID: state.BindID, Generation: state.Generation, SessionID: state.SessionID, Secret: "local-secret"}) {
		t.Fatal("direct load did not retain the original route and exact local capability")
	}
	if after, err := os.ReadFile(filepath.Join(dir, "state.json")); err != nil || !bytes.Equal(after, before) {
		t.Fatal("loading the direct route rewrote the pending publication WAL")
	}
	// All mutations retain the same BindID and a valid schema. A credential
	// match alone must not authorize a different route or native session.
	for name, change := range map[string]func(*State){
		"workspace":  func(s *State) { s.Workspace = t.TempDir() },
		"runtime":    func(s *State) { s.Runtime = model.RuntimeClaude },
		"session":    func(s *State) { s.SessionID = "another-native-session" },
		"generation": func(s *State) { s.Generation++ },
		"slot":       func(s *State) { s.Slot = model.ActorSlot1 },
		"endpoint":   func(s *State) { s.LAN.Endpoint = trap },
	} {
		t.Run(name, func(t *testing.T) {
			changed, route := state, *state.LAN
			changed.LAN = &route
			change(&changed)
			if !validStateFormat(changed) || changed.BindID != state.BindID {
				t.Fatal("fixture failed to reach the private route comparison")
			}
			if err := privatefile.WriteJSON(filepath.Join(dir, "state.json"), changed); err != nil {
				t.Fatal(err)
			}
			loaded, err := loadLocal(dir)
			if err != nil {
				t.Fatalf("valid local capability did not reach route validation: %v", err)
			}
			defer loaded.close()
			if loaded.LAN != nil || loaded.endpointErr == nil || errors.Is(loaded.endpointErr, errTransportUnavailable) {
				t.Fatal("changed route or identity became a usable transport")
			}
			if err := loaded.call(context.Background(), "summary", nil, &relay.Summary{}); err != loaded.endpointErr {
				t.Fatal("route validation failure was bypassed by a relay call")
			}
		})
	}
	metadata, err := direct.Metadata(context.Background())
	if err != nil || metadata.BindID != state.BindID || metadata.SessionID != state.SessionID || metadata.Invite.Endpoint != state.LAN.Endpoint {
		t.Fatal("mismatched workspace state changed the original private association")
	}
	if connections.Load() != 0 {
		t.Fatal("route loading or rejection contacted the alternate local Service")
	}
}

func TestDirectTransportPreflightKeepsUnavailableHostAndPublicationState(t *testing.T) {
	isolateCaller(t)
	t.Setenv("CODEX_SESSION_ID", "discovery-session")
	state, _ := directDiscoveryFixture(t)
	_, connections := directTransportLocalTrap(t)
	command := exec.Command("git", "-C", state.Workspace, "init", "-q")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	// An admitted client without its original local capability needs repair,
	// even when the private Room key and a healthy local Service both exist.
	missing := preflightLANState(context.Background(), state)
	if missing.Status != checkFail || missing.Transport != "lan_direct" || !strings.Contains(missing.Hint, "private client record") || missing.EndpointPath != "" {
		t.Fatalf("missing capability was not reported as direct binding recovery: %+v", missing)
	}
	dir := directTransportCredentials(t, state)
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	ctx, observed := withServiceObservation(context.Background())
	err = client.call(ctx, "summary", nil, &relay.Summary{})
	if !errors.Is(err, errTransportUnavailable) || strings.Contains(err.Error(), state.LAN.Endpoint) || strings.Contains(err.Error(), "local-secret") {
		t.Fatalf("offline original host was not a sanitized transport failure: %v", err)
	}
	if observed.needsProbe(errNoAssociatedBinding) {
		t.Fatal("direct failure enabled local Service version probing")
	}
	report, _, err := runPreflightJSON(t, options{repo: state.Workspace})
	if !errors.Is(err, errPreflightNotReady) || report.Mode != "lan_direct" || report.Service.Status != checkFail || report.Service.Transport != "lan_direct" || report.Service.EndpointPath != "" || !strings.Contains(report.Service.Hint, "host") {
		t.Fatalf("offline direct preflight retargeted or hid the host failure: %+v, %v", report.Service, err)
	}
	if after, err := os.ReadFile(filepath.Join(dir, "state.json")); err != nil || !bytes.Equal(after, before) {
		t.Fatal("read-only direct preflight changed the unresolved publication identity")
	}
	if connections.Load() != 0 {
		t.Fatal("offline direct call or preflight contacted a local Service")
	}
}

func TestDirectTransportColdPreflightUsesCatalogWithoutRepairingLocators(t *testing.T) {
	isolateCaller(t)
	t.Setenv("CODEX_SESSION_ID", "discovery-session")
	state, direct := directDiscoveryFixture(t)
	dir := directTransportCredentials(t, state)
	_, connections := directTransportLocalTrap(t)
	command := exec.Command("git", "-C", state.Workspace, "init", "-q")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := forgetSession(state); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	caller := nativeCaller{runtime: state.Runtime, session: state.SessionID}
	resolved, err := directRoomWorkspace(ctx, state.Room, caller)
	if err != nil || !sameWorkspace(resolved, state.Workspace) {
		t.Fatalf("explicit direct Room lost its original workspace: %q, %v", resolved, err)
	}
	if _, err := directRoomWorkspace(ctx, state.Room, nativeCaller{runtime: state.Runtime, session: "different-session"}); !errors.Is(err, relay.ErrAuth) {
		t.Fatal("another native session resolved the private association")
	}
	if meta, err := directSessionMetadata(ctx, nativeCaller{}); err != nil || meta != nil {
		t.Fatal("an unassociated caller selected another session's direct client")
	}
	meta, err := direct.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := preflightBoundState(ctx, options{}, "")
	if err != nil || bound == nil || bound.Room != state.Room || bound.Pending == nil || bound.Pending.Text != state.Pending.Text {
		t.Fatalf("cold preflight did not recover the original binding from its client catalog: %+v, %v", bound, err)
	}
	if states, err := indexedSessions(caller); err != nil || len(states) != 0 {
		t.Fatal("read-only preflight repaired the disposable locator")
	}
	other := sessionGitRoot(t)
	for name, options := range map[string]options{
		"room":      {room: "another-room"},
		"slot":      {slot: string(model.ActorSlot1)},
		"service":   {endpoint: filepath.Join(t.TempDir(), relay.EndpointFile)},
		"workspace": {repo: other, repoExplicit: true},
	} {
		t.Run(name, func(t *testing.T) {
			if bound, err := preflightBoundState(ctx, options, ""); err == nil || bound != nil {
				t.Fatal("cold preflight accepted a target different from the original association")
			}
		})
	}
	if _, err := selectDirectWorkspace(ctx, meta, "bind", &options{create: true}); err == nil {
		t.Fatal("an existing direct native session was available for a new local Room")
	}
	if err := os.Remove(filepath.Join(dir, "state.json")); err != nil {
		t.Fatal(err)
	}
	if bound, err := preflightBoundState(ctx, options{}, ""); err == nil || bound != nil || !strings.Contains(err.Error(), "confirmation") {
		t.Fatalf("missing workspace binding was not left for explicit confirmation: %+v, %v", bound, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("preflight recreated a missing publication WAL")
	}
	if connections.Load() != 0 {
		t.Fatal("cold recovery or target rejection contacted a local Service")
	}
}

func TestDirectTransportErrorNormalizationPreservesReceiptDecisions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		input     error
		transport bool
		code      string
	}{
		{name: "successful operation"},
		{name: "no host response", input: fmt.Errorf("request: %w", lanclient.ErrTransportUnavailable), transport: true},
		{name: "network detail is private", input: &url.Error{Op: "Post", URL: "https://127.0.0.1:443/private?secret=fixture", Err: errors.New("private TLS detail")}, transport: true},
		{name: "definite local conflict", input: &lanclient.Error{Status: http.StatusConflict, Code: relay.SendPayloadConflictCode, Message: "publication payload differs"}, code: relay.SendPayloadConflictCode},
		{name: "definite host receipt rejection", input: &lanshare.Error{Status: http.StatusConflict, Code: "receipt_conflict", Message: "receipt does not match"}, code: "receipt_conflict"},
		{name: "canonical publication conflict", input: relay.ErrSendPayloadConflict, code: relay.SendPayloadConflictCode},
		{name: "definite membership rejection", input: relay.ErrAuth},
		{name: "unknown effect is not transport proof", input: relay.ErrUnknown},
		{name: "bounded host failure is definite", input: lanclient.ErrUnavailable},
		{name: "caller canceled", input: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := normalizeLANError("ack", tc.input)
			if (err == nil) != (tc.input == nil) || errors.Is(err, errTransportUnavailable) != tc.transport || relayErrorCode(err) != tc.code {
				t.Fatalf("normalization changed the receipt decision: %v", err)
			}
			if tc.transport && (strings.Contains(err.Error(), "https://") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "TLS detail")) {
				t.Fatal("transport diagnostics exposed private request details")
			}
			var local *lanclient.Error
			var remote *lanshare.Error
			if errors.As(tc.input, &local) || errors.As(tc.input, &remote) {
				message := ""
				if local != nil {
					message = local.Message
				} else {
					message = remote.Message
				}
				var definite *relayError
				if !errors.As(err, &definite) || definite.action != "ack" || definite.message != message {
					t.Fatal("definite rejection lost its action or stable description")
				}
			} else if !tc.transport && !errors.Is(err, tc.input) {
				t.Fatal("a settled local decision was replaced by a retryable failure")
			}
		})
	}
}
