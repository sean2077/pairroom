package relayclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/enterpriseclient"
	"github.com/sean2077/pairroom/internal/enterpriseservice"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/service"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
)

type multiScopePGService struct {
	store                           *sqlstore.Store
	admin                           *pgx.Conn
	owner                           sqlstore.AuthContext
	server                          *httptest.Server
	deployment, room, directory, ca string
	requests                        atomic.Int64
}

// Identity verification and native session metadata are synthetic fixture
// inputs. All deployment, login, Device approval, Binding and Relay mutations
// use the real typed APIs; TLS and PostgreSQL are real. No vendor CLI runs.
func newMultiScopePGService(t *testing.T, suffix string) *multiScopePGService {
	t.Helper()
	ctx := t.Context()
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	cfg := sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "multi-scope-" + suffix, RequireSessionAuth: true}
	if err := sqlstore.InitializeDeployment(ctx, cfg, "org-scope"); err != nil {
		t.Fatal(err)
	}
	store, err := sqlstore.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity := enterpriseauth.VerifiedIdentity{ConnectionID: "fixture", Issuer: "https://fixture-idp.example.invalid", Subject: "owner"}
	if _, err := store.InitializeIdentity(ctx, sqlstore.IdentityConfig{Connections: []sqlstore.TrustConnectionConfig{{ConnectionID: identity.ConnectionID, Issuer: identity.Issuer}}, Bootstrap: identity}); err != nil {
		t.Fatal(err)
	}
	token := sha256.Sum256([]byte("fixture-browser-" + suffix))
	login := sqlstore.IdentityLoginCommand{ClientID: "login-owner", Completion: enterpriseauth.Completion{Purpose: enterpriseauth.PurposeLogin, Identity: identity}, Session: sqlstore.BrowserSessionCandidate{SessionID: "owner-session", TokenHash: token, CSRFHash: sha256.Sum256([]byte("csrf" + suffix)), ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)}}
	if _, err := store.AdmitIdentity(ctx, login); err != nil {
		t.Fatal(err)
	}
	browser, err := store.AuthenticateSession(ctx, token)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := model.PrepareEnterpriseRoomIntent("Scope "+suffix, model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	room := "same-server-room"
	if _, err := store.CommitRoomCreate(ctx, sqlstore.RoomCreateCommand{Auth: browser.Auth, RoomID: room, ClientID: "create-room", Intent: intent}); err != nil {
		t.Fatal(err)
	}
	f := &multiScopePGService{store: store, admin: sqltest.Connect(t, dsn, schema), owner: browser.Auth, deployment: cfg.DeploymentID, room: room}
	f.server = httptest.NewUnstartedServer(nil)
	origin := "https://" + f.server.Listener.Addr().String()
	transactions, err := enterpriseauth.NewMemoryStore(8)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := enterpriseservice.New(enterpriseservice.Config{DeploymentID: cfg.DeploymentID, PublicOrigin: origin}, multiScopeUnusedLogin{}, transactions, store)
	if err != nil {
		t.Fatal(err)
	}
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.requests.Add(1); handler.ServeHTTP(w, r) })
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	f.ca = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := enterpriseclient.New(enterpriseclient.Config{Origin: f.server.URL, DeploymentID: f.deployment, CAFile: f.ca})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	f.directory = client.Directory()
	proof, access, refresh := enterpriseFixtureSecret("proof"+suffix), enterpriseFixtureSecret("access"+suffix), enterpriseFixtureSecret("refresh"+suffix)
	code := sha256.Sum256([]byte("approve-code" + suffix))
	enrollment := sqlstore.DeviceEnrollmentCommand{RequestID: "device-request", DeviceID: "scope-device", Label: "same-home workstation", Origin: f.server.URL, CallbackURI: "http://127.0.0.1:49152/callback", ClientState: enterpriseFixtureSecret("state" + suffix), ProofHash: sha256.Sum256([]byte(proof)), Credential: sqlstore.DeviceCredentialCandidate{GrantID: "device-grant", AccessHash: sha256.Sum256([]byte(access)), RefreshHash: sha256.Sum256([]byte(refresh))}}
	if _, err := store.BeginDeviceEnrollment(ctx, enrollment); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideDeviceEnrollment(ctx, sqlstore.DeviceEnrollmentDecision{Auth: browser.Auth, ClientID: "approve-device", RequestID: enrollment.RequestID, Approve: true, CodeHash: code}); err != nil {
		t.Fatal(err)
	}
	completion, err := store.CompleteDeviceEnrollment(ctx, sqlstore.DeviceEnrollmentCompletion{RequestID: enrollment.RequestID, ClientID: "complete-device", Origin: f.server.URL, ProofHash: enrollment.ProofHash, CodeHash: code})
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{"version": 1, "origin": f.server.URL, "deployment_id": f.deployment, "revision": 1, "device_id": enrollment.DeviceID, "user_id": completion.Control.UserID, "proof": proof, "active": map[string]any{"grant_id": enrollment.Credential.GrantID, "access": access, "refresh": refresh, "expires_at": completion.Control.ExpiresAt, "refresh_expires_at": completion.Control.RefreshExpiresAt}}
	if err := privatefile.WriteJSON(filepath.Join(f.directory, "device.json"), state); err != nil {
		t.Fatal(err)
	}
	return f
}

func multiScopeCaller(t *testing.T, kind model.RuntimeKind, session string) {
	t.Helper()
	// Keep one home/config directory. Only the official session metadata changes.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
	if kind == model.RuntimeClaude {
		t.Setenv("CLAUDE_CODE_SESSION_ID", session)
	} else {
		t.Setenv("CODEX_SESSION_ID", session)
	}
}

func multiScopeRun(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	err := Run(t.Context(), args, strings.NewReader(""), &out, &diagnostic)
	return out.Bytes(), err
}

func multiScopeBind(t *testing.T, f *multiScopePGService, root string, kind model.RuntimeKind, session string) State {
	t.Helper()
	multiScopeCaller(t, kind, session)
	args := []string{"bind", "--repo", root, "--enterprise-service", f.server.URL, "--deployment", f.deployment, "--enterprise-ca", f.ca, "--room", f.room, "--slot", "1", "--id", "bind-scope"}
	out, err := multiScopeRun(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		Status    string `json:"status"`
		RequestID string `json:"request_id"`
	}
	if json.Unmarshal(out, &pending) != nil || pending.Status != "awaiting_owner" || pending.RequestID == "" {
		t.Fatal("Binding did not require Owner approval")
	}
	if _, err := f.store.DecideBindingRequest(t.Context(), sqlstore.BindingDecisionCommand{Auth: f.owner, RoomID: f.room, RequestID: pending.RequestID, ClientID: "approve-native", Approve: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := multiScopeRun(t, args...); err != nil {
		t.Fatal(err)
	}
	var state State
	if err := readPrivate(filepath.Join(root, ".pairroom", "rooms", enterpriseRoutingID(f.server.URL, f.deployment, f.room), "slots", "slot1", "state.json"), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestEnterpriseSameHomeLocalAndTwoRemoteSessionsStayPinnedAcrossDefaultChangeAndOutage(t *testing.T) {
	IsolateNativeCaller(t)
	home, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeCodex} {
		if err := editHooks(root, kind, false); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := service.OpenRegistry(t.Context(), service.RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	noSpawn := service.ProvisionerFunc(func(context.Context, service.Project, model.ActorID, service.BindingSpec, string) (service.Binding, func(context.Context) error, error) {
		t.Error("Native spawned a vendor adapter")
		return service.Binding{}, nil, fmt.Errorf("forbidden vendor spawn")
	})
	room, err := registry.ProvisionRoom(t.Context(), service.ProvisionRequest{ProjectID: project.ID, Name: "Local scope", HostMode: model.HostNative}, noSpawn)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := service.NewRuntimeManager(registry, service.EmbeddedRuntimeFactory(registry, service.EmbeddedRuntimeConfig{}), service.RuntimeManagerConfig{Limit: 2, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	management, err := service.NewManagementServer(service.ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: noSpawn, Token: "test-owned-local-management"})
	if err != nil {
		t.Fatal(err)
	}
	local := httptest.NewServer(management.Handler())
	t.Cleanup(local.Close)
	endpoint := filepath.Join(registry.Root(), relay.EndpointFile)
	if err := relay.WriteEndpoint(registry.Root(), relay.Endpoint{URL: local.URL, Token: "test-owned-local-management"}); err != nil {
		t.Fatal(err)
	}
	multiScopeCaller(t, model.RuntimeClaude, "local-native-session")
	if _, err := multiScopeRun(t, "bind", "--repo", root, "--service-file", endpoint, "--room", room.ID, "--slot", "1"); err != nil {
		t.Fatal(err)
	}
	var localState State
	if err := readPrivate(filepath.Join(root, ".pairroom", "rooms", room.ID, "slots", "slot1", "state.json"), &localState); err != nil {
		t.Fatal(err)
	}
	a, b := newMultiScopePGService(t, "a"), newMultiScopePGService(t, "b")
	aState := multiScopeBind(t, a, root, model.RuntimeCodex, "remote-a-native-session")
	bState := multiScopeBind(t, b, root, model.RuntimeClaude, "remote-b-native-session")
	if a.directory == b.directory || aState.Room == bState.Room || aState.Room == localState.Room || aState.Enterprise.DeploymentID == bState.Enterprise.DeploymentID {
		t.Fatal("scope fixtures collided")
	}
	if current, _ := os.UserConfigDir(); current != home {
		t.Fatal("fixture changed home instead of testing coexistence")
	}
	// Switch only the unbound default endpoint; bound Local retains its explicit
	// original endpoint and both enterprise routes retain their HTTPS tuple.
	defaultDir, err := relay.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	// The Local default endpoint accepts only numeric-loopback HTTP. A decoy
	// listener has no Room API and cannot accept any business operation.
	var defaultRequests atomic.Int64
	decoy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultRequests.Add(1)
		http.Error(w, "unbound default", http.StatusServiceUnavailable)
	}))
	t.Cleanup(decoy.Close)
	if err := relay.WriteEndpoint(defaultDir, relay.Endpoint{URL: decoy.URL, Token: "unrelated-default-capability"}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		kind    model.RuntimeKind
		session string
		state   State
		remote  *multiScopePGService
	}{{model.RuntimeClaude, "local-native-session", localState, nil}, {model.RuntimeCodex, "remote-a-native-session", aState, a}, {model.RuntimeClaude, "remote-b-native-session", bState, b}}
	expectedMessages := map[string]int{}
	verify := func(c struct {
		kind    model.RuntimeKind
		session string
		state   State
		remote  *multiScopePGService
	}, id string) {
		t.Helper()
		multiScopeCaller(t, c.kind, c.session)
		if _, err := multiScopeRun(t, "status"); err != nil {
			t.Fatal("pinned status", err)
		}
		if _, err := multiScopeRun(t, "send", "--id", id, "--to", "@user", "--text", "scope "+c.session); err != nil {
			t.Fatal("pinned send", err)
		}
		expectedMessages[c.state.Room]++
		statusBytes, err := multiScopeRun(t, "status")
		if err != nil {
			t.Fatal("post-send status", err)
		}
		var status struct {
			Relay relay.Snapshot `json:"relay"`
		}
		if json.Unmarshal(statusBytes, &status) != nil {
			t.Fatal("invalid public status")
		}
		expectedRoom := c.state.Room
		if c.remote != nil {
			expectedRoom = c.remote.room
		}
		binding := status.Relay.Bindings[model.ActorSlot1]
		if status.Relay.RoomID != expectedRoom || binding.Generation != c.state.Generation || binding.Runtime != c.kind {
			t.Fatalf("public status boundary: room=%s want=%s generation=%d want=%d runtime=%s want=%s", status.Relay.RoomID, expectedRoom, binding.Generation, c.state.Generation, binding.Runtime, c.kind)
		}
		historyBytes, err := multiScopeRun(t, "history")
		if err != nil {
			t.Fatal("pinned history", err)
		}
		var history relay.HistoryPage
		if json.Unmarshal(historyBytes, &history) != nil || history.HasMore || len(history.Messages) != expectedMessages[c.state.Room] || history.Total != expectedMessages[c.state.Room] {
			t.Fatal("public original Room history count changed")
		}
		last := history.Messages[len(history.Messages)-1]
		if last.Text != "scope "+c.session || last.FromRuntime != c.kind || last.FromGeneration != c.state.Generation || last.From != model.ActorSlot1 || last.To != model.ActorUser {
			t.Fatal("publication author/runtime/generation reached a different Room")
		}
		if c.remote != nil {
			var stored int64
			if err := c.remote.admin.QueryRow(t.Context(), `SELECT count(*) FROM room_events WHERE room_id=$1 AND kind=$2`, c.remote.room, relay.EventMessage).Scan(&stored); err != nil || stored != int64(expectedMessages[c.state.Room]) {
				t.Fatal("actual SQL publication count differs from original Room status", err)
			}
		} else {
			events, err := os.ReadFile(filepath.Join(room.DataDir, "events.jsonl"))
			if err != nil || !bytes.Contains(events, []byte("scope "+c.session)) {
				t.Fatal("Local publication absent from original durable event log")
			}
		}
		var persisted State
		if err := readPrivate(filepath.Join(root, ".pairroom", "rooms", c.state.Room, "slots", "slot1", "state.json"), &persisted); err != nil {
			t.Fatal(err)
		}
		if persisted.Room != c.state.Room || persisted.BindID != c.state.BindID || persisted.Generation != c.state.Generation || persisted.SessionID != c.session || persisted.Runtime != c.kind || !reflect.DeepEqual(persisted.Enterprise, c.state.Enterprise) || persisted.EndpointPath != c.state.EndpointPath {
			t.Fatal("default switch retargeted original Binding")
		}
	}
	for i, c := range cases {
		verify(c, fmt.Sprintf("before-outage-%d", i))
	}
	counts := func(f *multiScopePGService) int64 {
		t.Helper()
		var n int64
		if err := f.admin.QueryRow(t.Context(), `SELECT count(*) FROM room_events WHERE room_id=$1`, f.room).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	beforeA, beforeB := counts(a), counts(b)
	bRequests := b.requests.Load()
	localEvents, err := os.ReadFile(filepath.Join(room.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	a.server.Close()
	multiScopeCaller(t, model.RuntimeCodex, "remote-a-native-session")
	if out, err := multiScopeRun(t, "send", "--id", "a-offline", "--to", "@user", "--text", "must stay at A"); err == nil || len(out) != 0 {
		t.Fatal("offline A accepted or emitted a fallback result")
	}
	if counts(a) != beforeA || counts(b) != beforeB || b.requests.Load() != bRequests {
		t.Fatal("offline A touched a different service or wrote an effect")
	}
	afterLocal, err := os.ReadFile(filepath.Join(room.DataDir, "events.jsonl"))
	if err != nil || !bytes.Equal(afterLocal, localEvents) {
		t.Fatal("offline A mutated Local Room")
	}
	verify(cases[0], "local-after-a-outage")
	verify(cases[2], "b-after-a-outage")
	if defaultRequests.Load() != 0 {
		t.Fatal("bound commands contacted changed unbound default endpoint")
	}
	if counts(a) != beforeA || counts(b) != beforeB+1 {
		t.Fatal("survivor publication reached the wrong remote Room")
	}
}

type multiScopeUnusedLogin struct{}

func (multiScopeUnusedLogin) Begin(context.Context, enterpriseauth.TransactionStore, string, enterpriseauth.LoginIntent) (enterpriseauth.Authorization, error) {
	return enterpriseauth.Authorization{}, fmt.Errorf("unexpected browser login in Native scope test")
}
func (multiScopeUnusedLogin) Complete(context.Context, enterpriseauth.TransactionStore, string, string, string) (enterpriseauth.Completion, error) {
	return enterpriseauth.Completion{}, fmt.Errorf("unexpected OIDC callback in Native scope test")
}
