package enterpriseclient

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type wireFixture struct {
	t                    *testing.T
	server               *httptest.Server
	mu                   sync.Mutex
	client               *Client
	begin                beginRequest
	code                 secret
	rotation             rotationRequest
	refresh              string
	drop                 string
	postCount, authCount int
	rotationEntered      chan struct{}
	releaseRotation      chan struct{}
	deployment           string
	redirect             string
	postRedirect         string
}

func newWireFixture(t *testing.T) *wireFixture {
	t.Helper()
	f := &wireFixture{t: t, deployment: "dep-test"}
	f.code, _ = newSecret()
	f.server = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if e := os.WriteFile(ca, cert, 0o600); e != nil {
		t.Fatal(e)
	}
	var e error
	f.client, e = New(Config{Origin: f.server.URL, DeploymentID: "dep-test", Directory: filepath.Join(t.TempDir(), "private"), CAFile: ca, OpenBrowser: f.openBrowser})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(f.client.Close)
	return f
}
func (f *wireFixture) openBrowser(location string) error {
	f.mu.Lock()
	b := f.begin
	f.mu.Unlock()
	if location != f.server.URL+"/enterprise/device-enrollments/"+b.RequestID {
		return errors.New("untrusted browser destination")
	}
	go func() {
		response, e := http.Get(callbackLocation(b.CallbackURI, b.ClientState, f.code))
		if e != nil {
			f.t.Error("callback failed")
			return
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, response.Body)
		if response.StatusCode != 200 {
			f.t.Errorf("callback status %d", response.StatusCode)
		}
	}()
	return nil
}
func (f *wireFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(DeploymentHeader, "dep-test")
	if r.URL.Path == devicePath+"/session" {
		s, e := f.client.read()
		if e != nil || s.Active == nil || r.Header.Get("Authorization") != "Bearer "+string(s.Active.Access) {
			http.Error(w, "invalid", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"deployment_id": "dep-test", "origin": f.server.URL, "user_id": s.UserID, "device_id": s.DeviceID, "grant_id": s.Active.GrantID, "expires_at": s.Active.ExpiresAt, "refresh_expires_at": s.Active.RefreshExpiresAt})
		return
	}
	if r.URL.Path == devicePath+"/deployment" {
		f.mu.Lock()
		deployment, redirect := f.deployment, f.redirect
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "" {
			f.t.Error("credential sent in deployment preflight")
		}
		if redirect != "" {
			http.Redirect(w, r, redirect, http.StatusFound)
			return
		}
		w.Header().Set(DeploymentHeader, deployment)
		_ = json.NewEncoder(w).Encode(map[string]string{"deployment_id": deployment, "origin": f.server.URL})
		return
	}
	f.mu.Lock()
	f.postCount++
	if r.Header.Get("Authorization") != "" {
		f.authCount++
	}
	drop := f.drop
	postRedirect := f.postRedirect
	f.mu.Unlock()
	if postRedirect != "" {
		http.Redirect(w, r, postRedirect, http.StatusFound)
		return
	}
	if r.Method != "POST" || r.Header.Get(DeploymentHeader) != "dep-test" || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cookie") != "" || r.Header.Get("Origin") != "" {
		f.t.Error("native request envelope invalid")
		http.Error(w, "invalid", 400)
		return
	}
	s, e := f.client.read()
	if e != nil || s.Pending == nil {
		f.t.Error("effect was sent before private journal persisted")
		http.Error(w, "not persisted", 500)
		return
	}
	expiry, refreshExpiry := time.Now().UTC().Add(15*time.Minute), time.Now().UTC().Add(30*24*time.Hour)
	response := operationResponse{Outcome: "committed", DeviceID: s.Pending.DeviceID, UserID: "user-test", GrantID: s.Pending.Next.GrantID, ExpiresAt: expiry, RefreshExpiresAt: refreshExpiry}
	switch r.URL.Path {
	case devicePath + "/enrollments":
		var b beginRequest
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			f.t.Error("invalid begin")
		}
		if b.ProofHash != verifier(s.Pending.Proof) || b.Credential != s.Pending.Next.candidate() || b.CallbackURI != s.Pending.CallbackURI {
			f.t.Error("begin differs from durable candidates")
		}
		f.mu.Lock()
		f.begin = b
		f.mu.Unlock()
		response = operationResponse{Outcome: "committed", Kind: "device.enrollment.begin", RequestID: b.RequestID, DeviceID: b.DeviceID, Status: "pending", ExpiresAt: time.Now().Add(10 * time.Minute), VerificationURL: f.server.URL + "/enterprise/device-enrollments/" + b.RequestID}
		if drop == "begin" {
			closeResponse(w)
			return
		}
	case devicePath + "/credentials/rotate", devicePath + "/credentials/recover":
		var command rotationRequest
		if json.NewDecoder(r.Body).Decode(&command) != nil {
			f.t.Error("invalid rotation")
		}
		if command.Next != s.Pending.Next.candidate() || command.ClientProof != s.Pending.Proof || r.Header.Get("Authorization") != "Bearer "+string(s.Pending.CurrentRefresh) {
			f.t.Error("rotation credentials changed after durable save")
		}
		f.mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/rotate") {
			f.rotation = command
			f.refresh = r.Header.Get("Authorization")
		} else if command != f.rotation || r.Header.Get("Authorization") != f.refresh {
			f.t.Error("recovery changed original wire intent")
		}
		entered, release := f.rotationEntered, f.releaseRotation
		f.mu.Unlock()
		if entered != nil {
			close(entered)
			<-release
		}
		response.Kind = "device.rotate"
		if strings.HasSuffix(r.URL.Path, "/recover") {
			response.Kind = "device.recover"
		}
		if drop == "rotate" && strings.HasSuffix(r.URL.Path, "/rotate") {
			closeResponse(w)
			return
		}
	default:
		if !strings.HasSuffix(r.URL.Path, "/complete") {
			http.NotFound(w, r)
			return
		}
		var c completeRequest
		if json.NewDecoder(r.Body).Decode(&c) != nil {
			f.t.Error("invalid completion")
		}
		if c.ClientID != s.Pending.ClientID || c.ClientProof != s.Pending.Proof || c.CompletionCode != s.Pending.CompletionCode || s.Pending.CompletionHash != s.Pending.completionHash() {
			f.t.Error("completion sent before its code was durable")
		}
		response.Kind, response.RequestID, response.Status = "device.enrollment.complete", s.Pending.RequestID, "completed"
		if drop == "complete" {
			closeResponse(w)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(response)
}
func closeResponse(w http.ResponseWriter) {
	conn, _, e := w.(http.Hijacker).Hijack()
	if e == nil {
		_ = conn.Close()
	}
}
func (f *wireFixture) login(t *testing.T) {
	t.Helper()
	s, e := f.client.Login(context.Background(), "Endpoint", false)
	if e != nil || s.State != "active" || s.UserID != "user-test" {
		t.Fatalf("login: %+v %v", s, e)
	}
}

func TestHTTPSLoginPersistsCandidatesBeforeEffectsAndPublishesOnlyMetadata(t *testing.T) {
	f := newWireFixture(t)
	f.login(t)
	s, e := f.client.read()
	if e != nil || s.Active == nil || s.Pending != nil {
		t.Fatalf("private activation: %v", e)
	}
	public, e := f.client.Status()
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(public)
	for _, value := range []secret{s.Active.Access, s.Active.Refresh, s.Proof} {
		if strings.Contains(string(encoded), string(value)) {
			t.Fatal("status exposed credential")
		}
	}
	if strings.Contains(string(encoded), "callback") || strings.Contains(string(encoded), "session") {
		t.Fatal("status includes endpoint-private identity")
	}
	if _, e = f.client.Login(context.Background(), "Second", false); !errors.Is(e, ErrAlreadyLoggedIn) {
		t.Fatal("login silently replaced an active device")
	}
}
func TestLostCompletionResponsePreservesExactCodeAndRequiresExplicitRecovery(t *testing.T) {
	f := newWireFixture(t)
	f.mu.Lock()
	f.drop = "complete"
	f.mu.Unlock()
	status, e := f.client.Login(context.Background(), "Endpoint", false)
	var operation *Error
	if !errors.As(e, &operation) || operation.Outcome != "unknown" || status.State != "pending_completion_unknown" {
		t.Fatalf("lost response: %+v %v", status, e)
	}
	before, e := f.client.read()
	if e != nil || before.Active != nil || before.Pending == nil || before.Pending.CompletionCode == "" {
		t.Fatal("unknown completion lost pending proof/code")
	}
	f.mu.Lock()
	count := f.postCount
	f.mu.Unlock()
	if _, e = f.client.Login(context.Background(), "Endpoint", false); !errors.Is(e, ErrPending) {
		t.Fatal("ordinary login retried uncertain effect")
	}
	f.mu.Lock()
	if f.postCount != count {
		t.Fatal("pending normal login sent another effect")
	}
	f.drop = ""
	f.mu.Unlock()
	status, e = f.client.Login(context.Background(), "", true)
	if e != nil || status.State != "active" {
		t.Fatalf("explicit completion recovery: %+v %v", status, e)
	}
	after, _ := f.client.read()
	if after.Active.GrantID != before.Pending.Next.GrantID || after.Active.Access != before.Pending.Next.Access || after.Proof != before.Pending.Proof {
		t.Fatal("recovery minted new credentials")
	}
}
func TestLostRefreshResponseRetainsOriginalPredecessorAndOneCandidate(t *testing.T) {
	f := newWireFixture(t)
	f.login(t)
	initial, _ := f.client.read()
	f.mu.Lock()
	f.drop = "rotate"
	f.mu.Unlock()
	status, e := f.client.Refresh(context.Background(), RefreshNew)
	var operation *Error
	if !errors.As(e, &operation) || operation.Outcome != "unknown" || status.State != "pending_rotation_unknown" {
		t.Fatalf("rotation unknown: %+v %v", status, e)
	}
	saved, _ := f.client.read()
	if saved.Active.GrantID != initial.Active.GrantID || saved.Pending.CurrentRefresh != initial.Active.Refresh || saved.Pending.Next.GrantID == initial.Active.GrantID {
		t.Fatal("lost rotation replaced active or predecessor state")
	}
	if _, e = f.client.Refresh(context.Background(), RefreshNew); !errors.Is(e, ErrPending) {
		t.Fatal("normal refresh retried uncertainty")
	}
	if _, e = f.client.Refresh(context.Background(), RefreshRetry); !errors.Is(e, ErrPending) {
		t.Fatal("explicit retry bypassed absence proof")
	}
	f.mu.Lock()
	f.drop = ""
	f.mu.Unlock()
	status, e = f.client.Refresh(context.Background(), RefreshRecover)
	if e != nil || status.State != "active" {
		t.Fatalf("explicit refresh recovery: %+v %v", status, e)
	}
	current, _ := f.client.read()
	if current.Active.Access != saved.Pending.Next.Access || current.Active.Refresh != saved.Pending.Next.Refresh || current.Pending != nil {
		t.Fatal("confirmed rotation changed frozen next secrets")
	}
}
func TestDeploymentMismatchAndRedirectTransmitNoAuthorization(t *testing.T) {
	for _, which := range []string{"deployment", "redirect"} {
		t.Run(which, func(t *testing.T) {
			f := newWireFixture(t)
			f.login(t)
			var trapCalls atomic.Int32
			trap := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { trapCalls.Add(1) }))
			defer trap.Close()
			f.mu.Lock()
			before := f.postCount
			if which == "deployment" {
				f.deployment = "another-deployment"
			} else {
				f.redirect = trap.URL
			}
			f.mu.Unlock()
			if _, e := f.client.Refresh(context.Background(), RefreshNew); e == nil {
				t.Fatal("untrusted preflight accepted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.postCount != before || f.authCount != 0 || trapCalls.Load() != 0 {
				t.Fatalf("credential request escaped pin: posts=%d auth=%d redirected=%d", f.postCount-before, f.authCount, trapCalls.Load())
			}
		})
	}
}
func TestStateIsBoundToOriginAndDeploymentBeforeAnyRequest(t *testing.T) {
	f := newWireFixture(t)
	f.login(t)
	for _, mutate := range []func(*Client){func(c *Client) { c.origin = "https://other.example.test" }, func(c *Client) { c.deployment = "other-deployment" }} {
		other := *f.client
		mutate(&other)
		if _, e := other.Refresh(context.Background(), RefreshNew); !errors.Is(e, ErrPrivateState) {
			t.Fatalf("reused authority in another scope: %v", e)
		}
	}
	for _, origin := range []string{"http://127.0.0.1", "https://example.test/path", "https://example.test?", "https://user@example.test", "https://EXAMPLE.test", "https://:443"} {
		if _, e := New(Config{Origin: origin, DeploymentID: "dep-test", Directory: filepath.Join(t.TempDir(), "never-created")}); e == nil {
			t.Fatal("invalid service origin accepted")
		}
	}
}
func TestLocalLogoutRetainsUncertainJournalAndDoesNotRevokeServer(t *testing.T) {
	f := newWireFixture(t)
	f.login(t)
	f.mu.Lock()
	f.drop = "rotate"
	f.mu.Unlock()
	_, _ = f.client.Refresh(context.Background(), RefreshNew)
	before, _ := f.client.read()
	f.mu.Lock()
	calls := f.postCount
	f.mu.Unlock()
	status, e := f.client.Logout(context.Background())
	if e != nil || status.State != "logged_out_pending_reconciliation" {
		t.Fatalf("local logout: %+v %v", status, e)
	}
	after, _ := f.client.read()
	if after.Active != nil || after.Proof != "" || after.Pending == nil || after.Pending.IntentHash != before.Pending.IntentHash || after.Pending.CurrentRefresh != before.Pending.CurrentRefresh {
		t.Fatal("logout lost pending journal or preserved discovery credential")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postCount != calls {
		t.Fatal("local logout pretended server revocation")
	}
}
func TestConcurrentClientsCannotReplacePendingRotation(t *testing.T) {
	f := newWireFixture(t)
	f.login(t)
	f.rotationEntered = make(chan struct{})
	f.releaseRotation = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, e := f.client.Refresh(context.Background(), RefreshNew); done <- e }()
	<-f.rotationEntered
	other := *f.client
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, e := other.Refresh(ctx, RefreshNew); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("concurrent client bypassed private lock: %v", e)
	}
	close(f.releaseRotation)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	s, _ := f.client.read()
	if s.Pending != nil || s.Active == nil {
		t.Fatal("concurrent client overwrote accepted state")
	}
}
func TestCorruptFrozenPendingIntentNeverReachesNetwork(t *testing.T) {
	f := newWireFixture(t)
	f.login(t)
	f.mu.Lock()
	f.drop = "rotate"
	f.mu.Unlock()
	_, _ = f.client.Refresh(context.Background(), RefreshNew)
	s, _ := f.client.read()
	s.Pending.Next.Access = secret(strings.Repeat("A", 43))
	if e := f.client.save(&s); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	before := f.postCount
	f.mu.Unlock()
	if _, e := f.client.Refresh(context.Background(), RefreshRecover); !errors.Is(e, ErrPrivateState) {
		t.Fatal("corrupt pending hash accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postCount != before {
		t.Fatal("corrupt operation reached network")
	}
}
func TestVerifierUsesExactCanonicalSecretString(t *testing.T) {
	s := secret("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if verifier(s) != "DwBzhbb51LfusnSGBa_hqYSgo7-j8BTQnip4TOnlzRo" {
		t.Fatal("verifier confused secret and digest bytes")
	}
}

func TestExplicitLogoutThenNewLoginRetainsOriginalUnknownOperation(t *testing.T) {
	f := newWireFixture(t)
	f.mu.Lock()
	f.drop = "complete"
	f.mu.Unlock()
	_, _ = f.client.Login(context.Background(), "First", false)
	before, e := f.client.read()
	if e != nil || before.Pending == nil {
		t.Fatal("unknown enrollment missing")
	}
	original := *before.Pending
	if _, e = f.client.Logout(context.Background()); e != nil {
		t.Fatal(e)
	}
	loggedOut, _ := f.client.read()
	if loggedOut.LogoutAt.IsZero() {
		t.Fatal("explicit logout decision not durable")
	}
	f.mu.Lock()
	f.drop = ""
	f.mu.Unlock()
	status, e := f.client.Login(context.Background(), "Fresh endpoint", false)
	if e != nil || status.State != "active" || status.RetainedCount != 1 {
		t.Fatalf("fresh login after logout: %+v %v", status, e)
	}
	after, _ := f.client.read()
	if after.DeviceID == original.DeviceID || after.Active.GrantID == original.Next.GrantID || len(after.Retained) != 1 || after.Retained[0].Operation != original || !after.Retained[0].LogoutAt.Equal(loggedOut.LogoutAt) {
		t.Fatal("new login reused identity or changed original private evidence")
	}
	encoded, _ := json.Marshal(status)
	for _, value := range []secret{original.Proof, original.Next.Access, original.Next.Refresh, original.CompletionCode} {
		if strings.Contains(string(encoded), string(value)) {
			t.Fatal("retained metadata exposed credential")
		}
	}
	if len(status.RetainedOperations) != 1 || status.RetainedOperations[0].OperationID != original.ClientID || status.RetainedOperations[0].IntentHash != original.IntentHash || status.Notice == "" {
		t.Fatal("retained operation cannot be located from safe metadata")
	}
}
func TestRetainedJournalLimitRefusesNewLoginWithoutDiscardingEvidence(t *testing.T) {
	f := newWireFixture(t)
	f.mu.Lock()
	f.drop = "complete"
	f.mu.Unlock()
	_, _ = f.client.Login(context.Background(), "First", false)
	if _, e := f.client.Logout(context.Background()); e != nil {
		t.Fatal(e)
	}
	s, _ := f.client.read()
	for i := 0; i < maxRetainedOperations; i++ {
		p := *s.Pending
		p.ClientID = fmt.Sprintf("retained-%d", i)
		p.IntentHash = p.intentHash(s.Origin, s.DeploymentID)
		p.CompletionHash = p.completionHash()
		s.Retained = append(s.Retained, retainedOperation{Origin: s.Origin, DeploymentID: s.DeploymentID, RetainedAt: time.Now().UTC(), LogoutAt: s.LogoutAt, Resolution: "unresolved", Operation: p})
	}
	if e := f.client.save(&s); e != nil {
		t.Fatal(e)
	}
	before, _ := f.client.read()
	f.mu.Lock()
	calls := f.postCount
	f.mu.Unlock()
	if _, e := f.client.Login(context.Background(), "New", false); !errors.Is(e, ErrRetainedLimit) {
		t.Fatalf("unbounded retained journal accepted: %v", e)
	}
	after, _ := f.client.read()
	if len(after.Retained) != maxRetainedOperations || after.Pending.IntentHash != before.Pending.IntentHash || after.Revision != before.Revision {
		t.Fatal("limit failure changed original evidence")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.postCount != calls {
		t.Fatal("limit failure sent a new enrollment")
	}
}

func TestArbitraryPeerErrorCannotPrintCredentialMaterial(t *testing.T) {
	value, _ := newSecret()
	if safeClass(string(value)) != "operation_unconfirmed" || safeClass("rotation_recovery_unavailable") != "rotation_recovery_unavailable" {
		t.Fatal("peer error crossed fixed public class vocabulary")
	}
}

func TestCredentialPostRedirectIsNeverForwarded(t *testing.T) {
	f := newWireFixture(t)
	f.login(t)
	var calls atomic.Int32
	trap := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer trap.Close()
	f.mu.Lock()
	f.postRedirect = trap.URL
	f.mu.Unlock()
	status, e := f.client.Refresh(context.Background(), RefreshNew)
	var operation *Error
	if !errors.As(e, &operation) || operation.Outcome != "unknown" || status.State != "pending_rotation_unknown" || calls.Load() != 0 {
		t.Fatalf("POST redirected authority: %+v %v calls=%d", status, e, calls.Load())
	}
}
