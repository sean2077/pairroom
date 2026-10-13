package enterpriseservice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type fakeBindingBackend struct {
	*fakeDeviceBackend
	accessHash                                                                                                                        [32]byte
	owner                                                                                                                             string
	requestCommand                                                                                                                    sqlstore.BindingRequestCommand
	decision                                                                                                                          sqlstore.BindingDecisionCommand
	completion                                                                                                                        sqlstore.BindingCompletionCommand
	info                                                                                                                              sqlstore.BindingRequestInfo
	requestCalls, resolveRequestCalls, decisionCalls, resolveDecisionCalls, completionCalls, resolveCompletionCalls, bindingReadCalls int
	renewalCalls, resolveRenewalCalls                                                                                                 int
	renewal                                                                                                                           sqlstore.BindingRenewalCommand
	renewalExpires                                                                                                                    time.Time
	bindingOutcome                                                                                                                    sqlstore.Outcome
	bindingError                                                                                                                      error
}

func (b *fakeBindingBackend) AuthenticateDevice(ctx context.Context, hash [32]byte, origin string) (sqlstore.DeviceCredential, error) {
	if hash != b.accessHash {
		return sqlstore.DeviceCredential{}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	return b.fakeDeviceBackend.AuthenticateDevice(ctx, hash, origin)
}
func (b *fakeBindingBackend) bindingResult(kind string) (sqlstore.Result, error) {
	receipt := &sqlstore.BindingAuthorityReceipt{Version: 1, Kind: kind, Request: &b.info}
	if kind == "complete" {
		receipt.Binding = &model.EnterpriseBinding{Version: 1, RoomID: b.info.RoomID, Slot: b.info.Slot, BindID: b.info.BindingID, Generation: b.info.ObservedGeneration, AgentID: b.info.AgentID, Runtime: b.info.Runtime, Active: true, ParkEnabled: b.info.ParkEnabled}
		receipt.GrantID = b.requestCommand.Grant.GrantID
		receipt.ExpiresAt = time.Now().Add(time.Hour)
		receipt.Seq = 2
		receipt.Revision = 2
	}
	return sqlstore.Result{Outcome: b.bindingOutcome, BindingAuthority: receipt, Fact: model.Event{ID: "private-fact", Data: json.RawMessage(`{"receipt":"private-capability-must-not-leak"}`)}}, b.bindingError
}
func (b *fakeBindingBackend) CommitBindingRequest(_ context.Context, cmd sqlstore.BindingRequestCommand) (sqlstore.Result, error) {
	b.requestCalls++
	b.requestCommand = cmd
	b.info = sqlstore.BindingRequestInfo{RequestID: cmd.RequestID, RoomID: cmd.RoomID, Slot: cmd.Slot, ObservedGeneration: cmd.ObservedGeneration, Replace: cmd.Replace, AgentID: cmd.AgentID, Runtime: model.RuntimeCodex, BindingID: cmd.BindingID, UserID: cmd.Auth.UserID, DeviceID: "device-1", ParkEnabled: cmd.ParkEnabled, Status: "pending", RequestedAt: time.Now(), ExpiresAt: time.Now().Add(10 * time.Minute)}
	return b.bindingResult("request")
}
func (b *fakeBindingBackend) ResolveBindingRequest(_ context.Context, cmd sqlstore.BindingRequestCommand) (sqlstore.Result, error) {
	b.resolveRequestCalls++
	b.requestCommand = cmd
	return b.bindingResult("request")
}
func (b *fakeBindingBackend) DecideBindingRequest(_ context.Context, cmd sqlstore.BindingDecisionCommand) (sqlstore.Result, error) {
	b.decisionCalls++
	b.decision = cmd
	if cmd.Auth.UserID != b.owner || cmd.Auth.SessionID == "" {
		return sqlstore.Result{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	b.info.Status = "denied"
	if cmd.Approve {
		b.info.Status = "approved"
	}
	return b.bindingResult("decide")
}
func (b *fakeBindingBackend) ResolveBindingDecision(_ context.Context, cmd sqlstore.BindingDecisionCommand) (sqlstore.Result, error) {
	b.resolveDecisionCalls++
	b.decision = cmd
	return b.bindingResult("decide")
}
func (b *fakeBindingBackend) CompleteBindingRequest(_ context.Context, cmd sqlstore.BindingCompletionCommand) (sqlstore.Result, error) {
	b.completionCalls++
	b.completion = cmd
	if cmd.ProofHash != b.requestCommand.ProofHash {
		return sqlstore.Result{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	b.info.Status = "completed"
	return b.bindingResult("complete")
}
func (b *fakeBindingBackend) ResolveBindingCompletion(_ context.Context, cmd sqlstore.BindingCompletionCommand) (sqlstore.Result, error) {
	b.resolveCompletionCalls++
	b.completion = cmd
	return b.bindingResult("complete")
}
func (b *fakeBindingBackend) ReadBindingRequest(_ context.Context, auth sqlstore.AuthContext, room, id string) (sqlstore.BindingRequestInfo, error) {
	b.bindingReadCalls++
	if room != b.info.RoomID || id != b.info.RequestID || auth.SessionID != "" && auth.UserID != b.owner {
		return sqlstore.BindingRequestInfo{}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	return b.info, b.bindingError
}
func (b *fakeBindingBackend) ListBindingRequests(_ context.Context, auth sqlstore.AuthContext, room string) ([]sqlstore.BindingRequestInfo, error) {
	b.bindingReadCalls++
	if room != b.info.RoomID || auth.SessionID != "" && auth.UserID != b.owner {
		return nil, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	return []sqlstore.BindingRequestInfo{b.info}, b.bindingError
}
func (b *fakeBindingBackend) ReadBindingContext(_ context.Context, auth sqlstore.AuthContext, room string) (sqlstore.BindingContext, error) {
	b.bindingReadCalls++
	return sqlstore.BindingContext{RoomID: room, UserID: auth.UserID, Role: "member", OwnerUserID: b.owner, Slots: map[model.ActorID]sqlstore.BindingSlotContext{model.ActorSlot1: {Slot: model.ActorSlot1, HeadGeneration: 3, Vacant: true}, model.ActorSlot2: {Slot: model.ActorSlot2, HeadGeneration: 5, Vacant: true}}}, b.bindingError
}

func (b *fakeBindingBackend) renewalResult(cmd sqlstore.BindingRenewalCommand) (sqlstore.Result, error) {
	b.renewal = cmd
	expires := b.renewalExpires
	if expires.IsZero() {
		expires = time.Now().Add(time.Hour)
	}
	return sqlstore.Result{Outcome: b.bindingOutcome, BindingAuthority: &sqlstore.BindingAuthorityReceipt{Version: 1, Kind: "renew", Binding: &model.EnterpriseBinding{Version: 1, RoomID: cmd.RoomID, Slot: model.ActorSlot1, BindID: cmd.BindingID, Generation: cmd.Generation, AgentID: "agent-1", Runtime: model.RuntimeCodex, Active: true}, GrantID: cmd.Grant.GrantID, ExpiresAt: expires, Seq: 5, Revision: 3}}, b.bindingError
}
func (b *fakeBindingBackend) RenewBindingCredential(_ context.Context, cmd sqlstore.BindingRenewalCommand) (sqlstore.Result, error) {
	b.renewalCalls++
	return b.renewalResult(cmd)
}
func (b *fakeBindingBackend) ResolveBindingRenewal(_ context.Context, cmd sqlstore.BindingRenewalCommand) (sqlstore.Result, error) {
	b.resolveRenewalCalls++
	return b.renewalResult(cmd)
}

func newBindingFixture(t *testing.T) (*Handler, *fakeBindingBackend, string) {
	t.Helper()
	access, _ := enterpriseauth.NewBrowserSecret()
	b := &fakeBindingBackend{fakeDeviceBackend: newFakeDeviceBackend(), owner: "user-1", accessHash: tokenHash(access), bindingOutcome: sqlstore.OutcomeCommitted}
	b.deviceCredential = sqlstore.DeviceCredential{Auth: sqlstore.AuthContext{Deployment: "deployment-1", UserID: "user-1", DeviceGrantID: "device-grant-1"}, DeviceID: "device-1", GrantID: "device-grant-1", ExpiresAt: time.Now().Add(time.Hour)}
	_, provider, _ := newHandlerFixture(t)
	transactions, _ := enterpriseauth.NewMemoryStore(8)
	h, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, provider, transactions, b)
	if err != nil {
		t.Fatal(err)
	}
	return h, b, access
}

func bindingInput(t *testing.T) (bindingRequestBody, string, string) {
	t.Helper()
	proof, _ := enterpriseauth.NewBrowserSecret()
	token, _ := enterpriseauth.NewBrowserSecret()
	ph, th := tokenHash(proof), tokenHash(token)
	body := bindingRequestBody{ClientID: "request-operation-1", RequestID: "binding-request-1", AgentID: "agent-1", BindingID: "binding-1", Slot: model.ActorSlot1, ObservedGeneration: 3, ParkEnabled: false, ProofHash: base64.RawURLEncoding.EncodeToString(ph[:])}
	body.Grant.GrantID = "binding-grant-1"
	body.Grant.TokenHash = base64.RawURLEncoding.EncodeToString(th[:])
	return body, proof, token
}

func nativeGet(t *testing.T, h *Handler, path, access string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, serviceOrigin+path, nil)
	r.Header.Set(DeviceDeploymentHeader, "deployment-1")
	r.Header.Set("Authorization", "Bearer "+access)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBindingHTTPKeepsDeviceConsentOwnerApprovalAndCompletionSeparate(t *testing.T) {
	h, b, access := newBindingFixture(t)
	body, proof, token := bindingInput(t)
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests", body, access, nil)
	if w.Code != 200 || b.requestCalls != 1 || b.decisionCalls != 0 || b.completionCalls != 0 || b.requestCommand.Auth != b.deviceCredential.Auth || b.requestCommand.ParkEnabled || b.requestCommand.ObservedGeneration != 3 || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), "private-capability") {
		t.Fatalf("unsafe binding request: %d %s", w.Code, w.Body.String())
	}
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	w = request(t, h, http.MethodPost, "/enterprise/api/v1/rooms/room-1/binding-requests/binding-request-1/decision", `{"client_id":"approve-1","action":"approve"}`, cookie, csrf)
	if w.Code != 200 || b.decisionCalls != 1 || b.completionCalls != 0 || b.decision.Auth.SessionID != b.lastCandidate.SessionID || b.decision.Auth.DeviceGrantID != "" {
		t.Fatal("Owner approval impersonated or activated the endpoint")
	}
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests/binding-request-1/complete", bindingCompletionBody{ClientID: "complete-1", ClientProof: privateDeviceInput(proof)}, access, nil)
	if w.Code != 200 || b.completionCalls != 1 || b.completion.ProofHash != tokenHash(proof) || b.completion.Auth != b.deviceCredential.Auth || strings.Contains(w.Body.String(), proof) || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), body.Grant.TokenHash) || strings.Contains(w.Body.String(), "private-capability") {
		t.Fatalf("unsafe binding completion: %d %s", w.Code, w.Body.String())
	}
}

func TestBindingHTTPResolutionNeverReexecutesRequestDecisionOrCompletion(t *testing.T) {
	h, b, access := newBindingFixture(t)
	body, proof, _ := bindingInput(t)
	nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests", body, access, nil)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	b.bindingOutcome = sqlstore.OutcomeUnknown
	b.bindingError = errors.New("private-uncertain")
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests", body, access, nil)
	if w.Code != 503 || b.requestCalls != 2 || b.resolveRequestCalls != 0 || strings.Contains(w.Body.String(), "binding-request-1") {
		t.Fatal("unknown request auto-resolved/replayed or disclosed partial receipt")
	}
	b.bindingOutcome = sqlstore.OutcomeReplayed
	b.bindingError = nil
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests/binding-request-1/resolve-request", body, access, nil)
	if w.Code != 200 || b.requestCalls != 2 || b.resolveRequestCalls != 1 {
		t.Fatal("request verification performed the mutation")
	}
	w = request(t, h, http.MethodPost, "/enterprise/api/v1/rooms/room-1/binding-requests/binding-request-1/resolve-decision", `{"client_id":"approval-1","action":"approve"}`, cookie, csrf)
	if w.Code != 200 || b.decisionCalls != 0 || b.resolveDecisionCalls != 1 {
		t.Fatal("decision verification performed approval")
	}
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests/binding-request-1/resolve-completion", bindingCompletionBody{ClientID: "completion-1", ClientProof: privateDeviceInput(proof)}, access, nil)
	if w.Code != 200 || b.completionCalls != 0 || b.resolveCompletionCalls != 1 {
		t.Fatal("completion verification activated a grant")
	}
}

func TestBindingHTTPRejectsCredentialKindConfusionAndOwnerRetargeting(t *testing.T) {
	h, b, access := newBindingFixture(t)
	body, proof, bindingToken := bindingInput(t)
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests", body, bindingToken, nil)
	if w.Code < 400 || b.requestCalls != 0 {
		t.Fatal("a Binding token inherited Device authority")
	}
	nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests", body, access, nil)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	for _, extra := range []string{`"agent_id":"another-agent"`, `"runtime":"claude"`, `"slot":"slot2"`, `"observed_generation":99`, `"token_hash":"private"`} {
		jsonBody := `{"client_id":"approve-1","action":"approve",` + extra + `}`
		if w := request(t, h, http.MethodPost, "/enterprise/api/v1/rooms/room-1/binding-requests/binding-request-1/decision", jsonBody, cookie, csrf); w.Code != 400 {
			t.Fatal("Owner altered endpoint consent")
		}
	}
	b.owner = "different-owner"
	w = request(t, h, http.MethodPost, "/enterprise/api/v1/rooms/room-1/binding-requests/binding-request-1/decision", `{"client_id":"approve-1","action":"approve"}`, cookie, csrf)
	if w.Code != 403 {
		t.Fatal("organization administrator bypassed current Room Owner policy")
	}
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests/binding-request-1/complete", bindingCompletionBody{ClientID: "complete-1", ClientProof: privateDeviceInput(body.ProofHash)}, access, nil)
	if w.Code != 403 || b.completion.ProofHash == tokenHash(proof) {
		t.Fatal("public proof hash was interpreted as original client proof")
	}
}

func TestBindingHTTPReadAndContextHaveFencedMetadataOnly(t *testing.T) {
	h, b, access := newBindingFixture(t)
	body, _, token := bindingInput(t)
	nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests", body, access, nil)
	cookie := loginSession(t, h)
	for _, suffix := range []string{"binding-requests", "binding-requests/binding-request-1", "binding-context"} {
		w := nativeGet(t, h, "/enterprise/device/v1/rooms/room-1/"+suffix, access)
		if w.Code != 200 || strings.Contains(w.Body.String(), token) || strings.Contains(w.Body.String(), body.ProofHash) || strings.Contains(w.Body.String(), "messages") || strings.Contains(w.Body.String(), "token_hash") {
			t.Fatalf("native metadata response: %d %s", w.Code, w.Body.String())
		}
		w = request(t, h, http.MethodGet, "/enterprise/api/v1/rooms/room-1/"+suffix, "", cookie, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "private-") {
			t.Fatalf("browser metadata response: %d %s", w.Code, w.Body.String())
		}
	}
	if b.roomCalls != 0 {
		t.Fatal("Binding metadata read loaded Room content")
	}
	w := nativeGet(t, h, "/enterprise/device/v1/rooms/room-1/binding-context", access)
	if !strings.Contains(w.Body.String(), `"head_generation":3`) || !strings.Contains(w.Body.String(), `"head_generation":5`) {
		t.Fatal("metadata guessed an initial slot generation")
	}
}

func TestBindingHTTPDeniesBrowserTransportCrossoversAndMalformedGenerations(t *testing.T) {
	h, b, access := newBindingFixture(t)
	body, _, _ := bindingInput(t)
	for _, generation := range []uint64{0, ^uint64(0)} {
		bad := body
		bad.ObservedGeneration = generation
		if w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests", bad, access, nil); w.Code != 400 {
			t.Fatal("invalid observed generation accepted")
		}
	}
	for _, edit := range []func(*http.Request){func(r *http.Request) { r.Header.Set("Cookie", "browser=private") }, func(r *http.Request) { r.Header.Set("Origin", serviceOrigin) }, func(r *http.Request) { r.Header.Set(DeviceDeploymentHeader, "another-deployment") }} {
		if w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/binding-requests", body, access, edit); w.Code != 400 {
			t.Fatal("browser or unpinned authority accepted on Device Binding endpoint")
		}
	}
	if b.requestCalls != 0 {
		t.Fatal("invalid Binding request reached the SQL mutation")
	}
}

func TestBindingHTTPRenewalKeepsBindingIdentityAndUsesOnlyDeviceAuthority(t *testing.T) {
	h, b, access := newBindingFixture(t)
	secret, _ := enterpriseauth.NewBrowserSecret()
	hash := tokenHash(secret)
	input := bindingRenewalBody{ClientID: "renewal-1", Generation: 3}
	input.Grant.GrantID = "binding-grant-next"
	input.Grant.TokenHash = base64.RawURLEncoding.EncodeToString(hash[:])
	w := nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/bindings/binding-1/renew", input, access, nil)
	if w.Code != 200 || b.renewalCalls != 1 || b.requestCalls != 0 || b.decisionCalls != 0 || b.completionCalls != 0 || b.renewal.Auth != b.deviceCredential.Auth || b.renewal.BindingID != "binding-1" || b.renewal.Generation != 3 || b.renewal.Grant.TokenHash != hash || strings.Contains(w.Body.String(), secret) || strings.Contains(w.Body.String(), input.Grant.TokenHash) {
		t.Fatalf("renewal altered Binding identity or exposed candidate: %d %s", w.Code, w.Body.String())
	}
	b.bindingOutcome = sqlstore.OutcomeUnknown
	b.bindingError = errors.New("private-unknown")
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/bindings/binding-1/renew", input, access, nil)
	if w.Code != 503 || b.renewalCalls != 2 || b.resolveRenewalCalls != 0 {
		t.Fatal("unknown renewal automatically retried or resolved")
	}
	b.bindingOutcome = sqlstore.OutcomeReplayed
	b.bindingError = nil
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/bindings/binding-1/resolve-renewal", input, access, nil)
	if w.Code != 200 || b.renewalCalls != 2 || b.resolveRenewalCalls != 1 {
		t.Fatal("renewal verification invoked another credential rotation")
	}
	b.renewalExpires = time.Now().Add(-time.Hour)
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/bindings/binding-1/resolve-renewal", input, access, nil)
	if w.Code != 200 || b.renewalCalls != 2 || b.resolveRenewalCalls != 2 {
		t.Fatal("expired historical receipt could not be confirmed without renewing again")
	}
	other, _ := enterpriseauth.NewBrowserSecret()
	w = nativeRequest(t, h, "/enterprise/device/v1/rooms/room-1/bindings/binding-1/renew", input, other, nil)
	if w.Code < 400 || b.renewalCalls != 2 {
		t.Fatal("another credential class could renew the narrow grant")
	}
}

func TestBindingApprovalPageEntryIsRequestOnlyAndSelfContained(t *testing.T) {
	h, b, _ := newBindingFixture(t)
	for _, tc := range []struct {
		path, contentType, marker string
	}{
		{"/enterprise/binding-requests/room-1/request-1", "text/html", "/enterprise/assets/binding.js"},
		{"/enterprise/assets/binding.js", "text/javascript", "resolve-decision"},
	} {
		r := httptest.NewRequest(http.MethodGet, serviceOrigin+tc.path, nil)
		w := httptest.NewRecorder()
		if !h.serveBindingApproval(w, r) || w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), tc.contentType) || !strings.Contains(w.Body.String(), tc.marker) {
			t.Fatalf("approval page unavailable: %s %d", tc.path, w.Code)
		}
		if len(w.Result().Cookies()) != 0 || !strings.Contains(w.Header().Get("Content-Security-Policy"), "script-src 'self'") || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatal("static review page minted a credential or omitted its script/framing policy")
		}
	}
	for _, path := range []string{"/enterprise/binding-requests/room-1/request-1?token=private", "/enterprise/binding-requests/room-1/request-1?"} {
		w := httptest.NewRecorder()
		if !h.serveBindingApproval(w, httptest.NewRequest(http.MethodGet, serviceOrigin+path, nil)) || w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "private") {
			t.Fatal("static review accepted a query or reflected private input")
		}
	}
	for _, path := range []string{"/enterprise/binding-requests/room-1", "/enterprise/binding-requests/room-1/request-1/extra"} {
		if h.serveBindingApproval(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, serviceOrigin+path, nil)) {
			t.Fatal("static review accepted an ambiguous request identity")
		}
	}
	if b.bindingReadCalls != 0 || b.decisionCalls != 0 || b.completionCalls != 0 || b.authenticateCalls != 0 {
		t.Fatal("request-only static page performed private lookup or approval")
	}
}
