package enterpriseservice

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
	"golang.org/x/oauth2"
)

// These tests run real TLS HTTP, the maintained OIDC verifier and PostgreSQL.
// The IdP is synthetic and the client is a cookie jar, not a browser or vendor
// session. No test replaces the SQL Backend or directly fabricates Completion.
type httpPGCode struct{ subject, nonce, challenge, redirect string }
type httpPGIDP struct {
	server         *httptest.Server
	key            *ecdsa.PrivateKey
	online         atomic.Bool
	discoveryCalls atomic.Int32
	tokenCalls     atomic.Int32
	mu             sync.Mutex
	callback       string
	codes          map[string]httpPGCode
	access         map[string]string
}

func httpPGCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "PairRoom HTTP integration fixture"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, roots
}

func newHTTPPGIDP(t *testing.T, cert tls.Certificate) *httpPGIDP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	idp := &httpPGIDP{key: key, codes: map[string]httpPGCode{}, access: map[string]string{}}
	idp.online.Store(true)
	idp.server = httptest.NewUnstartedServer(http.HandlerFunc(idp.serve))
	idp.server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	idp.server.StartTLS()
	t.Cleanup(idp.server.Close)
	return idp
}

func (p *httpPGIDP) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/.well-known/openid-configuration" {
		p.discoveryCalls.Add(1)
	}
	if !p.online.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": p.server.URL, "authorization_endpoint": p.server.URL + "/authorize", "token_endpoint": p.server.URL + "/token", "jwks_uri": p.server.URL + "/jwks", "userinfo_endpoint": p.server.URL + "/userinfo", "id_token_signing_alg_values_supported": []string{"ES256"}})
	case "/authorize":
		q := r.URL.Query()
		subject, err := r.Cookie("fixture_subject")
		if err != nil || q.Get("client_id") != "fixture-client" || q.Get("redirect_uri") != p.callback || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !validOpaqueSecret(q.Get("nonce")) || !validOpaqueSecret(q.Get("state")) {
			w.WriteHeader(400)
			return
		}
		code, err := enterpriseauth.NewBrowserSecret()
		if err != nil {
			w.WriteHeader(500)
			return
		}
		p.codes[code] = httpPGCode{subject: subject.Value, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: p.callback}
		callback, _ := url.Parse(p.callback)
		callback.RawQuery = url.Values{"state": {q.Get("state")}, "code": {code}}.Encode()
		http.Redirect(w, r, callback.String(), http.StatusSeeOther)
	case "/token":
		p.tokenCalls.Add(1)
		_ = r.ParseForm()
		client, secret, ok := r.BasicAuth()
		code, exists := p.codes[r.Form.Get("code")]
		delete(p.codes, r.Form.Get("code"))
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || client != "fixture-client" || secret != "fixture-client-secret" || !exists || r.Form.Get("redirect_uri") != code.redirect || r.Form.Get("grant_type") != "authorization_code" || base64.RawURLEncoding.EncodeToString(challenge[:]) != code.challenge {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		now := time.Now().Unix()
		claims := map[string]any{"iss": p.server.URL, "sub": code.subject, "aud": "fixture-client", "exp": now + 300, "iat": now, "auth_time": now, "nonce": code.nonce, "email": "operator@example.test", "admin": true, "roles": []string{"administrator"}}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: p.key}, (&jose.SignerOptions{}).WithHeader("kid", "fixture-signing-key"))
		if err != nil {
			w.WriteHeader(500)
			return
		}
		payload, _ := json.Marshal(claims)
		signed, err := signer.Sign(payload)
		if err != nil {
			w.WriteHeader(500)
			return
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			w.WriteHeader(500)
			return
		}
		access := "access-" + r.Form.Get("code")
		p.access[access] = code.subject
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 300, "id_token": raw})
	case "/jwks":
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &p.key.PublicKey, KeyID: "fixture-signing-key", Algorithm: "ES256", Use: "sig"}}})
	case "/userinfo":
		subject, ok := p.access[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": subject, "email": "operator@example.test"})
	default:
		w.WriteHeader(404)
	}
}

type httpPGFixture struct {
	dsn, schema, origin string
	cfg                 sqlstore.Config
	admin               *pgx.Conn
	store               *sqlstore.Store
	idp                 *httpPGIDP
	roots               *x509.CertPool
	server              *httptest.Server
	handler             atomic.Pointer[Handler]
	providerConfig      enterpriseauth.ProviderConfig
	provider            *enterpriseauth.RecoveringProvider
	t                   *testing.T
}

func newHTTPPGFixture(t *testing.T) *httpPGFixture {
	t.Helper()
	dsn := sqltest.RequireDSN(t)
	cert, roots := httpPGCertificate(t)
	idp := newHTTPPGIDP(t, cert)
	schema, _ := sqltest.CreateSchema(t, dsn)
	cfg := sqlstore.Config{DSN: dsn, Schema: schema, DeploymentID: "dep-http-" + strings.TrimPrefix(schema, "sqltest_"), RequireSessionAuth: true}
	if err := sqlstore.InitializeDeployment(context.Background(), cfg, "org-http"); err != nil {
		t.Fatal(err)
	}
	store, err := sqlstore.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	identity := func(subject string) enterpriseauth.VerifiedIdentity {
		return enterpriseauth.VerifiedIdentity{ConnectionID: "fixture-connection", Issuer: idp.server.URL, Subject: subject}
	}
	result, err := store.InitializeIdentity(context.Background(), sqlstore.IdentityConfig{Connections: []sqlstore.TrustConnectionConfig{{ConnectionID: "fixture-connection", Issuer: idp.server.URL}}, Bootstrap: identity("operator"), Admitted: []enterpriseauth.VerifiedIdentity{identity("member-one"), identity("member-two")}})
	if err != nil || result.Outcome != sqlstore.OutcomeCommitted {
		t.Fatalf("initialize identity: outcome=%s error=%v", result.Outcome, err)
	}
	f := &httpPGFixture{dsn: dsn, schema: schema, cfg: cfg, store: store, idp: idp, roots: roots, t: t}
	f.admin = sqltest.Connect(t, dsn, schema)
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := f.handler.Load()
		if h == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		h.ServeHTTP(w, r)
	}))
	f.server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	_, port, err := net.SplitHostPort(f.server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	f.origin = "https://localhost:" + port
	idp.mu.Lock()
	idp.callback = f.origin + "/enterprise/auth/callback"
	idp.mu.Unlock()
	f.providerConfig = enterpriseauth.ProviderConfig{ConnectionID: "fixture-connection", Issuer: idp.server.URL, ClientID: "fixture-client", ClientSecret: "fixture-client-secret", RedirectURL: idp.callback, AuthStyle: oauth2.AuthStyleInHeader, SigningAlgorithms: []string{"ES256"}, AllowedReturnPaths: []string{"/enterprise/"}, UseUserInfo: true, Outbound: enterpriseauth.OutboundPolicy{AllowedOrigins: []string{idp.server.URL}, RootCAs: roots, Timeout: 3 * time.Second}}
	f.installHandler(store)
	f.server.Listener, err = LimitListener(f.server.Listener, 256)
	if err != nil {
		t.Fatal(err)
	}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

func (f *httpPGFixture) installHandler(store *sqlstore.Store) {
	f.t.Helper()
	if err := store.CheckIdentityConfiguration(context.Background(), "org-http", f.providerConfig.ConnectionID, f.providerConfig.Issuer); err != nil {
		f.t.Fatal(err)
	}
	provider, err := enterpriseauth.NewRecoveringProvider(context.Background(), f.providerConfig)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(provider.Close)
	transactions, err := enterpriseauth.NewMemoryStore(16)
	if err != nil {
		f.t.Fatal(err)
	}
	handler, err := New(Config{DeploymentID: f.cfg.DeploymentID, PublicOrigin: f.origin}, provider, transactions, store)
	if err != nil {
		f.t.Fatal(err)
	}
	f.provider = provider
	f.handler.Store(handler)
}

func (f *httpPGFixture) browser(subject string) *http.Client {
	f.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		f.t.Fatal(err)
	}
	idpURL, _ := url.Parse(f.idp.server.URL)
	jar.SetCookies(idpURL, []*http.Cookie{{Name: "fixture_subject", Value: subject, Path: "/", Secure: true, HttpOnly: true}})
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.roots.Clone(), MinVersion: tls.VersionTLS12}}
	f.t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Jar: jar, Timeout: 15 * time.Second}
}

func (f *httpPGFixture) get(client *http.Client, path string) (int, []byte) {
	f.t.Helper()
	response, err := client.Get(f.origin + path)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		f.t.Fatal(err)
	}
	return response.StatusCode, body
}

func (f *httpPGFixture) login(client *http.Client) {
	f.t.Helper()
	status, body := f.get(client, "/enterprise/auth/login")
	if status != http.StatusOK || !bytes.Contains(body, []byte("Your shared Rooms.")) {
		f.t.Fatalf("full OIDC/browser redirect did not reach landing: %d %s", status, body)
	}
}

type httpPGSession struct {
	UserID        string `json:"user_id"`
	Administrator bool   `json:"administrator"`
	CSRF          string `json:"csrf"`
}

func (f *httpPGFixture) session(client *http.Client) httpPGSession {
	f.t.Helper()
	status, body := f.get(client, "/enterprise/api/v1/session")
	if status != 200 {
		f.t.Fatalf("HTTP session: %d %s", status, body)
	}
	var result httpPGSession
	if err := json.Unmarshal(body, &result); err != nil {
		f.t.Fatal(err)
	}
	if result.UserID == "" || !validOpaqueSecret(result.CSRF) {
		f.t.Fatal("HTTP session missing identity/CSRF")
	}
	return result
}

func (f *httpPGFixture) post(client *http.Client, path string, value any, csrf string) (int, []byte) {
	f.t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, f.origin+path, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", f.origin)
	if csrf != "" {
		request.Header.Set(CSRFHeader, csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		f.t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		f.t.Fatal(err)
	}
	return response.StatusCode, data
}

func (f *httpPGFixture) createRoom(client *http.Client, csrf, roomID string) {
	f.t.Helper()
	intent, err := model.PrepareEnterpriseRoomIntent("Private Room", model.Collaboration{})
	if err != nil {
		f.t.Fatal(err)
	}
	status, body := f.post(client, "/enterprise/api/v1/rooms", createRequest{RoomID: roomID, ClientID: "create-" + roomID, Intent: intent}, csrf)
	if status != 200 {
		f.t.Fatalf("HTTP Room creation: %d %s", status, body)
	}
}

func (f *httpPGFixture) cookie(client *http.Client) *http.Cookie {
	f.t.Helper()
	origin, _ := url.Parse(f.origin)
	name := f.handler.Load().policy.sessionCookie
	for _, cookie := range client.Jar.Cookies(origin) {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

func TestHTTPPGOIDCAdmissionCookieAuditAndCrossUserRoomBoundary(t *testing.T) {
	f := newHTTPPGFixture(t)
	member := f.browser("member-one")
	f.login(member)
	memberSession := f.session(member)
	if memberSession.Administrator {
		t.Fatal("first login or administrator claims granted an administrator role")
	}
	cookie := f.cookie(member)
	if cookie == nil {
		t.Fatal("confirmed login did not issue session cookie")
	}
	var sessions, audits int
	hash := tokenHash(cookie.Value)
	if err := f.admin.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM browser_sessions WHERE token_hash=$1), (SELECT count(*) FROM audit WHERE kind='identity.login')`, hash[:]).Scan(&sessions, &audits); err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || audits != 1 {
		t.Fatalf("cookie preceded durable session/audit: %d/%d", sessions, audits)
	}
	f.createRoom(member, memberSession.CSRF, "room-private")
	status, body := f.get(member, "/enterprise/api/v1/rooms/room-private")
	if status != 200 || bytes.Contains(body, []byte(`"Events"`)) || bytes.Contains(body, []byte(`"events"`)) || bytes.Contains(body, []byte(`"Data"`)) {
		t.Fatalf("Room response did not use public projection: %d %s", status, body)
	}
	other := f.browser("member-two")
	f.login(other)
	otherSession := f.session(other)
	if otherSession.UserID == memberSession.UserID || otherSession.Administrator {
		t.Fatal("equal email/admin claims merged identities or granted authority")
	}
	status, body = f.get(other, "/enterprise/api/v1/rooms/room-private")
	if status != 403 || bytes.Contains(body, []byte("Private Room")) {
		t.Fatalf("cross-user private Room disclosed: %d %s", status, body)
	}
	intent, _ := model.PrepareEnterpriseRoomIntent("Blocked", model.Collaboration{})
	status, _ = f.post(other, "/enterprise/api/v1/rooms", createRequest{RoomID: "room-wrong-csrf", ClientID: "wrong-csrf", Intent: intent}, memberSession.CSRF)
	if status != 403 {
		t.Fatalf("cross-user CSRF accepted: %d", status)
	}
	var count int
	if err := f.admin.QueryRow(context.Background(), `SELECT count(*) FROM rooms WHERE room_id='room-wrong-csrf'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rejected CSRF produced a Room")
	}
	operator := f.browser("operator")
	f.login(operator)
	if !f.session(operator).Administrator {
		t.Fatal("operator-selected bootstrap identity lost administrator role")
	}
}

func TestHTTPPGUnadmittedIdentityAndAuditFailureIssueNoSessionCookie(t *testing.T) {
	f := newHTTPPGFixture(t)
	unadmitted := f.browser("not-admitted")
	status, _ := f.get(unadmitted, "/enterprise/auth/login")
	if status != 403 || f.cookie(unadmitted) != nil {
		t.Fatal("unadmitted IdP profile received an application session")
	}
	var beforeUsers, beforeSessions int
	if err := f.admin.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM browser_sessions)`).Scan(&beforeUsers, &beforeSessions); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(context.Background(), `CREATE FUNCTION reject_http_login_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind = 'identity.login' THEN RAISE EXCEPTION 'synthetic audit rejection'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_http_login_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION reject_http_login_audit()`); err != nil {
		t.Fatal(err)
	}
	member := f.browser("member-one")
	status, body := f.get(member, "/enterprise/auth/login")
	if status < 400 || f.cookie(member) != nil || bytes.Contains(body, []byte("synthetic audit rejection")) {
		t.Fatalf("failed audit issued session or leaked SQL error: %d %s", status, body)
	}
	var afterUsers, afterSessions int
	if err := f.admin.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM users),(SELECT count(*) FROM browser_sessions)`).Scan(&afterUsers, &afterSessions); err != nil {
		t.Fatal(err)
	}
	if beforeUsers != afterUsers || beforeSessions != afterSessions {
		t.Fatal("audit failure left partial identity/session state")
	}
	if f.idp.tokenCalls.Load() != 2 {
		t.Fatal("callback automatically retried authorization-code exchange")
	}
}

func TestHTTPPGSessionSurvivesStoreRestartAndIdPOutageThenLogoutRevokes(t *testing.T) {
	f := newHTTPPGFixture(t)
	browser := f.browser("member-one")
	f.login(browser)
	before := f.session(browser)
	f.createRoom(browser, before.CSRF, "room-restart")
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.provider.Close()
	f.idp.online.Store(false)
	successor, err := sqlstore.Open(context.Background(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = successor.Close() })
	f.store = successor
	// Reconstruct the actual service core at the same HTTPS origin while the
	// listener stays stable. This is not a process-crash or browser E2E claim.
	f.installHandler(successor)
	if f.provider.Ready() {
		t.Fatal("offline IdP unexpectedly ready")
	}
	discoveryCount := f.idp.discoveryCalls.Load()
	after := f.session(browser)
	if after.UserID != before.UserID || after.CSRF != before.CSRF {
		t.Fatal("persistent session or derived CSRF changed across service-core restart")
	}
	status, body := f.get(browser, "/enterprise/api/v1/rooms/room-restart")
	if status != 200 || !bytes.Contains(body, []byte(`"room_id":"room-restart"`)) {
		t.Fatalf("offline-IdP Room read: %d %s", status, body)
	}
	if f.idp.discoveryCalls.Load() != discoveryCount {
		t.Fatal("existing session routes contacted unavailable IdP")
	}
	oldCookie := f.cookie(browser)
	status, body = f.post(browser, "/enterprise/api/v1/session/logout", map[string]string{"client_id": "logout-restart"}, after.CSRF)
	if status != 200 {
		t.Fatalf("logout: %d %s", status, body)
	}
	if f.cookie(browser) != nil {
		t.Fatal("confirmed logout did not remove cookie")
	}
	origin, _ := url.Parse(f.origin)
	browser.Jar.SetCookies(origin, []*http.Cookie{{Name: oldCookie.Name, Value: oldCookie.Value, Path: "/", Secure: true, HttpOnly: true}})
	status, _ = f.get(browser, "/enterprise/api/v1/session")
	if status != 401 {
		t.Fatal("reintroduced revoked browser credential became active")
	}
}

func TestHTTPPGDeposedLeaseCannotServePrivateSessionOrRoom(t *testing.T) {
	f := newHTTPPGFixture(t)
	browser := f.browser("member-one")
	f.login(browser)
	session := f.session(browser)
	f.createRoom(browser, session.CSRF, "room-fenced")
	var pid int32
	if err := f.admin.QueryRow(context.Background(), `SELECT lease_pid FROM ownership WHERE id`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	var terminated bool
	if err := f.admin.QueryRow(context.Background(), `SELECT pg_catalog.pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil || !terminated {
		t.Fatal("failed to terminate test-owned lease backend")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var present bool
		if err := f.admin.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_stat_activity WHERE pid=$1)`, pid).Scan(&present); err != nil {
			t.Fatal(err)
		}
		if !present {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease backend did not terminate")
		}
		time.Sleep(10 * time.Millisecond)
	}
	successor, err := sqlstore.Open(context.Background(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = successor.Close() })
	for _, path := range []string{"/enterprise/api/v1/session", "/enterprise/api/v1/rooms/room-fenced"} {
		status, body := f.get(browser, path)
		if status != 503 || bytes.Contains(body, []byte(session.CSRF)) || bytes.Contains(body, []byte("Private Room")) {
			t.Fatalf("deposed private route %s: %d %s", path, status, body)
		}
	}
	intent, _ := model.PrepareEnterpriseRoomIntent("Blocked", model.Collaboration{})
	status, _ := f.post(browser, "/enterprise/api/v1/rooms", createRequest{RoomID: "room-after-lease-loss", ClientID: "lease-loss", Intent: intent}, session.CSRF)
	if status != 503 {
		t.Fatal("deposed service admitted Room mutation")
	}
	f.installHandler(successor)
	status, _ = f.get(browser, "/enterprise/api/v1/rooms/room-fenced")
	if status != 200 {
		t.Fatal("successor could not serve still-live application session")
	}
}
