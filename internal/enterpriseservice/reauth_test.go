package enterpriseservice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type fakeGovernanceBackend struct {
	*fakeBackend
	prepareCalls, finishCalls, transferCalls int
	resolveCalls                             int
	action                                   sqlstore.GovernanceAction
	actor                                    sqlstore.AuthContext
	lastCompletion                           enterpriseauth.Completion
	transfer                                 sqlstore.OwnershipTransferCommand
	governanceErr                            error
	extraActions                             int
}

func (b *fakeGovernanceBackend) PrepareGovernanceReauthentication(_ context.Context, auth sqlstore.AuthContext, _ string, intent sqlstore.GovernanceIntent) (sqlstore.GovernanceAction, error) {
	b.prepareCalls++
	b.actor = auth
	b.action = sqlstore.GovernanceAction{ID: "governance-action-1", Intent: intent, ExpiresAt: time.Now().Add(5 * time.Minute)}
	return b.action, b.governanceErr
}
func (b *fakeGovernanceBackend) FinishGovernanceReauthentication(_ context.Context, auth sqlstore.AuthContext, completion enterpriseauth.Completion) (sqlstore.GovernanceAction, error) {
	b.finishCalls++
	b.lastCompletion = completion
	if auth != b.actor || completion.Identity.Subject != "subject-1" || completion.CorrelationID != b.action.ID {
		return sqlstore.GovernanceAction{}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization, Err: errors.New("private-identity-detail")}
	}
	b.action.Ready = true
	return b.action, b.governanceErr
}
func (b *fakeGovernanceBackend) ReadGovernanceAction(_ context.Context, auth sqlstore.AuthContext, id string) (sqlstore.GovernanceAction, error) {
	if auth != b.actor || id != b.action.ID {
		return sqlstore.GovernanceAction{}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	return b.action, b.governanceErr
}
func (b *fakeGovernanceBackend) ListGovernanceActions(_ context.Context, auth sqlstore.AuthContext) ([]sqlstore.GovernanceAction, error) {
	if auth != b.actor {
		return nil, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization}
	}
	actions := []sqlstore.GovernanceAction{b.action}
	for range b.extraActions {
		actions = append(actions, b.action)
	}
	return actions, b.governanceErr
}
func (b *fakeGovernanceBackend) TransferRoomOwnership(_ context.Context, command sqlstore.OwnershipTransferCommand) (sqlstore.Result, error) {
	b.transferCalls++
	b.transfer = command
	return b.operation, b.operationErr
}

func (b *fakeGovernanceBackend) ResolveRoomOwnership(_ context.Context, command sqlstore.OwnershipTransferCommand) (sqlstore.Result, error) {
	b.resolveCalls++
	b.transfer = command
	return b.operation, b.operationErr
}

func newGovernanceFixture(t *testing.T) (*Handler, *fakeProvider, *fakeGovernanceBackend) {
	t.Helper()
	_, provider, _ := newHandlerFixture(t)
	store, _ := enterpriseauth.NewMemoryStore(8)
	backend := &fakeGovernanceBackend{fakeBackend: newFakeBackend()}
	h, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, provider, store, backend)
	if err != nil {
		t.Fatal(err)
	}
	return h, provider, backend
}
func governanceIntent() sqlstore.GovernanceIntent {
	return sqlstore.GovernanceIntent{RoomID: "room-1", TargetUserID: "member-1", ExpectedOwnerUserID: "disabled-owner", ExpectedRevision: 7, Reason: "Recover disabled Owner after verified operator request"}
}
func governanceBody(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(struct {
		ClientID string                    `json:"client_id"`
		Intent   sqlstore.GovernanceIntent `json:"intent"`
	}{"prepare-1", governanceIntent()})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func prepareReauth(t *testing.T, h *Handler, provider *fakeProvider, cookie *http.Cookie, csrf string) *http.Cookie {
	t.Helper()
	response := request(t, h, http.MethodPost, "/enterprise/api/v1/governance/actions", governanceBody(t), cookie, csrf)
	if response.Code != 200 {
		t.Fatalf("prepare step-up: %d %s", response.Code, response.Body.String())
	}
	if provider.beginIntent.Purpose != enterpriseauth.PurposeReauthenticate || provider.beginIntent.ReturnPath != "/enterprise/" || provider.beginIntent.CorrelationID != "governance-action-1" {
		t.Fatal("step-up did not freeze its purpose/action/return")
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != h.policy.loginCookie {
		t.Fatal("step-up begin changed the application session credential")
	}
	return cookies[0]
}

func completeReauth(t *testing.T, h *Handler, provider *fakeProvider, sessionCookie, browserCookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, serviceOrigin+"/enterprise/auth/callback?state=public-state&code=private-code", nil)
	r.AddCookie(sessionCookie)
	r.AddCookie(browserCookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func verifiedReauth() enterpriseauth.Completion {
	return enterpriseauth.Completion{Identity: enterpriseauth.VerifiedIdentity{ConnectionID: "connection-1", Issuer: "https://idp.example.test", Subject: "subject-1", AuthTime: time.Now().UTC()}, Purpose: enterpriseauth.PurposeReauthenticate, ReturnPath: "/enterprise/", CorrelationID: "governance-action-1", FreshReauthentication: true}
}

func TestGovernanceHTTPStepupKeepsOriginalSessionAndNeverPerformsTransferOrLogin(t *testing.T) {
	h, provider, b := newGovernanceFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	browser := prepareReauth(t, h, provider, cookie, csrf)
	admissions, completions := b.admitCalls, provider.completeCalls
	provider.completion = verifiedReauth()
	w := completeReauth(t, h, provider, cookie, browser)
	if w.Code != 303 || w.Header().Get("Location") != "/enterprise/" || len(w.Result().Cookies()) != 0 || b.finishCalls != 1 || b.admitCalls != admissions || b.transferCalls != 0 || provider.completeCalls != completions+1 {
		t.Fatalf("step-up crossed authority boundaries: %d %s", w.Code, w.Body.String())
	}
	if b.actor.SessionID != b.lastCandidate.SessionID || !b.action.Ready {
		t.Fatal("ready proof lost original browser session")
	}
	for _, path := range []string{"/enterprise/api/v1/governance/actions", "/enterprise/api/v1/governance/actions/governance-action-1"} {
		w = request(t, h, http.MethodGet, path, "", cookie, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), cookie.Value) || strings.Contains(w.Body.String(), "SessionID") || strings.Contains(w.Body.String(), "auth_time") {
			t.Fatal("action metadata disclosed credentials/evidence")
		}
	}
	if b.roomCalls != 0 {
		t.Fatal("governance preparation/read loaded Room content")
	}
}

func TestGovernanceHTTPRejectsMissingCSRFAndRequestSuppliedEvidence(t *testing.T) {
	h, provider, b := newGovernanceFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	for _, body := range []string{strings.TrimSuffix(governanceBody(t), "}") + `,"fresh_reauthentication":true}`, strings.TrimSuffix(governanceBody(t), "}") + `,"completion":{"subject":"admin"}}`} {
		if w := request(t, h, http.MethodPost, "/enterprise/api/v1/governance/actions", body, cookie, csrf); w.Code != 400 {
			t.Fatal("request-controlled authentication evidence admitted")
		}
	}
	if w := request(t, h, http.MethodPost, "/enterprise/api/v1/governance/actions", governanceBody(t), cookie, ""); w.Code != 403 {
		t.Fatal("step-up started without browser CSRF")
	}
	if b.prepareCalls != 0 || provider.beginIntent.Purpose == enterpriseauth.PurposeReauthenticate {
		t.Fatal("invalid request prepared SQL action or contacted Provider")
	}
}

func TestGovernanceHTTPWrongIdentityOrNewSessionCannotCompleteOriginalAction(t *testing.T) {
	for _, mode := range []string{"wrong_identity", "same_user_new_session", "different_user_session", "revoked_session"} {
		t.Run(mode, func(t *testing.T) {
			h, provider, b := newGovernanceFixture(t)
			cookie := loginSession(t, h)
			csrf := sessionCSRF(t, h, cookie)
			browser := prepareReauth(t, h, provider, cookie, csrf)
			provider.completion = verifiedReauth()
			if mode == "wrong_identity" {
				provider.completion.Identity.Subject = "another-identity"
			} else if mode == "revoked_session" {
				delete(b.sessions, tokenHash(cookie.Value))
			} else {
				secret, _ := enterpriseauth.NewBrowserSecret()
				auth := sqlstore.AuthContext{Deployment: "deployment-1", UserID: "user-1", SessionID: "session-new"}
				if mode == "different_user_session" {
					auth.UserID = "user-2"
				}
				b.sessions[tokenHash(secret)] = sqlstore.BrowserSession{Auth: auth, CSRFHash: tokenHash(deriveCSRF(secret, auth.Deployment, auth.SessionID)), ExpiresAt: time.Now().Add(time.Hour)}
				cookie = &http.Cookie{Name: h.policy.sessionCookie, Value: secret}
			}
			w := completeReauth(t, h, provider, cookie, browser)
			if w.Code < 400 || len(w.Result().Cookies()) != 0 || w.Header().Get("Location") != "" || b.action.Ready || b.transferCalls != 0 || b.admitCalls != 1 || strings.Contains(w.Body.String(), "private-") {
				t.Fatalf("wrong actor completed proof: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestGovernanceHTTPInvalidVerifiedPurposeCannotCreateProof(t *testing.T) {
	for _, change := range []func(*enterpriseauth.Completion){func(c *enterpriseauth.Completion) { c.FreshReauthentication = false }, func(c *enterpriseauth.Completion) { c.Identity.AuthTime = time.Time{} }, func(c *enterpriseauth.Completion) { c.CorrelationID = "" }, func(c *enterpriseauth.Completion) { c.ReturnPath = "//attacker.test" }, func(c *enterpriseauth.Completion) { c.Identity.AuthTime = time.Now().Add(time.Hour) }} {
		h, provider, b := newGovernanceFixture(t)
		cookie := loginSession(t, h)
		csrf := sessionCSRF(t, h, cookie)
		browser := prepareReauth(t, h, provider, cookie, csrf)
		provider.completion = verifiedReauth()
		change(&provider.completion)
		w := completeReauth(t, h, provider, cookie, browser)
		if w.Code != 401 || b.finishCalls != 0 || len(w.Result().Cookies()) != 0 {
			t.Fatal("invalid step-up evidence entered SQL proof creation")
		}
	}
}

func TestGovernanceHTTPOwnershipTransferPassesExactIntentProofAndSession(t *testing.T) {
	h, _, b := newGovernanceFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	for _, action := range []string{"", "governance-action-1"} {
		body, _ := json.Marshal(struct {
			ClientID string                    `json:"client_id"`
			Intent   sqlstore.GovernanceIntent `json:"intent"`
			ActionID string                    `json:"action_id,omitempty"`
		}{"transfer-1", governanceIntent(), action})
		w := request(t, h, http.MethodPost, "/enterprise/api/v1/rooms/room-1/ownership", string(body), cookie, csrf)
		if w.Code != 200 || b.transfer.Auth.SessionID != b.lastCandidate.SessionID || b.transfer.Intent != governanceIntent() || b.transfer.GovernanceActionID != action {
			t.Fatalf("transfer selectors changed: %d %s", w.Code, w.Body.String())
		}
	}
	body, _ := json.Marshal(struct {
		ClientID string                    `json:"client_id"`
		Intent   sqlstore.GovernanceIntent `json:"intent"`
	}{"transfer-2", governanceIntent()})
	if w := request(t, h, http.MethodPost, "/enterprise/api/v1/rooms/room-other/ownership", string(body), cookie, csrf); w.Code != 400 {
		t.Fatal("URL Room disagreed with frozen governance intent")
	}
	if b.transferCalls != 2 || b.roomCalls != 0 {
		t.Fatal("transfer adapter performed extra content reads/effects")
	}
}

func TestGovernanceHTTPUnknownTransferDoesNotReplayOrExposeProof(t *testing.T) {
	h, _, b := newGovernanceFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	b.operation = sqlstore.Result{Outcome: sqlstore.OutcomeUnknown, Class: sqlstore.ClassCommitUncertain}
	b.operationErr = errors.New("private-proof-detail")
	body, _ := json.Marshal(struct {
		ClientID string                    `json:"client_id"`
		Intent   sqlstore.GovernanceIntent `json:"intent"`
		ActionID string                    `json:"action_id"`
	}{"transfer-1", governanceIntent(), "governance-action-1"})
	w := request(t, h, http.MethodPost, "/enterprise/api/v1/rooms/room-1/ownership", string(body), cookie, csrf)
	if w.Code != 503 || b.transferCalls != 1 || strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), "governance-action-1") {
		t.Fatal("uncertain transfer repeated or leaked proof references")
	}
	b.operation = sqlstore.Result{Outcome: sqlstore.OutcomeReplayed, Seq: 8, Revision: 8}
	b.operationErr = nil
	w = request(t, h, http.MethodPost, "/enterprise/api/v1/rooms/room-1/resolve-ownership", string(body), cookie, csrf)
	if w.Code != 200 || b.transferCalls != 1 || b.resolveCalls != 1 || b.transfer.GovernanceActionID != "governance-action-1" || b.transfer.Auth.SessionID != b.lastCandidate.SessionID {
		t.Fatalf("explicit verification replayed the effect or changed context: %d %s", w.Code, w.Body.String())
	}
}
