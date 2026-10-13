package enterpriseauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

// This fixture performs real TLS discovery, browser authorization redirects,
// PKCE code exchange and ES256 verification. Mode 1 deliberately serves an
// unsigned JWT; mode 2 signs a same-origin issuer with the configured path lost.
// It owns no SQL admission or application-cookie assertion.
type acceptanceEdgeIDP struct {
	server        *httptest.Server
	key           *ecdsa.PrivateKey
	prefix        string
	mode          atomic.Int32
	mu            sync.Mutex
	codes         map[string]struct{ nonce, challenge string }
	paths         []string
	tokenCalls    int
	userInfoCalls int
	badRequest    bool
}

func newAcceptanceEdgeIDP(t *testing.T, prefix string) *acceptanceEdgeIDP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &acceptanceEdgeIDP{key: key, prefix: prefix, codes: make(map[string]struct{ nonce, challenge string })}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

func (f *acceptanceEdgeIDP) issuer() string { return f.server.URL + f.prefix }
func (f *acceptanceEdgeIDP) config() ProviderConfig {
	pool := x509.NewCertPool()
	pool.AddCert(f.server.Certificate())
	return ProviderConfig{ConnectionID: "edge-connection", Issuer: f.issuer(), ClientID: "edge-client", ClientSecret: "edge-client-secret", RedirectURL: "https://service.example.test/auth/callback", AuthStyle: oauth2.AuthStyleInHeader, SigningAlgorithms: []string{"ES256"}, AllowedReturnPaths: []string{"/", "/rooms"}, UseUserInfo: true, Outbound: OutboundPolicy{AllowedOrigins: []string{f.server.URL}, RootCAs: pool, Timeout: 2 * time.Second, MaxResponseBytes: 8192}}
}

func (f *acceptanceEdgeIDP) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	path := strings.TrimPrefix(r.URL.Path, f.prefix)
	if f.prefix != "" && !strings.HasPrefix(r.URL.Path, f.prefix+"/") {
		w.WriteHeader(404)
		return
	}
	switch path {
	case "/.well-known/openid-configuration":
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.issuer(), "authorization_endpoint": f.issuer() + "/authorize", "token_endpoint": f.issuer() + "/token", "jwks_uri": f.issuer() + "/jwks", "userinfo_endpoint": f.issuer() + "/userinfo", "id_token_signing_alg_values_supported": []string{"ES256"}})
	case "/authorize":
		q := r.URL.Query()
		if q.Get("client_id") != "edge-client" || q.Get("redirect_uri") != "https://service.example.test/auth/callback" || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !validSecret(q.Get("state")) || !validSecret(q.Get("nonce")) || !validSecret(q.Get("code_challenge")) {
			f.badRequest = true
			w.WriteHeader(400)
			return
		}
		callback, _ := url.Parse(q.Get("redirect_uri"))
		values := url.Values{"state": {q.Get("state")}}
		if denial, err := r.Cookie("fixture_denied"); err == nil && denial.Value == "1" {
			values.Set("error", "access_denied")
		} else {
			code, err := NewBrowserSecret()
			if err != nil {
				w.WriteHeader(500)
				return
			}
			f.codes[code] = struct{ nonce, challenge string }{q.Get("nonce"), q.Get("code_challenge")}
			values.Set("code", code)
		}
		callback.RawQuery = values.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	case "/token":
		f.tokenCalls++
		_ = r.ParseForm()
		client, secret, ok := r.BasicAuth()
		code, exists := f.codes[r.Form.Get("code")]
		delete(f.codes, r.Form.Get("code"))
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || client != "edge-client" || secret != "edge-client-secret" || !exists || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != "https://service.example.test/auth/callback" || base64.RawURLEncoding.EncodeToString(challenge[:]) != code.challenge {
			f.badRequest = true
			w.WriteHeader(400)
			return
		}
		issuer := f.issuer()
		if f.mode.Load() == 2 {
			issuer = f.server.URL
		}
		now := time.Now().Unix()
		payload, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "edge-subject", "aud": "edge-client", "exp": now + 600, "iat": now, "nonce": code.nonce, "auth_time": now})
		var token string
		if f.mode.Load() == 1 {
			token = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + "."
		} else {
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: f.key}, (&jose.SignerOptions{}).WithHeader("kid", "edge-key"))
			if err != nil {
				w.WriteHeader(500)
				return
			}
			signed, err := signer.Sign(payload)
			if err != nil {
				w.WriteHeader(500)
				return
			}
			token, err = signed.CompactSerialize()
			if err != nil {
				w.WriteHeader(500)
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": token, "access_token": "edge-access", "token_type": "Bearer", "expires_in": 600})
	case "/jwks":
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: "edge-key", Algorithm: "ES256", Use: "sig"}}})
	case "/userinfo":
		f.userInfoCalls++
		if r.Header.Get("Authorization") != "Bearer edge-access" {
			f.badRequest = true
			w.WriteHeader(401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"sub": "edge-subject"})
	default:
		w.WriteHeader(404)
	}
}

func (f *acceptanceEdgeIDP) authorize(t *testing.T, authorization Authorization, denied bool) url.Values {
	t.Helper()
	cfg := f.config()
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: cfg.Outbound.RootCAs, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := http.NewRequest(http.MethodGet, authorization.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if denied {
		r.AddCookie(&http.Cookie{Name: "fixture_denied", Value: "1"})
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 302 {
		t.Fatalf("authorization status %d", response.StatusCode)
	}
	callback, err := url.Parse(response.Header.Get("Location"))
	if err != nil || callback.Scheme+"://"+callback.Host+callback.Path != cfg.RedirectURL {
		t.Fatal("authorization changed callback destination")
	}
	return callback.Query()
}

func edgeBegin(t *testing.T, p *Provider, store TransactionStore, browser, correlation, returnPath string) Authorization {
	t.Helper()
	authorization, err := p.Begin(t.Context(), store, browser, LoginIntent{Purpose: PurposeLogin, CorrelationID: correlation, ReturnPath: returnPath})
	if err != nil {
		t.Fatal(err)
	}
	return authorization
}

func TestOIDCAcceptanceUnsignedNoneTokenCannotProduceIdentity(t *testing.T) {
	f := newAcceptanceEdgeIDP(t, "")
	f.mode.Store(1)
	p := testProvider(t, f.config())
	store := testMemoryStore(t, 2)
	browser, _ := NewBrowserSecret()
	callback := f.authorize(t, edgeBegin(t, p, store, browser, "unsigned", "/rooms"), false)
	if completion, err := p.Complete(t.Context(), store, browser, callback.Get("state"), callback.Get("code")); !errors.Is(err, ErrIdentityInvalid) || completion != (Completion{}) {
		t.Fatal("unsigned alg=none token produced a trusted identity")
	}
	if _, err := p.Complete(t.Context(), store, browser, callback.Get("state"), callback.Get("code")); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatal("unsigned-token callback remained reusable")
	}
	f.mode.Store(0)
	callback = f.authorize(t, edgeBegin(t, p, store, browser, "signed-control", "/rooms"), false)
	completion, err := p.Complete(t.Context(), store, browser, callback.Get("state"), callback.Get("code"))
	if err != nil || completion.Identity.Subject != "edge-subject" {
		t.Fatal("valid signed control did not complete")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.badRequest || f.tokenCalls != 2 || f.userInfoCalls != 1 {
		t.Fatal("none rejection did not occur after one valid PKCE exchange or skipped signed control")
	}
}

func TestOIDCAcceptancePathIssuerCompletesAndCannotLosePath(t *testing.T) {
	f := newAcceptanceEdgeIDP(t, "/sso/tenant-a")
	p := testProvider(t, f.config())
	store := testMemoryStore(t, 2)
	browser, _ := NewBrowserSecret()
	callback := f.authorize(t, edgeBegin(t, p, store, browser, "path-issuer", "/rooms"), false)
	completion, err := p.Complete(t.Context(), store, browser, callback.Get("state"), callback.Get("code"))
	if err != nil || completion.Identity.Issuer != f.issuer() || completion.Identity.Subject != "edge-subject" || completion.CorrelationID != "path-issuer" {
		t.Fatalf("configured path-bearing issuer failed: %v", err)
	}
	f.mode.Store(2)
	callback = f.authorize(t, edgeBegin(t, p, store, browser, "lost-path", "/rooms"), false)
	if completion, err := p.Complete(t.Context(), store, browser, callback.Get("state"), callback.Get("code")); !errors.Is(err, ErrIdentityInvalid) || completion != (Completion{}) {
		t.Fatal("same-origin signed issuer without exact path accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := map[string]bool{}
	for _, path := range f.paths {
		if !strings.HasPrefix(path, "/sso/tenant-a/") {
			t.Fatal("issuer path was stripped from outbound destination")
		}
		seen[path] = true
	}
	for _, suffix := range []string{"/.well-known/openid-configuration", "/authorize", "/token", "/jwks", "/userinfo"} {
		if !seen[f.prefix+suffix] {
			t.Fatal("path-bearing flow skipped a required real TLS endpoint")
		}
	}
	if f.badRequest || f.tokenCalls != 2 || f.userInfoCalls != 1 {
		t.Fatal("path test did not verify exact signed identity after PKCE")
	}
}

func TestOIDCAcceptanceParallelLoginCancellationDoesNotConsumeOtherTab(t *testing.T) {
	f := newAcceptanceEdgeIDP(t, "")
	p := testProvider(t, f.config())
	store := testMemoryStore(t, 2)
	browser, _ := NewBrowserSecret()
	var authorizations [2]Authorization
	var errs [2]error
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range authorizations {
		i := i
		wg.Go(func() {
			<-start
			returnPath := "/rooms"
			correlation := "tab-a"
			if i == 1 {
				returnPath = "/"
				correlation = "tab-b"
			}
			authorizations[i], errs[i] = p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: returnPath, CorrelationID: correlation})
		})
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	a, _ := url.Parse(authorizations[0].URL)
	b, _ := url.Parse(authorizations[1].URL)
	for _, field := range []string{"state", "nonce", "code_challenge"} {
		if a.Query().Get(field) == b.Query().Get(field) || a.Query().Get(field) == "" || b.Query().Get(field) == "" {
			t.Fatal("parallel tabs shared frozen OIDC state")
		}
	}
	denied := f.authorize(t, authorizations[0], true)
	allowed := f.authorize(t, authorizations[1], false)
	if denied.Get("error") != "access_denied" || denied.Get("code") != "" || denied.Get("state") != a.Query().Get("state") {
		t.Fatal("fixture did not return the actual denied callback")
	}
	if completion, err := p.Complete(t.Context(), store, browser, denied.Get("state"), denied.Get("code")); !errors.Is(err, ErrIdentityInvalid) || completion != (Completion{}) {
		t.Fatal("canceled tab produced identity")
	}
	if _, err := p.Complete(t.Context(), store, browser, denied.Get("state"), ""); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatal("canceled tab state was not consumed")
	}
	f.mu.Lock()
	before := f.tokenCalls
	f.mu.Unlock()
	if before != 0 {
		t.Fatal("canceling tab contacted token endpoint")
	}
	completion, err := p.Complete(t.Context(), store, browser, allowed.Get("state"), allowed.Get("code"))
	if err != nil || completion.Identity.Subject != "edge-subject" || completion.CorrelationID != "tab-b" || completion.ReturnPath != "/" || completion.FreshReauthentication {
		t.Fatal("canceled tab consumed or relabeled the independent valid tab")
	}
	if _, err := p.Complete(t.Context(), store, browser, allowed.Get("state"), allowed.Get("code")); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatal("valid tab callback repeated")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.badRequest || f.tokenCalls != 1 || f.userInfoCalls != 1 {
		t.Fatal("parallel flow exchanged or consumed the wrong transaction")
	}
}
