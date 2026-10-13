package enterpriseservice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type fakeDeviceBackend struct {
	*fakeBackend
	resolveRevokeCalls                                                                                            int
	beginCalls, completeCalls, rotateCalls, recoverCalls, agentCalls, decisionCalls, readCalls, revokeDeviceCalls int
	beginCommand                                                                                                  sqlstore.DeviceEnrollmentCommand
	completeCommand                                                                                               sqlstore.DeviceEnrollmentCompletion
	rotationCommand                                                                                               sqlstore.DeviceRotationCommand
	decisionCommand                                                                                               sqlstore.DeviceEnrollmentDecision
	deviceCredential                                                                                              sqlstore.DeviceCredential
	deviceAuthHash                                                                                                [32]byte
	deviceError                                                                                                   error
	deviceOutcome                                                                                                 sqlstore.Outcome
	info                                                                                                          sqlstore.DeviceEnrollmentInfo
}

func newFakeDeviceBackend() *fakeDeviceBackend {
	return &fakeDeviceBackend{fakeBackend: newFakeBackend(), deviceOutcome: sqlstore.OutcomeCommitted}
}
func (b *fakeDeviceBackend) deviceResult(kind string) *sqlstore.ControlReceipt {
	return &sqlstore.ControlReceipt{Version: 1, Kind: kind, RequestID: b.info.RequestID, DeviceID: b.info.DeviceID, UserID: "user-1", Status: b.info.Status, GrantID: b.beginCommand.Credential.GrantID, ExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(24 * time.Hour)}
}
func (b *fakeDeviceBackend) BeginDeviceEnrollment(_ context.Context, cmd sqlstore.DeviceEnrollmentCommand) (sqlstore.Result, error) {
	b.beginCalls++
	b.beginCommand = cmd
	b.info = sqlstore.DeviceEnrollmentInfo{RequestID: cmd.RequestID, DeviceID: cmd.DeviceID, Label: cmd.Label, Origin: cmd.Origin, CallbackURI: cmd.CallbackURI, ClientState: cmd.ClientState, Status: "pending", ExpiresAt: time.Now().Add(10 * time.Minute)}
	return sqlstore.Result{Outcome: b.deviceOutcome, Control: b.deviceResult(sqlstore.ControlDeviceEnrollment)}, b.deviceError
}
func (b *fakeDeviceBackend) GetDeviceEnrollment(_ context.Context, auth sqlstore.AuthContext, id string) (sqlstore.DeviceEnrollmentInfo, error) {
	b.readCalls++
	b.lastAuth = auth
	if id != b.info.RequestID {
		return sqlstore.DeviceEnrollmentInfo{}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	return b.info, b.deviceError
}
func (b *fakeDeviceBackend) DecideDeviceEnrollment(_ context.Context, cmd sqlstore.DeviceEnrollmentDecision) (sqlstore.Result, error) {
	b.decisionCalls++
	b.decisionCommand = cmd
	b.lastAuth = cmd.Auth
	b.info.Status = "denied"
	if cmd.Approve {
		b.info.Status = "approved"
	}
	return sqlstore.Result{Outcome: b.deviceOutcome, Control: b.deviceResult(sqlstore.ControlDeviceDecision)}, b.deviceError
}
func (b *fakeDeviceBackend) CompleteDeviceEnrollment(_ context.Context, cmd sqlstore.DeviceEnrollmentCompletion) (sqlstore.Result, error) {
	b.completeCalls++
	b.completeCommand = cmd
	if cmd.ProofHash != b.beginCommand.ProofHash || cmd.CodeHash != b.decisionCommand.CodeHash || b.info.Status != "approved" {
		return sqlstore.Result{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	b.info.Status = "completed"
	return sqlstore.Result{Outcome: b.deviceOutcome, Control: b.deviceResult(sqlstore.ControlDeviceComplete)}, b.deviceError
}
func (b *fakeDeviceBackend) RotateDeviceCredential(_ context.Context, cmd sqlstore.DeviceRotationCommand) (sqlstore.Result, error) {
	b.rotateCalls++
	b.rotationCommand = cmd
	receipt := b.deviceResult(sqlstore.ControlDeviceRotate)
	receipt.DeviceID = cmd.DeviceID
	receipt.GrantID = cmd.Next.GrantID
	return sqlstore.Result{Outcome: b.deviceOutcome, Control: receipt}, b.deviceError
}
func (b *fakeDeviceBackend) RecoverDeviceRotation(_ context.Context, cmd sqlstore.DeviceRotationCommand) (sqlstore.Result, error) {
	b.recoverCalls++
	b.rotationCommand = cmd
	receipt := b.deviceResult(sqlstore.ControlDeviceRecover)
	receipt.DeviceID = cmd.DeviceID
	receipt.GrantID = cmd.Next.GrantID
	return sqlstore.Result{Outcome: b.deviceOutcome, Control: receipt}, b.deviceError
}
func (b *fakeDeviceBackend) AuthenticateDevice(_ context.Context, hash [32]byte, origin string) (sqlstore.DeviceCredential, error) {
	b.deviceAuthHash = hash
	if origin != serviceOrigin {
		return sqlstore.DeviceCredential{}, errors.New("wrong origin")
	}
	return b.deviceCredential, b.deviceError
}
func (b *fakeDeviceBackend) RegisterAgent(_ context.Context, auth sqlstore.AuthContext, _, _ string, _ model.RuntimeKind) (sqlstore.Result, error) {
	b.agentCalls++
	b.lastAuth = auth
	receipt := b.deviceResult(sqlstore.ControlAgentRegister)
	receipt.AgentID = "agent-1"
	return sqlstore.Result{Outcome: b.deviceOutcome, Control: receipt}, b.deviceError
}
func (b *fakeDeviceBackend) RevokeDevice(_ context.Context, auth sqlstore.AuthContext, _, deviceID string) (sqlstore.Result, error) {
	b.revokeDeviceCalls++
	b.lastAuth = auth
	receipt := b.deviceResult(sqlstore.ControlDeviceRevoke)
	receipt.DeviceID = deviceID
	return sqlstore.Result{Outcome: b.deviceOutcome, Control: receipt}, b.deviceError
}

func (b *fakeDeviceBackend) ResolveDeviceRevocation(_ context.Context, auth sqlstore.AuthContext, _, deviceID string) (sqlstore.Result, error) {
	b.resolveRevokeCalls++
	b.lastAuth = auth
	receipt := b.deviceResult(sqlstore.ControlDeviceRevoke)
	receipt.DeviceID = deviceID
	return sqlstore.Result{Outcome: b.deviceOutcome, Control: receipt}, b.deviceError
}

func newDeviceFixture(t *testing.T) (*Handler, *fakeDeviceBackend) {
	t.Helper()
	_, provider, _ := newHandlerFixture(t)
	store, _ := enterpriseauth.NewMemoryStore(8)
	backend := newFakeDeviceBackend()
	h, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, provider, store, backend)
	if err != nil {
		t.Fatal(err)
	}
	return h, backend
}

func candidateRequest(t *testing.T, id string) deviceCandidateRequest {
	t.Helper()
	access, _ := enterpriseauth.NewBrowserSecret()
	refresh, _ := enterpriseauth.NewBrowserSecret()
	a, r := tokenHash(access), tokenHash(refresh)
	return deviceCandidateRequest{GrantID: id, AccessHash: base64.RawURLEncoding.EncodeToString(a[:]), RefreshHash: base64.RawURLEncoding.EncodeToString(r[:])}
}
func enrollmentRequest(t *testing.T) (deviceBeginRequest, string) {
	t.Helper()
	proof, _ := enterpriseauth.NewBrowserSecret()
	state, _ := enterpriseauth.NewBrowserSecret()
	hash := tokenHash(proof)
	return deviceBeginRequest{RequestID: "request-1", DeviceID: "device-1", Label: "My CLI", CallbackURI: "http://127.0.0.1:49152/pairroom/callback", ClientState: privateDeviceInput(state), ProofHash: base64.RawURLEncoding.EncodeToString(hash[:]), Credential: candidateRequest(t, "grant-1")}, proof
}
func nativeRequest(t *testing.T, h *Handler, path string, value any, bearer string, edit func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, serviceOrigin+path, strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(DeviceDeploymentHeader, "deployment-1")
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	if edit != nil {
		edit(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDeviceHTTPEnrollmentRequiresExplicitBrowserDecisionAndOriginalClientProof(t *testing.T) {
	h, b := newDeviceFixture(t)
	begin, proof := enrollmentRequest(t)
	w := nativeRequest(t, h, "/enterprise/device/v1/enrollments", begin, "", nil)
	if w.Code != 200 || b.beginCommand.Origin != serviceOrigin || b.beginCommand.ProofHash != tokenHash(proof) || strings.Contains(w.Body.String(), proof) || w.Header().Get("Location") != "" || len(w.Result().Cookies()) != 0 {
		t.Fatalf("unsafe begin: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), serviceOrigin+"/enterprise/device-enrollments/request-1") {
		t.Fatal("begin omitted request-only browser verification URL")
	}
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	w = request(t, h, http.MethodGet, "/enterprise/api/v1/device-enrollments/request-1", "", cookie, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), string(begin.ClientState)) || strings.Contains(w.Body.String(), "callback_uri") || strings.Contains(w.Body.String(), begin.ProofHash) {
		t.Fatal("browser metadata disclosed private transport state")
	}
	w = request(t, h, http.MethodPost, "/enterprise/api/v1/device-enrollments/request-1/decision", `{"client_id":"approve-1","action":"approve"}`, cookie, csrf)
	if w.Code != 200 || b.decisionCalls != 1 || b.lastAuth.SessionID != b.lastCandidate.SessionID {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	var reply struct {
		CompletionLocation string `json:"completion_location"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(reply.CompletionLocation)
	if err != nil {
		t.Fatal(err)
	}
	code := u.Query().Get("completion_code")
	if u.Host != "127.0.0.1:49152" || u.Path != "/pairroom/callback" || u.Query().Get("state") != string(begin.ClientState) || tokenHash(code) != b.decisionCommand.CodeHash {
		t.Fatal("approval changed frozen callback or code verifier")
	}
	w = nativeRequest(t, h, "/enterprise/device/v1/enrollments/request-1/complete", deviceCompleteRequest{ClientID: "complete-1", ClientProof: privateDeviceInput(begin.ProofHash), CompletionCode: privateDeviceInput(code)}, "", nil)
	if w.Code != 403 {
		t.Fatal("public proof hash was accepted as the client's original proof")
	}
	w = nativeRequest(t, h, "/enterprise/device/v1/enrollments/request-1/complete", deviceCompleteRequest{ClientID: "complete-1", ClientProof: privateDeviceInput(proof), CompletionCode: privateDeviceInput(code)}, "", nil)
	if w.Code != 200 || b.completeCommand.Origin != serviceOrigin || strings.Contains(w.Body.String(), proof) || strings.Contains(w.Body.String(), code) || w.Header().Get("Location") != "" {
		t.Fatalf("unsafe completion: %d %s", w.Code, w.Body.String())
	}
}

func TestDeviceHTTPRejectsBrowserCookiesWrongDeploymentAndCallerOriginBeforeSQL(t *testing.T) {
	h, b := newDeviceFixture(t)
	begin, _ := enrollmentRequest(t)
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Cookie", "session=private") }, func(r *http.Request) { r.Header.Set("Origin", serviceOrigin) }, func(r *http.Request) { r.Header.Set(DeviceDeploymentHeader, "other-deployment") }, func(r *http.Request) { r.Host = "attacker.example.test" }, func(r *http.Request) { r.TLS = nil; r.Header.Set("X-Forwarded-Proto", "https") }} {
		w := nativeRequest(t, h, "/enterprise/device/v1/enrollments", begin, "", edit)
		if w.Code < 400 {
			t.Fatal("cross-boundary device request admitted")
		}
	}
	body, _ := json.Marshal(begin)
	var untrusted map[string]any
	_ = json.Unmarshal(body, &untrusted)
	untrusted["origin"] = "https://attacker.example.test"
	if w := nativeRequest(t, h, "/enterprise/device/v1/enrollments", untrusted, "", nil); w.Code != 400 {
		t.Fatal("request-selected service origin admitted")
	}
	if b.beginCalls != 0 {
		t.Fatal("untrusted transport reached SQL")
	}
}

func TestDeviceHTTPUnknownDecisionNeverReleasesCompletionCodeOrRetries(t *testing.T) {
	h, b := newDeviceFixture(t)
	begin, _ := enrollmentRequest(t)
	nativeRequest(t, h, "/enterprise/device/v1/enrollments", begin, "", nil)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	b.deviceOutcome = sqlstore.OutcomeUnknown
	b.deviceError = &sqlstore.KernelError{Outcome: sqlstore.OutcomeUnknown, Class: sqlstore.ClassCommitUncertain, Err: errors.New("private-database-detail")}
	w := request(t, h, http.MethodPost, "/enterprise/api/v1/device-enrollments/request-1/decision", `{"client_id":"approve-1","action":"approve"}`, cookie, csrf)
	// GetDeviceEnrollment is also unavailable in this fake, so no decision is
	// made. A separate direct decision fake below isolates commit uncertainty.
	if w.Code != 503 || strings.Contains(w.Body.String(), "completion_location") || strings.Contains(w.Body.String(), "private-") {
		t.Fatal("unavailable approval disclosed private output")
	}
	b.deviceError = nil
	w = request(t, h, http.MethodPost, "/enterprise/api/v1/device-enrollments/request-1/decision", `{"client_id":"approve-2","action":"approve"}`, cookie, csrf)
	if w.Code != 503 || b.decisionCalls != 1 || strings.Contains(w.Body.String(), "completion_location") || len(w.Result().Cookies()) != 0 {
		t.Fatal("unknown approval issued completion or repeated its effect")
	}
}

func TestDeviceHTTPRefreshAndOneTimeRecoveryAreDifferentExplicitCalls(t *testing.T) {
	h, b := newDeviceFixture(t)
	refresh, _ := enterpriseauth.NewBrowserSecret()
	proof, _ := enterpriseauth.NewBrowserSecret()
	request := deviceRotationRequest{DeviceID: "device-1", CurrentGrantID: "grant-old", RotationID: "rotate-1", ClientProof: privateDeviceInput(proof), Next: candidateRequest(t, "grant-next")}
	b.deviceOutcome = sqlstore.OutcomeUnknown
	b.deviceError = errors.New("private-transport-error")
	w := nativeRequest(t, h, "/enterprise/device/v1/credentials/rotate", request, refresh, nil)
	if w.Code != 503 || b.rotateCalls != 1 || b.recoverCalls != 0 || strings.Contains(w.Body.String(), proof) || strings.Contains(w.Body.String(), refresh) {
		t.Fatal("rotation uncertainty triggered a retry/recovery or leaked secrets")
	}
	if b.rotationCommand.CurrentRefreshHash != tokenHash(refresh) || b.rotationCommand.ClientProofHash != tokenHash(proof) || b.rotationCommand.Origin != serviceOrigin {
		t.Fatal("rotation proof or deployment origin changed")
	}
	b.deviceOutcome = sqlstore.OutcomeCommitted
	b.deviceError = nil
	w = nativeRequest(t, h, "/enterprise/device/v1/credentials/recover", request, refresh, nil)
	if w.Code != 200 || b.recoverCalls != 1 || b.rotateCalls != 1 || !strings.Contains(w.Body.String(), sqlstore.ControlDeviceRecover) {
		t.Fatal("explicit recovery silently called ordinary rotation")
	}
	b.deviceOutcome = sqlstore.OutcomeRefused
	b.deviceError = &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: "rotation_recovery_unavailable"}
	w = nativeRequest(t, h, "/enterprise/device/v1/credentials/recover", request, refresh, nil)
	if w.Code != 403 || b.rotateCalls != 1 || strings.Contains(w.Body.String(), "grant-next") {
		t.Fatal("spent predecessor recovery was presented as reusable success")
	}
}

func TestDeviceHTTPAgentRegistrationUsesOnlyVerifiedDeviceAuthority(t *testing.T) {
	h, b := newDeviceFixture(t)
	access, _ := enterpriseauth.NewBrowserSecret()
	source, _ := enterpriseauth.NewBrowserSecret()
	b.deviceCredential = sqlstore.DeviceCredential{Auth: sqlstore.AuthContext{Deployment: "deployment-1", UserID: "user-1", DeviceGrantID: "grant-1"}, DeviceID: "device-1", GrantID: "grant-1", ExpiresAt: time.Now().Add(time.Hour)}
	value := map[string]string{"client_id": "agent-register-1", "source_key": source, "runtime": "codex"}
	w := nativeRequest(t, h, "/enterprise/device/v1/agents", value, access, nil)
	if w.Code != 200 || b.agentCalls != 1 || b.lastAuth != b.deviceCredential.Auth || b.deviceAuthHash != tokenHash(access) {
		t.Fatalf("Agent registration: %d %s", w.Code, w.Body.String())
	}
	b.deviceCredential.Auth.SessionID = "browser-session"
	w = nativeRequest(t, h, "/enterprise/device/v1/agents", value, access, nil)
	if w.Code != 401 || b.agentCalls != 1 {
		t.Fatal("device endpoint inherited browser authority")
	}
	value["source_key"] = "native-session-raw-identifier"
	if w := nativeRequest(t, h, "/enterprise/device/v1/agents", value, access, nil); w.Code != 400 {
		t.Fatal("raw native session-shaped source key accepted")
	}
}

func TestDeviceBrowserDecisionRequiresLiveSessionOriginAndCSRF(t *testing.T) {
	h, b := newDeviceFixture(t)
	begin, _ := enrollmentRequest(t)
	nativeRequest(t, h, "/enterprise/device/v1/enrollments", begin, "", nil)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	for _, mutation := range []func(*http.Request){func(r *http.Request) { r.Header.Del("Origin") }, func(r *http.Request) { r.Header.Set("Origin", "https://attacker.test") }, func(r *http.Request) { r.Header.Del(CSRFHeader) }, func(r *http.Request) { r.Header.Del("Cookie") }} {
		r := httptest.NewRequest(http.MethodPost, serviceOrigin+"/enterprise/api/v1/device-enrollments/request-1/decision", strings.NewReader(`{"client_id":"approve","action":"approve"}`))
		r.AddCookie(cookie)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", serviceOrigin)
		r.Header.Set(CSRFHeader, csrf)
		mutation(r)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatal("unprotected browser approval accepted")
		}
	}
	if b.decisionCalls != 0 {
		t.Fatal("CSRF rejection mutated an enrollment")
	}
	delete(b.sessions, tokenHash(cookie.Value))
	if w := request(t, h, http.MethodPost, "/enterprise/api/v1/device-enrollments/request-1/decision", `{"client_id":"approve","action":"approve"}`, cookie, csrf); w.Code != 401 {
		t.Fatal("revoked browser approved a device")
	}
}

func TestDeviceSensitiveInputDoesNotEnterFormattingOrErrorResponses(t *testing.T) {
	secret, _ := enterpriseauth.NewBrowserSecret()
	request := deviceCompleteRequest{ClientID: "operation", ClientProof: privateDeviceInput(secret), CompletionCode: privateDeviceInput(secret)}
	for _, formatted := range []string{fmt.Sprintf("%v", request), fmt.Sprintf("%+v", request), fmt.Sprintf("%#v", request)} {
		if strings.Contains(formatted, secret) {
			t.Fatal("request formatter exposed a device proof")
		}
	}
	h, _ := newDeviceFixture(t)
	response := nativeRequest(t, h, "/enterprise/device/v1/enrollments/request-1/complete", map[string]string{"client_id": "operation", "client_proof": secret, "completion_code": "bad"}, "", nil)
	if response.Code != 400 || strings.Contains(response.Body.String(), secret) {
		t.Fatal("validation error exposed supplied proof")
	}
}

func TestDeviceHTTPDeploymentPreflightUsesNoCredentialsOrPrivateLookup(t *testing.T) {
	h, b := newDeviceFixture(t)
	get := func(edit func(*http.Request)) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, serviceOrigin+"/enterprise/device/v1/deployment", nil)
		if edit != nil {
			edit(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := get(nil)
	if w.Code != 200 || w.Header().Get(DeviceDeploymentHeader) != "deployment-1" || w.Header().Get("Location") != "" || !strings.Contains(w.Body.String(), `"deployment_id":"deployment-1"`) || !strings.Contains(w.Body.String(), serviceOrigin) || len(w.Result().Cookies()) != 0 {
		t.Fatalf("invalid public deployment preflight: %d %s", w.Code, w.Body.String())
	}
	w = get(func(r *http.Request) { r.Header.Set(DeviceDeploymentHeader, "another-deployment") })
	if w.Code != 409 {
		t.Fatal("wrong deployment not rejected before credential submission")
	}
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Authorization", "Bearer private-token") }, func(r *http.Request) { r.Header.Set("Cookie", "private-cookie") }, func(r *http.Request) { r.Header.Set("Origin", serviceOrigin) }, func(r *http.Request) { r.URL.RawQuery = "token=private" }, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "none") }} {
		if w := get(edit); w.Code != 400 {
			t.Fatal("preflight accepted credentials/browser authority")
		}
	}
	if b.deviceAuthHash != ([32]byte{}) || b.authenticateCalls != 0 || b.beginCalls != 0 {
		t.Fatal("public preflight contacted private identity state")
	}
}

func TestDeviceHTTPSessionInspectUsesOnlyCurrentAccessCredentialAndPublicMetadata(t *testing.T) {
	h, b := newDeviceFixture(t)
	access, _ := enterpriseauth.NewBrowserSecret()
	b.deviceCredential = sqlstore.DeviceCredential{Auth: sqlstore.AuthContext{Deployment: "deployment-1", UserID: "user-1", DeviceGrantID: "grant-1"}, DeviceID: "device-1", GrantID: "grant-1", ExpiresAt: time.Now().Add(time.Hour), RefreshExpiresAt: time.Now().Add(24 * time.Hour)}
	w := nativeGet(t, h, "/enterprise/device/v1/session", access)
	if w.Code != 200 || b.deviceAuthHash != tokenHash(access) || !strings.Contains(w.Body.String(), `"grant_id":"grant-1"`) || strings.Contains(w.Body.String(), access) || strings.Contains(w.Body.String(), "Hash") || strings.Contains(w.Body.String(), "SessionID") || w.Header().Get(DeviceDeploymentHeader) != "deployment-1" {
		t.Fatalf("unsafe device inspection: %d %s", w.Code, w.Body.String())
	}
	b.deviceError = &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	w = nativeGet(t, h, "/enterprise/device/v1/session", access)
	if w.Code != 401 || strings.Contains(w.Body.String(), "grant-1") {
		t.Fatal("invalid/current-predecessor credential disclosed active metadata")
	}
	if b.rotateCalls != 0 || b.recoverCalls != 0 {
		t.Fatal("read-only current access inspection invoked refresh or predecessor recovery")
	}
}
