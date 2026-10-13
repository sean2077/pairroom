package enterpriseservice

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type relayHTTPTestBackend struct {
	*fakeDeviceBackend
	credential                                          sqlstore.BindingCredential
	token                                               [32]byte
	authCalls, snapshotCalls, commitCalls, resolveCalls int
	revokeAtRelease                                     bool
	resolved                                            sqlstore.Result
	resolveErr                                          error
	commits                                             []sqlstore.Result
	commands                                            []sqlstore.RelayCommand
}

func (b *relayHTTPTestBackend) AuthenticateBinding(_ context.Context, hash [32]byte, origin string) (sqlstore.BindingCredential, error) {
	b.authCalls++
	if hash != b.token || origin != serviceOrigin || b.revokeAtRelease && b.authCalls > 1 {
		return sqlstore.BindingCredential{}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	return b.credential, nil
}
func (b *relayHTTPTestBackend) ReadonlySnapshot(_ context.Context, auth sqlstore.AuthContext, room string) (sqlstore.RoomSnapshot, error) {
	b.snapshotCalls++
	if auth != b.credential.Auth || room != b.credential.RoomID {
		panic("lost Binding scope")
	}
	return sqlstore.RoomSnapshot{Revision: uint64(b.snapshotCalls)}, nil
}
func (b *relayHTTPTestBackend) ResolveRelayOperation(_ context.Context, cmd sqlstore.RelayCommand) (sqlstore.Result, error) {
	b.resolveCalls++
	b.commands = append(b.commands, cmd)
	return b.resolved, b.resolveErr
}
func (b *relayHTTPTestBackend) CommitRelayOperation(_ context.Context, cmd sqlstore.RelayCommand) (sqlstore.Result, error) {
	b.commitCalls++
	b.commands = append(b.commands, cmd)
	return b.commits[b.commitCalls-1], nil
}

func newRelayHTTPFixture(t *testing.T) (*Handler, *relayHTTPTestBackend, string) {
	t.Helper()
	secret, err := enterpriseauth.NewBrowserSecret()
	if err != nil {
		t.Fatal(err)
	}
	b := &relayHTTPTestBackend{fakeDeviceBackend: newFakeDeviceBackend(), token: tokenHash(secret), resolved: sqlstore.Result{Outcome: sqlstore.OutcomeNotCommitted}, credential: sqlstore.BindingCredential{Auth: sqlstore.AuthContext{Deployment: "deployment-1", UserID: "user-1", BindingGrantID: "binding-grant"}, DeviceID: "device-1", RoomID: "room-1", BindingID: "binding-1", Generation: 1}}
	transactions, _ := enterpriseauth.NewMemoryStore(8)
	h, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, &fakeProvider{}, transactions, b)
	if err != nil {
		t.Fatal(err)
	}
	return h, b, secret
}

func relayHTTPRequest(intent relay.EnterpriseOperationIntent) any {
	return struct {
		ClientID   string                          `json:"client_id"`
		BindingID  string                          `json:"binding_id"`
		Generation uint64                          `json:"generation"`
		Intent     relay.EnterpriseOperationIntent `json:"intent"`
	}{"operation-1", "binding-1", 1, intent}
}

func relayHTTPClaimResult() sqlstore.Result {
	return sqlstore.Result{Outcome: sqlstore.OutcomeCommitted, Revision: 2, Seq: 3, Relay: &relay.EnterpriseOperationResult{Version: 1, Kind: relay.EnterpriseOperationClaim, Claim: &relay.Claim{ID: "message-1", Receipt: "collector-private-receipt", PeerGeneration: 7, Envelope: "original peer context"}}, Facts: []model.Event{{Data: []byte(`{"receipt":"another-private-receipt"}`)}}}
}

func TestRelayHTTPRecoversOriginalClaimBeforeCurrentSnapshot(t *testing.T) {
	h, b, secret := newRelayHTTPFixture(t)
	b.resolved = relayHTTPClaimResult()
	b.resolved.Outcome = sqlstore.OutcomeReplayed
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/relay", relayHTTPRequest(relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}), secret, nil)
	if w.Code != 200 || b.resolveCalls != 1 || b.snapshotCalls != 0 || b.commitCalls != 0 || b.authCalls != 2 || !strings.Contains(w.Body.String(), `"peer_generation":7`) || strings.Contains(w.Body.String(), "another-private-receipt") || strings.Contains(w.Body.String(), "Facts") || w.Header().Get(DeviceDeploymentHeader) != "deployment-1" {
		t.Fatalf("unsafe claim recovery: %d %s calls=%+v", w.Code, w.Body.String(), b)
	}
}

func TestRelayHTTPRevokedAfterCommitReleasesNoEnvelope(t *testing.T) {
	h, b, secret := newRelayHTTPFixture(t)
	b.commits = []sqlstore.Result{relayHTTPClaimResult()}
	b.revokeAtRelease = true
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/relay", relayHTTPRequest(relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}), secret, nil)
	if w.Code < 400 || b.commitCalls != 1 || b.authCalls != 2 || strings.Contains(w.Body.String(), "collector-private") || strings.Contains(w.Body.String(), "original peer context") {
		t.Fatalf("revoked claim exposed: %d %s", w.Code, w.Body.String())
	}
}

func TestRelayHTTPUnknownAndExplicitResolveNeverRepeatAnEffect(t *testing.T) {
	for _, path := range []string{"/relay", "/relay/resolve"} {
		t.Run(path, func(t *testing.T) {
			h, b, secret := newRelayHTTPFixture(t)
			b.resolved = sqlstore.Result{Outcome: sqlstore.OutcomeUnknown, Class: sqlstore.ClassCommitUncertain}
			b.resolveErr = &sqlstore.KernelError{Outcome: b.resolved.Outcome, Class: b.resolved.Class}
			w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1"+path, relayHTTPRequest(relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}), secret, nil)
			if w.Code != 503 || b.snapshotCalls != 0 || b.commitCalls != 0 {
				t.Fatal("incomplete verification started an effect")
			}
		})
	}
	h, b, secret := newRelayHTTPFixture(t)
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/relay/resolve", relayHTTPRequest(relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}), secret, nil)
	if w.Code != 409 || b.commitCalls != 0 || b.snapshotCalls != 0 {
		t.Fatal("explicit Resolve of absence became a claim")
	}
}

func TestRelayHTTPOnlyProvenRevisionConflictPermitsOneRebuild(t *testing.T) {
	h, b, secret := newRelayHTTPFixture(t)
	message := relay.Message{ID: "sent", From: model.ActorSlot1, To: model.ActorSlot2, FromRuntime: model.RuntimeCodex, FromGeneration: 1, TargetGeneration: 1, Text: "hello", State: "queued", Source: "send", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	b.commits = []sqlstore.Result{{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassRevisionConflict}, {Outcome: sqlstore.OutcomeCommitted, Relay: &relay.EnterpriseOperationResult{Version: 1, Kind: relay.EnterpriseOperationSend, Message: &message}}}
	intent := relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "hello", ExpectedPeerGeneration: 1}
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/relay", relayHTTPRequest(intent), secret, nil)
	if w.Code != 200 || b.commitCalls != 2 || b.snapshotCalls != 2 || b.commands[1].ClientID != b.commands[2].ClientID || b.commands[1].Intent != b.commands[2].Intent {
		t.Fatalf("CAS rebuild changed publication identity: %d %s", w.Code, w.Body.String())
	}
}

func TestRelayHTTPRefusesBrowserOrWrongBindingBeforeAnyOperation(t *testing.T) {
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Cookie", "browser=credential") }, func(r *http.Request) { r.Header.Set("Origin", serviceOrigin) }, func(r *http.Request) { r.Header.Set(DeviceDeploymentHeader, "other-deployment") }} {
		h, b, secret := newRelayHTTPFixture(t)
		w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/relay", relayHTTPRequest(relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}), secret, edit)
		if w.Code < 400 || b.authCalls != 0 || b.resolveCalls != 0 || b.commitCalls != 0 {
			t.Fatal("wrong authority reached Relay")
		}
	}
	h, b, secret := newRelayHTTPFixture(t)
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/other-room/relay", relayHTTPRequest(relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}), secret, nil)
	if w.Code != 403 || b.resolveCalls != 0 || b.commitCalls != 0 {
		t.Fatal("narrow grant crossed Room scope")
	}
}
