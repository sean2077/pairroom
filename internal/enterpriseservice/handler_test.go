package enterpriseservice

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

const serviceOrigin = "https://service.example.test"

type fakeProvider struct {
	beginIntent           enterpriseauth.LoginIntent
	browser               string
	completeCode          string
	completeCalls         int
	beginErr, completeErr error
	completion            enterpriseauth.Completion
}

func (p *fakeProvider) Begin(_ context.Context, _ enterpriseauth.TransactionStore, browser string, intent enterpriseauth.LoginIntent) (enterpriseauth.Authorization, error) {
	p.browser = browser
	p.beginIntent = intent
	return enterpriseauth.Authorization{URL: "https://idp.example.test/authorize?state=public-state", ExpiresAt: time.Now().Add(5 * time.Minute)}, p.beginErr
}
func (p *fakeProvider) Complete(_ context.Context, _ enterpriseauth.TransactionStore, browser, state, code string) (enterpriseauth.Completion, error) {
	p.completeCalls++
	p.completeCode = code
	if browser != p.browser {
		return enterpriseauth.Completion{}, enterpriseauth.ErrTransactionInvalid
	}
	return p.completion, p.completeErr
}

type fakeBackend struct {
	mu                                                    sync.Mutex
	sessionRequired                                       bool
	sessions                                              map[[32]byte]sqlstore.BrowserSession
	admitOutcome                                          sqlstore.Outcome
	admitErr                                              error
	corruptReceipt                                        bool
	onAdmit                                               func(sqlstore.IdentityLoginCommand)
	admitCalls, authenticateCalls, roomCalls, revokeCalls int
	lastCandidate                                         sqlstore.BrowserSessionCandidate
	lastAuth                                              sqlstore.AuthContext
	lastClientID, lastRoomID                              string
	operation                                             sqlstore.Result
	operationErr, readErr                                 error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{sessionRequired: true, sessions: make(map[[32]byte]sqlstore.BrowserSession), admitOutcome: sqlstore.OutcomeCommitted, operation: sqlstore.Result{Outcome: sqlstore.OutcomeCommitted, Seq: 1, Revision: 1, Fact: model.Event{ID: "event-1", RoomID: "room-1", Kind: "test.fact", Seq: 1}}}
}
func (b *fakeBackend) RequiresSessionAuth() bool { return b.sessionRequired }
func (b *fakeBackend) AdmitIdentity(_ context.Context, cmd sqlstore.IdentityLoginCommand) (sqlstore.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.admitCalls++
	b.lastCandidate = cmd.Session
	if b.onAdmit != nil {
		b.onAdmit(cmd)
	}
	receipt := &sqlstore.ControlReceipt{Version: 1, Kind: "identity.login", UserID: "user-1", SessionID: cmd.Session.SessionID, ExpiresAt: cmd.Session.ExpiresAt, Administrator: true}
	if b.corruptReceipt {
		receipt.SessionID = "another-session"
	}
	if b.admitErr == nil && (b.admitOutcome == sqlstore.OutcomeCommitted || b.admitOutcome == sqlstore.OutcomeReplayed) {
		b.sessions[cmd.Session.TokenHash] = sqlstore.BrowserSession{Auth: sqlstore.AuthContext{Deployment: "deployment-1", UserID: "user-1", SessionID: cmd.Session.SessionID}, CSRFHash: cmd.Session.CSRFHash, ExpiresAt: cmd.Session.ExpiresAt, Administrator: true}
	}
	return sqlstore.Result{Outcome: b.admitOutcome, Control: receipt}, b.admitErr
}
func (b *fakeBackend) AuthenticateSession(_ context.Context, hash [32]byte) (sqlstore.BrowserSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.authenticateCalls++
	session, ok := b.sessions[hash]
	if !ok {
		return sqlstore.BrowserSession{}, &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization, Err: errors.New("private-database-description")}
	}
	return session, nil
}
func (b *fakeBackend) RevokeSession(_ context.Context, auth sqlstore.AuthContext, clientID string) (sqlstore.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revokeCalls++
	b.lastAuth = auth
	b.lastClientID = clientID
	if b.operationErr == nil && confirmed(b.operation) {
		for hash, session := range b.sessions {
			if session.Auth == auth {
				delete(b.sessions, hash)
			}
		}
	}
	return b.operation, b.operationErr
}
func (b *fakeBackend) record(auth sqlstore.AuthContext, room, client string) (sqlstore.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roomCalls++
	b.lastAuth = auth
	b.lastRoomID = room
	b.lastClientID = client
	return b.operation, b.operationErr
}
func (b *fakeBackend) CommitRoomCreate(_ context.Context, cmd sqlstore.RoomCreateCommand) (sqlstore.Result, error) {
	return b.record(cmd.Auth, cmd.RoomID, cmd.ClientID)
}
func (b *fakeBackend) ResolveRoomCreate(_ context.Context, auth sqlstore.AuthContext, room, client string, _ model.EnterpriseRoomIntent) (sqlstore.Result, error) {
	return b.record(auth, room, client)
}
func (b *fakeBackend) CommitRoomMembership(_ context.Context, cmd sqlstore.RoomMembershipCommand) (sqlstore.Result, error) {
	return b.record(cmd.Auth, cmd.RoomID, cmd.ClientID)
}
func (b *fakeBackend) ResolveRoomMembership(_ context.Context, auth sqlstore.AuthContext, room, client string, _ sqlstore.RoomMembershipIntent) (sqlstore.Result, error) {
	return b.record(auth, room, client)
}
func (b *fakeBackend) ReadonlySnapshot(_ context.Context, auth sqlstore.AuthContext, room string) (sqlstore.RoomSnapshot, error) {
	_, _ = b.record(auth, room, "")
	intent, err := model.PrepareEnterpriseRoomIntent("Room", model.Collaboration{})
	if err != nil {
		return sqlstore.RoomSnapshot{}, err
	}
	event, err := model.NewEnterpriseRoomCreation(room, "deployment-1", "organization-1", "user-1", intent, time.Now().UTC())
	if err != nil {
		return sqlstore.RoomSnapshot{}, err
	}
	return sqlstore.RoomSnapshot{DeploymentID: "deployment-1", OrgID: "organization-1", CreationUserID: "user-1", RoomID: room, HostMode: "native", Seq: 1, Revision: 1, Events: []model.Event{event}}, b.readErr
}

func newHandlerFixture(t *testing.T) (*Handler, *fakeProvider, *fakeBackend) {
	t.Helper()
	provider := &fakeProvider{completion: enterpriseauth.Completion{Identity: enterpriseauth.VerifiedIdentity{ConnectionID: "connection-1", Issuer: "https://idp.example.test", Subject: "subject-1"}, Purpose: enterpriseauth.PurposeLogin, ReturnPath: "/"}}
	backend := newFakeBackend()
	store, err := enterpriseauth.NewMemoryStore(8)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, provider, store, backend)
	if err != nil {
		t.Fatal(err)
	}
	return handler, provider, backend
}

func request(t *testing.T, h *Handler, method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, serviceOrigin+path, strings.NewReader(body))
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", serviceOrigin)
		if csrf != "" {
			r.Header.Set(CSRFHeader, csrf)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func loginBrowser(t *testing.T, h *Handler) *http.Cookie {
	t.Helper()
	w := request(t, h, http.MethodGet, "/enterprise/auth/login?return=%2F", "", nil, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("begin: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("begin cookies %d", len(cookies))
	}
	return cookies[0]
}

func loginSession(t *testing.T, h *Handler) *http.Cookie {
	t.Helper()
	browser := loginBrowser(t, h)
	w := request(t, h, http.MethodGet, "/enterprise/auth/callback?state=public-state&code=private-code", "", browser, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("callback cookies %d", len(cookies))
	}
	return cookies[0]
}

func sessionCSRF(t *testing.T, h *Handler, cookie *http.Cookie) string {
	t.Helper()
	w := request(t, h, http.MethodGet, "/enterprise/api/v1/session", "", cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("session: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !validOpaqueSecret(result.CSRF) {
		t.Fatal("session returned invalid CSRF")
	}
	return result.CSRF
}

func TestBrowserLoginIssuesPrivateSessionOnlyAfterConfirmedAdmission(t *testing.T) {
	h, p, b := newHandlerFixture(t)
	browser := loginBrowser(t, h)
	if !browser.Secure || !browser.HttpOnly || browser.Path != "/" || browser.Domain != "" || browser.SameSite != http.SameSiteLaxMode || browser.Name != h.policy.loginCookie || p.beginIntent.Purpose != enterpriseauth.PurposeLogin {
		t.Fatal("login cookie/purpose policy lost")
	}
	w := httptest.NewRecorder()
	b.onAdmit = func(cmd sqlstore.IdentityLoginCommand) {
		if len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatal("session cookie emitted before SQL admission")
		}
		if cmd.Completion.Identity.Subject != "subject-1" || cmd.Completion.Purpose != enterpriseauth.PurposeLogin || !validID(cmd.ClientID) {
			t.Fatal("verified identity or stable login operation lost")
		}
	}
	r := httptest.NewRequest(http.MethodGet, serviceOrigin+"/enterprise/auth/callback?state=public-state&code=private-code", nil)
	r.AddCookie(browser)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("callback emitted extra/readable cookies")
	}
	cookie := cookies[0]
	if cookie.Name != h.policy.sessionCookie || !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/" || cookie.Domain != "" || cookie.SameSite != http.SameSiteLaxMode || tokenHash(cookie.Value) != b.lastCandidate.TokenHash {
		t.Fatal("session credential not bound to durable hash")
	}
	csrf := sessionCSRF(t, h, cookie)
	if tokenHash(csrf) != b.lastCandidate.CSRFHash || csrf == cookie.Value {
		t.Fatal("CSRF is not separately derived and bound")
	}
	response := request(t, h, http.MethodGet, "/enterprise/api/v1/session", "", cookie, "")
	if strings.Contains(response.Body.String(), cookie.Value) || strings.Contains(response.Body.String(), "CSRFHash") || strings.Contains(response.Body.String(), "SessionID") {
		t.Fatal("session response exposed private authority")
	}
	for _, rec := range []*httptest.ResponseRecorder{w, response} {
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatal("credential-bearing browser boundary allows caching/referrers")
		}
	}
}

func TestCallbackFailureNeverSetsApplicationSessionCookie(t *testing.T) {
	for _, outcome := range []sqlstore.Outcome{sqlstore.OutcomeRefused, sqlstore.OutcomeUnknown, sqlstore.OutcomeConflict, sqlstore.OutcomeNotCommitted} {
		t.Run(string(outcome), func(t *testing.T) {
			h, _, b := newHandlerFixture(t)
			browser := loginBrowser(t, h)
			b.admitOutcome = outcome
			b.admitErr = errors.New("private-database-password")
			w := request(t, h, http.MethodGet, "/enterprise/auth/callback?state=public-state&code=private-code", "", browser, "")
			if w.Code < 400 || len(w.Result().Cookies()) != 0 || strings.Contains(w.Body.String(), "private-") {
				t.Fatalf("unsafe callback failure: %d %s", w.Code, w.Body.String())
			}
			if b.admitCalls != 1 {
				t.Fatal("admission retried")
			}
		})
	}
	h, _, b := newHandlerFixture(t)
	browser := loginBrowser(t, h)
	b.corruptReceipt = true
	w := request(t, h, http.MethodGet, "/enterprise/auth/callback?state=public-state&code=code", "", browser, "")
	if w.Code != 503 || len(w.Result().Cookies()) != 0 {
		t.Fatal("mismatched receipt authorized cookie")
	}
}

func TestPlainLoginCannotConsumeReauthenticationAsAccountLogin(t *testing.T) {
	h, p, b := newHandlerFixture(t)
	browser := loginBrowser(t, h)
	p.completion.Purpose = enterpriseauth.PurposeReauthenticate
	p.completion.FreshReauthentication = true
	w := request(t, h, http.MethodGet, "/enterprise/auth/callback?state=public-state&code=code", "", browser, "")
	if w.Code != http.StatusUnauthorized || b.admitCalls != 0 || len(w.Result().Cookies()) != 0 {
		t.Fatal("reauthentication was promoted to generic account login")
	}
}

func TestProviderDeniedCallbackConsumesStateWithoutAdmission(t *testing.T) {
	h, p, b := newHandlerFixture(t)
	browser := loginBrowser(t, h)
	w := request(t, h, http.MethodGet, "/enterprise/auth/callback?state=public-state&error=access_denied&error_description=private-description", "", browser, "")
	if w.Code != http.StatusUnauthorized || p.completeCalls != 1 || p.completeCode != "" || b.admitCalls != 0 || strings.Contains(w.Body.String(), "private-") {
		t.Fatal("denied callback did not consume safely")
	}
}

func TestSessionAndCSRFSurviveHandlerReconstructionWithoutNewSecrets(t *testing.T) {
	h, p, b := newHandlerFixture(t)
	cookie := loginSession(t, h)
	before := sessionCSRF(t, h, cookie)
	// This tests HTTP reconstruction only. The actual database restart test is
	// owned by sqlstore, which supplies the persistent Backend in production.
	store, _ := enterpriseauth.NewMemoryStore(8)
	replacement, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, p, store, b)
	if err != nil {
		t.Fatal(err)
	}
	if after := sessionCSRF(t, replacement, cookie); before != after {
		t.Fatal("handler restart changed session-derived CSRF")
	}
	if b.admitCalls != 1 {
		t.Fatal("page refresh silently minted another session")
	}
}

func creationBody(t *testing.T) string {
	t.Helper()
	intent, err := model.PrepareEnterpriseRoomIntent("Room One", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(createRequest{RoomID: "room-1", ClientID: "create-1", Intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestRoomOperationsPreserveFencedSessionAndStableIntent(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	body := creationBody(t)
	for _, entry := range []struct{ method, path, body string }{
		{http.MethodPost, "/enterprise/api/v1/rooms", body},
		{http.MethodGet, "/enterprise/api/v1/rooms/room-1", ""},
		{http.MethodPost, "/enterprise/api/v1/rooms/room-1/resolve-create", strings.Replace(body, `"room_id":"room-1",`, "", 1)},
		{http.MethodPost, "/enterprise/api/v1/rooms/room-1/memberships", `{"client_id":"member-1","expected_revision":1,"intent":{"target_user_id":"user-2","action":"admit","role":"member"}}`},
		{http.MethodPost, "/enterprise/api/v1/rooms/room-1/resolve-membership", `{"client_id":"member-1","intent":{"target_user_id":"user-2","action":"admit","role":"member"}}`},
	} {
		w := request(t, h, entry.method, entry.path, entry.body, cookie, csrf)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", entry.path, w.Code, w.Body.String())
		}
		if b.lastAuth.Deployment != "deployment-1" || b.lastAuth.UserID != "user-1" || b.lastAuth.SessionID != b.lastCandidate.SessionID || b.lastRoomID != "room-1" {
			t.Fatal("request dropped or relabeled fenced session")
		}
	}
}

func TestMutationsRequireExactOriginAndSessionCSRF(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	body := creationBody(t)
	for _, test := range []struct{ name, origin, csrf string }{
		{"missing_origin", "", csrf}, {"wrong_origin", "https://attacker.example.test", csrf}, {"null_origin", "null", csrf}, {"missing_csrf", serviceOrigin, ""}, {"wrong_csrf", serviceOrigin, strings.Repeat("A", 43)},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, serviceOrigin+"/enterprise/api/v1/rooms", strings.NewReader(body))
			r.AddCookie(cookie)
			r.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			if test.csrf != "" {
				r.Header.Set(CSRFHeader, test.csrf)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status=%d", w.Code)
			}
		})
	}
	if b.roomCalls != 0 {
		t.Fatal("CSRF rejection performed Room effect")
	}
}

func TestSessionMismatchAndRevocationPreventEveryRoomOperation(t *testing.T) {
	for _, mode := range []string{"revoked", "wrong_deployment", "empty_session", "wrong_csrf_hash", "expired"} {
		t.Run(mode, func(t *testing.T) {
			h, _, b := newHandlerFixture(t)
			cookie := loginSession(t, h)
			csrf := sessionCSRF(t, h, cookie)
			hash := tokenHash(cookie.Value)
			session := b.sessions[hash]
			switch mode {
			case "revoked":
				delete(b.sessions, hash)
			case "wrong_deployment":
				session.Auth.Deployment = "deployment-2"
			case "empty_session":
				session.Auth.SessionID = ""
			case "wrong_csrf_hash":
				session.CSRFHash = [32]byte{}
			case "expired":
				session.ExpiresAt = time.Now().Add(-time.Second)
			}
			if mode != "revoked" {
				b.sessions[hash] = session
			}
			w := request(t, h, http.MethodPost, "/enterprise/api/v1/rooms", creationBody(t), cookie, csrf)
			if w.Code != http.StatusUnauthorized || b.roomCalls != 0 || strings.Contains(w.Body.String(), "private-") {
				t.Fatalf("invalid session admitted: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestUnknownRoomMutationDoesNotExposePartialFactOrRetry(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	b.operation = sqlstore.Result{Outcome: sqlstore.OutcomeUnknown, Class: sqlstore.ClassCommitUncertain, Fact: model.Event{ID: "private-partial-fact"}, Seq: 9, Revision: 9}
	b.operationErr = errors.New("private-database-error")
	w := request(t, h, http.MethodPost, "/enterprise/api/v1/rooms", creationBody(t), cookie, csrf)
	if w.Code != http.StatusServiceUnavailable || strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), `"seq"`) || b.roomCalls != 1 {
		t.Fatalf("unsafe unknown result: %d %s", w.Code, w.Body.String())
	}
	b.readErr = &sqlstore.KernelError{Outcome: sqlstore.OutcomeRefused, Class: sqlstore.ClassAuthorization, Err: errors.New("private-database-error")}
	w = request(t, h, http.MethodGet, "/enterprise/api/v1/rooms/room-1", "", cookie, "")
	if w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "private-") {
		t.Fatalf("partial denied read leaked: %d %s", w.Code, w.Body.String())
	}
}

func TestLogoutRevokesOnlyAuthenticatedSessionWithExplicitOperation(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	w := request(t, h, http.MethodPost, "/enterprise/api/v1/session/logout", `{"client_id":"logout-1"}`, cookie, csrf)
	if w.Code != http.StatusOK || b.revokeCalls != 1 || b.lastAuth.SessionID != b.lastCandidate.SessionID || b.lastClientID != "logout-1" {
		t.Fatalf("logout: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge != -1 || cookies[0].Name != h.policy.sessionCookie {
		t.Fatal("confirmed logout did not clear credential")
	}
	if got := request(t, h, http.MethodGet, "/enterprise/api/v1/session", "", cookie, ""); got.Code != http.StatusUnauthorized {
		t.Fatal("revoked browser session accepted")
	}
}

func TestUnknownLogoutDoesNotClaimCredentialRevocation(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	b.operation = sqlstore.Result{Outcome: sqlstore.OutcomeUnknown, Class: sqlstore.ClassCommitUncertain}
	b.operationErr = errors.New("private")
	w := request(t, h, http.MethodPost, "/enterprise/api/v1/session/logout", `{"client_id":"logout-1"}`, cookie, csrf)
	if w.Code != http.StatusServiceUnavailable || len(w.Result().Cookies()) != 0 || b.revokeCalls != 1 {
		t.Fatal("uncertain revocation erased recovery credential or retried")
	}
}

func TestHTTPRejectsAuthorityFieldsDuplicateJSONAndQueryCredentials(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	body := creationBody(t)
	for _, bad := range []string{strings.Replace(body, `"room_id"`, `"Auth":{"UserID":"user-evil"},"room_id"`, 1), strings.Replace(body, `"client_id":"create-1"`, `"client_id":"first","client_id":"second"`, 1), strings.Replace(body, `"client_id"`, `"Client_ID"`, 1), strings.Replace(body, `"version"`, `"verſion"`, 1), body + body, strings.Repeat(" ", maxRequestBytes) + body} {
		w := request(t, h, http.MethodPost, "/enterprise/api/v1/rooms", bad, cookie, csrf)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("bad JSON status=%d", w.Code)
		}
	}
	for _, path := range []string{"/enterprise/api/v1/session?token=private", "/enterprise/api/v1/session?csrf=private", "/enterprise/auth/login?issuer=https://attacker.test", "/enterprise/auth/callback?state=a&state=b&code=c"} {
		w := request(t, h, http.MethodGet, path, "", cookie, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("bad query %s status=%d", path, w.Code)
		}
	}
	if b.roomCalls != 0 {
		t.Fatal("invalid request reached mutation backend")
	}
}

func TestHTTPOriginHostTLSAndProxyAuthorityAreExplicit(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	for _, test := range []struct {
		name string
		edit func(*http.Request)
	}{
		{"host", func(r *http.Request) { r.Host = "attacker.example.test" }},
		{"plaintext", func(r *http.Request) { r.TLS = nil; r.URL.Scheme = "" }},
		{"forged_proxy", func(r *http.Request) {
			r.TLS = nil
			r.URL.Scheme = ""
			r.RemoteAddr = "127.0.0.1:12345"
			r.Header.Set("X-Forwarded-Proto", "https")
		}},
		{"management_bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer local-management-secret") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, serviceOrigin+"/enterprise/auth/login", nil)
			test.edit(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
				t.Fatal("untrusted request authority accepted")
			}
		})
	}
	if b.authenticateCalls != 0 {
		t.Fatal("untrusted authority reached authentication")
	}
	proxyCfg := Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin, TrustedProxyPrefixes: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	policy, err := makeHTTPPolicy(proxyCfg)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "http://service.example.test/enterprise/auth/login", nil)
	r.URL.Scheme = ""
	r.RemoteAddr = "127.0.0.1:44321"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "attacker.example.test")
	if !policy.accepts(r) {
		t.Fatal("explicit TLS proxy refused despite preserved Host")
	}
	r.Host = "attacker.example.test"
	if policy.accepts(r) {
		t.Fatal("trusted proxy could override canonical Host")
	}
	r.Host = "service.example.test"
	r.Header.Set("X-Forwarded-Proto", "https,http")
	if policy.accepts(r) {
		t.Fatal("ambiguous proxy chain accepted")
	}
}

func TestHTTPConfigRequiresSessionFenceAndDeploymentCookieNamespace(t *testing.T) {
	h, p, b := newHandlerFixture(t)
	b.sessionRequired = false
	if _, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, p, h.transactions, b); !errors.Is(err, ErrConfiguration) {
		t.Fatal("fixture-only SQL auth admitted into public handler")
	}
	for _, cfg := range []Config{{PublicOrigin: serviceOrigin}, {DeploymentID: "deployment-1", PublicOrigin: "http://service.example.test"}, {DeploymentID: "deployment-1", PublicOrigin: serviceOrigin + "/"}, {DeploymentID: "deployment-1", PublicOrigin: serviceOrigin, TrustedProxyPrefixes: []netip.Prefix{netip.MustParsePrefix("0.0.0.0/0")}}} {
		if _, err := makeHTTPPolicy(cfg); err == nil {
			t.Fatal("invalid authority policy accepted")
		}
	}
	other, err := makeHTTPPolicy(Config{DeploymentID: "deployment-2", PublicOrigin: serviceOrigin})
	if err != nil {
		t.Fatal(err)
	}
	if other.sessionCookie == h.policy.sessionCookie || other.loginCookie == h.policy.loginCookie {
		t.Fatal("deployments share cookie authority")
	}
	secret, _ := enterpriseauth.NewBrowserSecret()
	a := deriveCSRF(secret, "deployment-1", "session-1")
	if a == deriveCSRF(secret, "deployment-2", "session-1") || a == deriveCSRF(secret, "deployment-1", "session-2") {
		t.Fatal("CSRF omitted deployment/session binding")
	}
}

func TestDuplicateSessionCookiesFailBeforeBackend(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	cookie := loginSession(t, h)
	before := b.authenticateCalls
	r := httptest.NewRequest(http.MethodGet, serviceOrigin+"/enterprise/api/v1/session", nil)
	r.AddCookie(cookie)
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized || b.authenticateCalls != before {
		t.Fatal("ambiguous cookie admitted")
	}
}

func TestCSRFTokensAreNotAcceptedAcrossSessions(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	first := loginSession(t, h)
	firstCSRF := sessionCSRF(t, h, first)
	second := loginSession(t, h)
	w := request(t, h, http.MethodPost, "/enterprise/api/v1/rooms", creationBody(t), second, firstCSRF)
	if w.Code != http.StatusForbidden || b.roomCalls != 0 {
		t.Fatal("another session's CSRF proof accepted")
	}
}

func TestResponseDoesNotSerializeBackendErrors(t *testing.T) {
	w := httptest.NewRecorder()
	writeOperation(w, sqlstore.Result{Outcome: sqlstore.OutcomeUnknown, Err: fmt.Errorf("private-token"), Class: sqlstore.ClassCommitUncertain}, errors.New("private-dsn"))
	if bytes.Contains(w.Body.Bytes(), []byte("private-")) {
		t.Fatal("backend error escaped HTTP boundary")
	}
}

func TestQueryParserDoesNotSilentlyDropMalformedParameters(t *testing.T) {
	if _, ok := strictQuery("state=a;token=b&code=c", "state", "code"); ok {
		t.Fatal("malformed query accepted")
	}
	if _, ok := strictQuery("state="+url.QueryEscape("a")+"&code=b", "state", "code"); !ok {
		t.Fatal("valid protocol query rejected")
	}
}

func TestCanonicalHTTPSOriginRejectsAmbiguousHostSpelling(t *testing.T) {
	for _, origin := range []string{"https://service.example.test:443", "https://service.example.test:", "https://service.example.test:99999", "https://SERVICE.example.test", "https://身份.example.test", "https://[fe80::1%25eth0]"} {
		if _, err := makeHTTPPolicy(Config{DeploymentID: "deployment-1", PublicOrigin: origin}); !errors.Is(err, ErrConfiguration) {
			t.Fatalf("ambiguous origin accepted: %s", origin)
		}
	}
	for _, origin := range []string{serviceOrigin, "https://service.example.test:8443", "https://127.0.0.1:8443", "https://[::1]:8443"} {
		if _, err := makeHTTPPolicy(Config{DeploymentID: "deployment-1", PublicOrigin: origin}); err != nil {
			t.Fatalf("canonical origin refused: %s", origin)
		}
	}
}

func TestDefaultLoginReturnMatchesConfiguredEnterpriseProvider(t *testing.T) {
	var idp *httptest.Server
	idp = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize", "token_endpoint": idp.URL + "/token", "jwks_uri": idp.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"}})
	}))
	defer idp.Close()
	pool := x509.NewCertPool()
	pool.AddCert(idp.Certificate())
	provider, err := enterpriseauth.NewProvider(context.Background(), enterpriseauth.ProviderConfig{ConnectionID: "connection-1", Issuer: idp.URL, ClientID: "client-1", RedirectURL: serviceOrigin + "/enterprise/auth/callback", AuthStyle: oauth2.AuthStyleInHeader, AllowedReturnPaths: []string{"/enterprise/"}, Outbound: enterpriseauth.OutboundPolicy{AllowedOrigins: []string{idp.URL}, RootCAs: pool}})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	transactions, _ := enterpriseauth.NewMemoryStore(2)
	handler, err := New(Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}, provider, transactions, newFakeBackend())
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, handler, http.MethodGet, "/enterprise/auth/login", "", nil, "")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("real provider refused default enterprise return: %d %s", w.Code, w.Body.String())
	}
	location, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("redirect_uri") != serviceOrigin+"/enterprise/auth/callback" {
		t.Fatal("configured callback changed")
	}
	state := location.Query().Get("state")
	browser := w.Result().Cookies()[0].Value
	tx, err := transactions.Consume(context.Background(), tokenHash(state), tokenHash(browser), time.Now())
	if err != nil || tx.Intent.ReturnPath != "/enterprise/" {
		t.Fatalf("default return not frozen: %v", err)
	}
}

func TestConstructorRejectsTypedNilDependencies(t *testing.T) {
	h, p, b := newHandlerFixture(t)
	cfg := Config{DeploymentID: "deployment-1", PublicOrigin: serviceOrigin}
	var provider *fakeProvider
	var transactions *enterpriseauth.MemoryStore
	var backend *fakeBackend
	if _, err := New(cfg, provider, h.transactions, b); !errors.Is(err, ErrConfiguration) {
		t.Fatal("typed nil provider accepted")
	}
	if _, err := New(cfg, p, transactions, b); !errors.Is(err, ErrConfiguration) {
		t.Fatal("typed nil transaction store accepted")
	}
	if _, err := New(cfg, p, h.transactions, backend); !errors.Is(err, ErrConfiguration) {
		t.Fatal("typed nil backend accepted")
	}
	if err := ValidateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.PublicOrigin = "http://service.example.test"
	if !errors.Is(ValidateConfig(cfg), ErrConfiguration) {
		t.Fatal("preflight config accepted HTTP origin")
	}
}

func TestLandingIsFunctionalWithoutExposingAuthenticationState(t *testing.T) {
	h, _, b := newHandlerFixture(t)
	for _, path := range []string{"/enterprise/", "/enterprise/assets/app.js", "/enterprise/assets/style.css"} {
		w := request(t, h, http.MethodGet, path, "", nil, "")
		if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("landing resource %s: %d", path, w.Code)
		}
		if strings.Contains(w.Header().Get("Content-Security-Policy"), "unsafe-inline") {
			t.Fatal("landing permits inline code")
		}
		// The bounded recovery journal intentionally persists noncredential
		// command metadata. Actual JS behavior tests verify its field allowlist,
		// account/deployment isolation, and zero implicit resend after reload.
	}
	if b.authenticateCalls != 0 {
		t.Fatal("public static shell disclosed session state")
	}
	w := request(t, h, http.MethodGet, "/enterprise/", "", nil, "")
	if !strings.Contains(w.Body.String(), `href="/enterprise/auth/login"`) || !strings.Contains(w.Body.String(), `id="room-form"`) {
		t.Fatal("landing has no usable sign-in/Room entry")
	}
}

func TestMutationReceiptNeverSerializesPrivateFactData(t *testing.T) {
	w := httptest.NewRecorder()
	writeOperation(w, sqlstore.Result{Outcome: sqlstore.OutcomeCommitted, Seq: 2, Revision: 2, Fact: model.Event{ID: "event-2", RoomID: "room-1", Kind: "native.message.updated", Data: json.RawMessage(`{"receipt":"private-capability","client_key":"private-operation-key"}`)}}, nil)
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), `"data"`) || !strings.Contains(w.Body.String(), "event-2") {
		t.Fatalf("unsafe public receipt: %s", w.Body.String())
	}
}
