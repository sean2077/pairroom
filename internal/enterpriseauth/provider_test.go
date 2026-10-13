package enterpriseauth

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

type testCode struct {
	nonce      string
	challenge  string
	modify     func(map[string]any)
	modifyBody func([]byte) []byte
	wrongKey   bool
	headerURL  string
	keyID      string
}

type testIDP struct {
	server          *httptest.Server
	key             *ecdsa.PrivateKey
	otherKey        *ecdsa.PrivateKey
	keyID           string
	jwksCalls       int
	mu              sync.Mutex
	codes           map[string]testCode
	tokenCalls      int
	userInfoCalls   int
	userInfoSubject string
	userInfoSigned  bool
	tokenStatus     int
	tokenErrorCode  string
	tokenRedirect   string
	tokenOversize   bool
	discoveryModify func(map[string]any)
	requestError    string
}

func newTestIDP(t *testing.T) *testIDP {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	idp := &testIDP{key: key, otherKey: other, keyID: "test-key", codes: make(map[string]testCode), userInfoSubject: "subject-1"}
	idp.server = httptest.NewTLSServer(http.HandlerFunc(idp.serve))
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *testIDP) serve(w http.ResponseWriter, r *http.Request) {
	idp.mu.Lock()
	defer idp.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		metadata := map[string]any{"issuer": idp.server.URL, "authorization_endpoint": idp.server.URL + "/authorize", "token_endpoint": idp.server.URL + "/token", "userinfo_endpoint": idp.server.URL + "/userinfo", "jwks_uri": idp.server.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"ES256"}}
		if idp.discoveryModify != nil {
			idp.discoveryModify(metadata)
		}
		_ = json.NewEncoder(w).Encode(metadata)
	case "/jwks":
		idp.jwksCalls++
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &idp.key.PublicKey, KeyID: idp.keyID, Algorithm: "ES256", Use: "sig"}}})
	case "/token":
		idp.tokenCalls++
		if idp.tokenRedirect != "" {
			http.Redirect(w, r, idp.tokenRedirect, http.StatusTemporaryRedirect)
			return
		}
		if idp.tokenOversize {
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
			return
		}
		if idp.tokenStatus != 0 {
			w.WriteHeader(idp.tokenStatus)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": idp.tokenErrorCode, "error_description": "private-provider-body expired authorization code", "error_uri": "https://private.example.test/explanation"})
			return
		}
		if err := r.ParseForm(); err != nil {
			idp.requestError = "bad token form"
			w.WriteHeader(400)
			return
		}
		client, secret, basic := r.BasicAuth()
		if !basic || client != "client-1" || secret != "private-client-secret" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("redirect_uri") != "https://service.example.test/auth/callback" {
			idp.requestError = "bad token request binding"
			w.WriteHeader(400)
			return
		}
		code, ok := idp.codes[r.Form.Get("code")]
		delete(idp.codes, r.Form.Get("code"))
		challenge := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(challenge[:]) != code.challenge {
			idp.requestError = "PKCE mismatch"
			w.WriteHeader(400)
			return
		}
		now := time.Now().Unix()
		claims := map[string]any{"iss": idp.server.URL, "sub": "subject-1", "aud": "client-1", "exp": now + 600, "iat": now, "nonce": code.nonce, "auth_time": now}
		if code.modify != nil {
			code.modify(claims)
		}
		key := idp.key
		if code.wrongKey {
			key = idp.otherKey
		}
		keyID := idp.keyID
		if code.keyID != "" {
			keyID = code.keyID
		}
		signerOptions := (&jose.SignerOptions{}).WithHeader("kid", keyID)
		if code.headerURL != "" {
			signerOptions = signerOptions.WithHeader("jku", code.headerURL).WithHeader("x5u", code.headerURL)
		}
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, signerOptions)
		if err != nil {
			idp.requestError = err.Error()
			w.WriteHeader(500)
			return
		}
		body, _ := json.Marshal(claims)
		if code.modifyBody != nil {
			body = code.modifyBody(body)
		}
		signed, err := signer.Sign(body)
		if err != nil {
			idp.requestError = err.Error()
			w.WriteHeader(500)
			return
		}
		raw, err := signed.CompactSerialize()
		if err != nil {
			idp.requestError = err.Error()
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "private-access-token", "token_type": "Bearer", "expires_in": 600, "id_token": raw, "refresh_token": "private-refresh-token"})
	case "/userinfo":
		idp.userInfoCalls++
		if r.Header.Get("Authorization") != "Bearer private-access-token" {
			idp.requestError = "UserInfo missing access token"
			w.WriteHeader(401)
			return
		}
		claims := map[string]any{"sub": idp.userInfoSubject, "email": "not-identity@example.test"}
		if idp.userInfoSigned {
			signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: idp.key}, (&jose.SignerOptions{}).WithHeader("kid", idp.keyID))
			if err != nil {
				idp.requestError = err.Error()
				w.WriteHeader(500)
				return
			}
			body, _ := json.Marshal(claims)
			signed, err := signer.Sign(body)
			if err != nil {
				idp.requestError = err.Error()
				w.WriteHeader(500)
				return
			}
			raw, err := signed.CompactSerialize()
			if err != nil {
				idp.requestError = err.Error()
				w.WriteHeader(500)
				return
			}
			w.Header().Set("Content-Type", "application/jwt")
			_, _ = w.Write([]byte(raw))
			return
		}
		_ = json.NewEncoder(w).Encode(claims)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (idp *testIDP) config() ProviderConfig {
	pool := x509.NewCertPool()
	pool.AddCert(idp.server.Certificate())
	return ProviderConfig{ConnectionID: "connection-1", Issuer: idp.server.URL, ClientID: "client-1", ClientSecret: "private-client-secret", RedirectURL: "https://service.example.test/auth/callback", AuthStyle: oauth2.AuthStyleInHeader, SigningAlgorithms: []string{"ES256"}, AllowedReturnPaths: []string{"/", "/rooms"}, Reauthentication: ReauthenticatePromptAndMaxAge, Outbound: OutboundPolicy{AllowedOrigins: []string{idp.server.URL}, RootCAs: pool, Timeout: 2 * time.Second, MaxResponseBytes: 4096}}
}

func testProvider(t *testing.T, cfg ProviderConfig) *Provider {
	t.Helper()
	p, err := NewProvider(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func testMemoryStore(t *testing.T, capacity int) *MemoryStore {
	t.Helper()
	store, err := NewMemoryStore(capacity)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func beginTestLogin(t *testing.T, p *Provider, idp *testIDP, store TransactionStore, purpose Purpose) (browser, state, code string, query url.Values) {
	t.Helper()
	browser, err := NewBrowserSecret()
	if err != nil {
		t.Fatal(err)
	}
	auth, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: purpose, ReturnPath: "/rooms", CorrelationID: "operation-1"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(auth.URL)
	if err != nil {
		t.Fatal(err)
	}
	query = u.Query()
	if u.Scheme+"://"+u.Host+u.Path != idp.server.URL+"/authorize" || query.Get("response_type") != "code" || query.Get("client_id") != "client-1" || query.Get("redirect_uri") != "https://service.example.test/auth/callback" || query.Get("scope") != "openid" || query.Get("code_challenge_method") != "S256" || !validSecret(query.Get("state")) || !validSecret(query.Get("nonce")) || !validSecret(query.Get("code_challenge")) {
		t.Fatal("authorization request missing bound OIDC/PKCE parameters")
	}
	if query.Get("client_secret") != "" || query.Get("code_verifier") != "" {
		t.Fatal("authorization URL exposed private client state")
	}
	state, code = query.Get("state"), "code-"+query.Get("state")
	idp.mu.Lock()
	idp.codes[code] = testCode{nonce: query.Get("nonce"), challenge: query.Get("code_challenge")}
	idp.mu.Unlock()
	return browser, state, code, query
}

func TestOIDCLoginBindsBrowserPKCENonceAndReturnsOnlyIdentity(t *testing.T) {
	idp := newTestIDP(t)
	cfg := idp.config()
	cfg.UseUserInfo = true
	p := testProvider(t, cfg)
	store := testMemoryStore(t, 4)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	wrong, _ := NewBrowserSecret()
	if _, err := p.Complete(context.Background(), store, wrong, state, code); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("wrong browser: %v", err)
	}
	if idp.tokenCalls != 0 {
		t.Fatal("wrong browser contacted token endpoint")
	}
	result, err := p.Complete(context.Background(), store, browser, state, code)
	if err != nil {
		t.Fatalf("complete: %v (fixture: %s)", err, idp.requestError)
	}
	if result.Identity.ConnectionID != "connection-1" || result.Identity.Issuer != idp.server.URL || result.Identity.Subject != "subject-1" || result.Identity.AuthTime.IsZero() || result.ReturnPath != "/rooms" || result.Purpose != PurposeLogin || result.CorrelationID != "operation-1" || result.FreshReauthentication {
		t.Fatalf("identity or frozen intent lost: %+v", result)
	}
	encoded, _ := json.Marshal(result)
	for _, secret := range []string{"private-access-token", "private-refresh-token", "private-client-secret", "not-identity@example.test"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("completion exposed provider secret/profile")
		}
	}
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("replay: %v", err)
	}
	if idp.tokenCalls != 1 || idp.userInfoCalls != 1 {
		t.Fatalf("provider contacts token=%d userinfo=%d", idp.tokenCalls, idp.userInfoCalls)
	}
}

func TestOIDCRejectsInvalidSignedIdentity(t *testing.T) {
	tests := []struct {
		name     string
		change   func(map[string]any)
		wrongKey bool
	}{
		{"signature", nil, true},
		{"issuer", func(c map[string]any) { c["iss"] = "https://different.example.test" }, false},
		{"audience", func(c map[string]any) { c["aud"] = "other-client" }, false},
		{"nonce", func(c map[string]any) { c["nonce"] = "wrong-nonce" }, false},
		{"missing_nonce", func(c map[string]any) { delete(c, "nonce") }, false},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }, false},
		{"subject", func(c map[string]any) { c["sub"] = "" }, false},
		{"subject_nul", func(c map[string]any) { c["sub"] = "subject\x00suffix" }, false},
		{"subject_control", func(c map[string]any) { c["sub"] = "subject\u0085suffix" }, false},
		{"multi_audience_without_azp", func(c map[string]any) { c["aud"] = []string{"client-1", "other-client"} }, false},
		{"wrong_azp", func(c map[string]any) { c["azp"] = "other-client" }, false},
		{"multi_audience_wrong_azp", func(c map[string]any) { c["aud"] = []string{"client-1", "other-client"}; c["azp"] = "other-client" }, false},
		{"future_auth_time", func(c map[string]any) { c["auth_time"] = time.Now().Add(time.Hour).Unix() }, false},
		{"fractional_auth_time", func(c map[string]any) { c["auth_time"] = 1234.5 }, false},
		{"extreme_auth_time", func(c map[string]any) { c["auth_time"] = int64(math.MaxInt64) }, false},
		{"string_auth_time", func(c map[string]any) { c["auth_time"] = "1234" }, false},
		{"bad_access_token_hash", func(c map[string]any) { c["at_hash"] = "bad" }, false},
	}
	idp := newTestIDP(t)
	p := testProvider(t, idp.config())
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := testMemoryStore(t, 1)
			browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
			idp.mu.Lock()
			entry := idp.codes[code]
			entry.modify = test.change
			entry.wrongKey = test.wrongKey
			idp.codes[code] = entry
			before := idp.tokenCalls
			idp.mu.Unlock()
			if result, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrIdentityInvalid) || result != (Completion{}) {
				t.Fatalf("accepted invalid identity: result=%+v err=%v", result, err)
			}
			if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrTransactionInvalid) {
				t.Fatalf("invalid identity callback replay: %v", err)
			}
			if idp.tokenCalls != before+1 {
				t.Fatal("invalid identity was re-exchanged")
			}
		})
	}
}

func TestOIDCMultiAudienceRequiresCorrectAuthorizedParty(t *testing.T) {
	idp := newTestIDP(t)
	p := testProvider(t, idp.config())
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	idp.mu.Lock()
	entry := idp.codes[code]
	entry.modify = func(c map[string]any) { c["aud"] = []string{"client-1", "other-client"}; c["azp"] = "client-1" }
	idp.codes[code] = entry
	idp.mu.Unlock()
	if _, err := p.Complete(context.Background(), store, browser, state, code); err != nil {
		t.Fatal(err)
	}
}

func TestOIDCReauthenticationRequiresSignedFreshAuthTime(t *testing.T) {
	for _, policy := range []ReauthenticationPolicy{ReauthenticatePromptLogin, ReauthenticateMaxAgeZero, ReauthenticatePromptAndMaxAge} {
		t.Run(string(policy), func(t *testing.T) {
			idp := newTestIDP(t)
			cfg := idp.config()
			cfg.Reauthentication = policy
			p := testProvider(t, cfg)
			for _, scenario := range []string{"fresh", "missing", "old", "future"} {
				t.Run(scenario, func(t *testing.T) {
					store := testMemoryStore(t, 1)
					browser, state, code, query := beginTestLogin(t, p, idp, store, PurposeReauthenticate)
					wantPrompt := policy == ReauthenticatePromptLogin || policy == ReauthenticatePromptAndMaxAge
					wantMaxAge := policy == ReauthenticateMaxAgeZero || policy == ReauthenticatePromptAndMaxAge
					if (query.Get("prompt") == "login") != wantPrompt || (query.Get("max_age") == "0") != wantMaxAge {
						t.Fatal("reauthentication request did not use configured policy")
					}
					idp.mu.Lock()
					entry := idp.codes[code]
					entry.modify = func(c map[string]any) {
						switch scenario {
						case "missing":
							delete(c, "auth_time")
						case "old":
							c["auth_time"] = time.Now().Add(-time.Minute).Unix()
						case "future":
							c["auth_time"] = time.Now().Add(time.Minute).Unix()
						}
					}
					idp.codes[code] = entry
					idp.mu.Unlock()
					result, err := p.Complete(context.Background(), store, browser, state, code)
					if scenario == "fresh" {
						if err != nil || !result.FreshReauthentication || result.Identity.AuthTime.IsZero() {
							t.Fatalf("fresh result=%+v err=%v", result, err)
						}
					} else if !errors.Is(err, ErrIdentityInvalid) {
						t.Fatalf("%s auth_time accepted: %v", scenario, err)
					}
				})
			}
		})
	}
}

func TestOIDCUserInfoSubjectMustMatchVerifiedToken(t *testing.T) {
	idp := newTestIDP(t)
	idp.userInfoSubject = "different-subject"
	cfg := idp.config()
	cfg.UseUserInfo = true
	p := testProvider(t, cfg)
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatalf("UserInfo accepted: %v", err)
	}
}

func TestOIDCExchangeErrorIsConsumedWithoutRetryOrSecretError(t *testing.T) {
	for _, test := range []struct {
		name, code string
		status     int
		want       error
	}{
		{"expired_code", "invalid_grant", http.StatusBadRequest, ErrIdentityInvalid},
		{"denied", "access_denied", http.StatusForbidden, ErrIdentityInvalid},
		{"invalid_client", "invalid_client", http.StatusUnauthorized, ErrProviderUnavailable},
		{"provider_failure", "server_error", http.StatusServiceUnavailable, ErrProviderUnavailable},
		{"unknown_response", "unexpected_error", http.StatusBadGateway, ErrProviderUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			idp := newTestIDP(t)
			idp.tokenStatus = test.status
			idp.tokenErrorCode = test.code
			p := testProvider(t, idp.config())
			store := testMemoryStore(t, 1)
			browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
			_, err := p.Complete(context.Background(), store, browser, state, code)
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "private-") || strings.Contains(err.Error(), code) {
				t.Fatalf("unsafe exchange failure: %v", err)
			}
			if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrTransactionInvalid) {
				t.Fatalf("exchange failure replay: %v", err)
			}
			if idp.tokenCalls != 1 {
				t.Fatalf("authorization code exchanged %d times", idp.tokenCalls)
			}
		})
	}
}

func TestOIDCRedirectIsNeverFollowed(t *testing.T) {
	var contacted atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { contacted.Add(1); w.WriteHeader(500) }))
	defer target.Close()
	idp := newTestIDP(t)
	idp.tokenRedirect = target.URL
	p := testProvider(t, idp.config())
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("redirect result: %v", err)
	}
	if contacted.Load() != 0 || idp.tokenCalls != 1 {
		t.Fatal("token redirect followed or retried")
	}
}

func TestOIDCDiscoveryValidatesEveryConfiguredDestination(t *testing.T) {
	for _, field := range []string{"authorization_endpoint", "token_endpoint", "jwks_uri", "userinfo_endpoint", "registration_endpoint", "revocation_endpoint", "introspection_endpoint", "device_authorization_endpoint", "end_session_endpoint", "check_session_iframe", "mtls_endpoint_aliases"} {
		t.Run(field, func(t *testing.T) {
			idp := newTestIDP(t)
			idp.discoveryModify = func(m map[string]any) {
				if field == "mtls_endpoint_aliases" {
					m[field] = map[string]any{"token_endpoint": "https://not-allowed.example.test/token"}
				} else {
					m[field] = "https://not-allowed.example.test/path"
				}
			}
			if p, err := NewProvider(context.Background(), idp.config()); err == nil {
				p.Close()
				t.Fatal("unconfigured discovery destination admitted")
			}
			if idp.tokenCalls != 0 || idp.userInfoCalls != 0 {
				t.Fatal("invalid discovery contacted resource endpoint")
			}
		})
	}
}

func TestOIDCResponseBodyIsBoundedWithoutTrustedContentLength(t *testing.T) {
	for _, mode := range []string{"chunked", "declared_large", "gzip", "false_short"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch mode {
				case "declared_large":
					w.Header().Set("Content-Length", "8192")
				case "false_short":
					w.Header().Set("Content-Length", "4")
				case "gzip":
					w.Header().Set("Content-Encoding", "gzip")
				}
				if mode == "chunked" {
					w.(http.Flusher).Flush()
				}
				if mode == "gzip" {
					out := gzip.NewWriter(w)
					_, _ = out.Write([]byte(strings.Repeat("x", 8192)))
					_ = out.Close()
				} else {
					_, _ = w.Write([]byte(strings.Repeat("x", 8192)))
				}
			}))
			defer server.Close()
			pool := x509.NewCertPool()
			pool.AddCert(server.Certificate())
			cfg := ProviderConfig{ConnectionID: "connection", Issuer: server.URL, ClientID: "client", RedirectURL: "https://service.example.test/callback", AuthStyle: oauth2.AuthStyleInHeader, AllowedReturnPaths: []string{"/"}, Outbound: OutboundPolicy{AllowedOrigins: []string{server.URL}, RootCAs: pool, MaxResponseBytes: 1024}}
			if p, err := NewProvider(context.Background(), cfg); err == nil {
				p.Close()
				t.Fatal("oversized/truncated discovery accepted")
			}
		})
	}
	// The same cap must apply beyond discovery, before OAuth's token decoder.
	idp := newTestIDP(t)
	idp.tokenOversize = true
	p := testProvider(t, idp.config())
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("oversized token result: %v", err)
	}
}

func TestOIDCConfigAndFrozenIntentFailClosed(t *testing.T) {
	idp := newTestIDP(t)
	for _, test := range []struct {
		name   string
		change func(*ProviderConfig)
	}{
		{"auth_autodetect", func(c *ProviderConfig) { c.AuthStyle = oauth2.AuthStyleAutoDetect }},
		{"http_issuer", func(c *ProviderConfig) { c.Issuer = strings.Replace(c.Issuer, "https:", "http:", 1) }},
		{"http_callback", func(c *ProviderConfig) { c.RedirectURL = "http://service.example.test/callback" }},
		{"unsafe_algorithm", func(c *ProviderConfig) { c.SigningAlgorithms = []string{"HS256"} }},
		{"no_origins", func(c *ProviderConfig) { c.Outbound.AllowedOrigins = nil }},
		{"bad_return", func(c *ProviderConfig) { c.AllowedReturnPaths = []string{"//external.example.test"} }},
		{"query_return", func(c *ProviderConfig) { c.AllowedReturnPaths = []string{"/?next=bad"} }},
		{"unbounded_timeout", func(c *ProviderConfig) { c.Outbound.Timeout = 2 * time.Minute }},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := idp.config()
			test.change(&cfg)
			if p, err := NewProvider(context.Background(), cfg); err == nil {
				p.Close()
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	p := testProvider(t, idp.config())
	store := testMemoryStore(t, 2)
	browser, _ := NewBrowserSecret()
	if _, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: "/unlisted"}); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("return path: %v", err)
	}
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	cfg := idp.config()
	cfg.ClientID = "different-client"
	other := testProvider(t, cfg)
	if _, err := other.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("reconfigured callback: %v", err)
	}
	if idp.tokenCalls != 0 {
		t.Fatal("reconfigured provider exchanged old code")
	}
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrTransactionInvalid) {
		t.Fatalf("consumed mismatch replay: %v", err)
	}
}

func TestOIDCPKCEWrongVerifierFailsAtProvider(t *testing.T) {
	idp := newTestIDP(t)
	p := testProvider(t, idp.config())
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	// Corrupt only private stored PKCE state, retaining the authorization challenge.
	store.mu.Lock()
	tx := store.transactions[secretHash(state)]
	tx.PKCEVerifier, _ = randomSecret()
	store.transactions[secretHash(state)] = tx
	store.mu.Unlock()
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("wrong PKCE verifier: %v", err)
	}
	if idp.requestError != "PKCE mismatch" || idp.tokenCalls != 1 {
		t.Fatal("fixture did not verify PKCE on the wire")
	}
}

func TestOIDCDiscoveryRedirectMakesNoTargetRequest(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer target.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer origin.Close()
	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	pool.AddCert(target.Certificate())
	cfg := ProviderConfig{ConnectionID: "c", Issuer: origin.URL, ClientID: "client", RedirectURL: "https://service.example.test/callback", AuthStyle: oauth2.AuthStyleInHeader, AllowedReturnPaths: []string{"/"}, Outbound: OutboundPolicy{AllowedOrigins: []string{origin.URL, target.URL}, RootCAs: pool}}
	if p, err := NewProvider(context.Background(), cfg); err == nil {
		p.Close()
		t.Fatal("discovery redirect admitted")
	}
	if calls.Load() != 0 {
		t.Fatal("discovery redirect target contacted despite no-redirect policy")
	}
}

func TestOIDCProviderRequestsHaveFiniteDeadline(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }))
	defer server.Close()
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	cfg := ProviderConfig{ConnectionID: "c", Issuer: server.URL, ClientID: "client", RedirectURL: "https://service.example.test/callback", AuthStyle: oauth2.AuthStyleInHeader, AllowedReturnPaths: []string{"/"}, Outbound: OutboundPolicy{AllowedOrigins: []string{server.URL}, RootCAs: pool, Timeout: 100 * time.Millisecond}}
	if p, err := NewProvider(context.Background(), cfg); err == nil {
		p.Close()
		t.Fatal("unbounded provider response accepted")
	}
	select {
	case <-started:
	default:
		t.Fatal("deadline did not exercise real provider request")
	}
}

func TestOIDCRejectsInvalidUTF8InSignedClaims(t *testing.T) {
	idp := newTestIDP(t)
	p := testProvider(t, idp.config())
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	idp.mu.Lock()
	entry := idp.codes[code]
	entry.modifyBody = func(body []byte) []byte { return bytes.Replace(body, []byte("subject-1"), []byte{'s', 0xff}, 1) }
	idp.codes[code] = entry
	idp.mu.Unlock()
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatalf("invalid UTF8 signed identity accepted: %v", err)
	}
}

type corruptingStore struct {
	TransactionStore
	corrupt func(*LoginTransaction)
}

func (s corruptingStore) Consume(ctx context.Context, state, browser [32]byte, now time.Time) (LoginTransaction, error) {
	tx, err := s.TransactionStore.Consume(ctx, state, browser, now)
	if err == nil {
		s.corrupt(&tx)
	}
	return tx, err
}

func TestOIDCRechecksConsumedTransactionBeforeProviderContact(t *testing.T) {
	idp := newTestIDP(t)
	p := testProvider(t, idp.config())
	for _, test := range []struct {
		name    string
		corrupt func(*LoginTransaction)
	}{
		{"expired", func(tx *LoginTransaction) { tx.ExpiresAt = time.Now().Add(-time.Second) }},
		{"future", func(tx *LoginTransaction) {
			tx.CreatedAt = time.Now().Add(time.Hour)
			tx.ExpiresAt = tx.CreatedAt.Add(time.Minute)
		}},
		{"state", func(tx *LoginTransaction) { tx.StateHash = secretHash("other-state") }},
		{"browser", func(tx *LoginTransaction) { tx.BrowserHash = secretHash("other-browser") }},
		{"config", func(tx *LoginTransaction) { tx.ProviderFingerprint = secretHash("other-config") }},
		{"connection", func(tx *LoginTransaction) { tx.ConnectionID = "other-connection" }},
		{"return", func(tx *LoginTransaction) { tx.Intent.ReturnPath = "/unlisted" }},
		{"purpose", func(tx *LoginTransaction) { tx.Intent.Purpose = "admin" }},
		{"nonce", func(tx *LoginTransaction) { tx.Nonce = "not-random" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testMemoryStore(t, 1)
			browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
			wrapped := corruptingStore{TransactionStore: store, corrupt: test.corrupt}
			if _, err := p.Complete(context.Background(), wrapped, browser, state, code); !errors.Is(err, ErrTransactionInvalid) {
				t.Fatalf("corrupt transaction: %v", err)
			}
			if idp.tokenCalls != 0 {
				t.Fatal("corrupt transaction contacted token endpoint")
			}
			if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrTransactionInvalid) {
				t.Fatalf("corrupt transaction replayed: %v", err)
			}
		})
	}
}

func TestOIDCBeginCapacityAndUnsupportedFreshPolicyMakeNoProviderContact(t *testing.T) {
	idp := newTestIDP(t)
	cfg := idp.config()
	cfg.Reauthentication = ""
	p := testProvider(t, cfg)
	store := testMemoryStore(t, 1)
	_, _, _, _ = beginTestLogin(t, p, idp, store, PurposeLogin)
	browser, _ := NewBrowserSecret()
	if _, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeLogin, ReturnPath: "/"}); !errors.Is(err, ErrTransactionCapacity) {
		t.Fatalf("capacity refused: %v", err)
	}
	if _, err := p.Begin(context.Background(), store, browser, LoginIntent{Purpose: PurposeReauthenticate, ReturnPath: "/"}); !errors.Is(err, ErrConfiguration) {
		t.Fatalf("unsupported fresh reauth: %v", err)
	}
	if idp.tokenCalls != 0 || idp.userInfoCalls != 0 {
		t.Fatal("begin failure contacted provider endpoints")
	}
}

type unavailableStore struct{ TransactionStore }

func (s unavailableStore) Consume(context.Context, [32]byte, [32]byte, time.Time) (LoginTransaction, error) {
	return LoginTransaction{}, errors.New("private-database-description")
}

func TestOIDCUnknownStateConsumeCannotExchangeCode(t *testing.T) {
	idp := newTestIDP(t)
	p := testProvider(t, idp.config())
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	_, err := p.Complete(context.Background(), unavailableStore{TransactionStore: store}, browser, state, code)
	if !errors.Is(err, ErrTransactionUnavailable) || strings.Contains(err.Error(), "private-") {
		t.Fatalf("state consume error: %v", err)
	}
	if idp.tokenCalls != 0 {
		t.Fatal("uncertain state consume contacted token endpoint")
	}
}

func TestOIDCTokenCannotSelectKeysOrSigningAlgorithm(t *testing.T) {
	var calls atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer target.Close()
	idp := newTestIDP(t)
	p := testProvider(t, idp.config())
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	idp.mu.Lock()
	entry := idp.codes[code]
	entry.wrongKey = true
	entry.headerURL = target.URL + "/keys"
	idp.codes[code] = entry
	idp.mu.Unlock()
	if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatalf("attacker key selection: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("token-supplied key URL contacted")
	}
	cfg := idp.config()
	cfg.SigningAlgorithms = []string{"RS256"}
	rsaOnly := testProvider(t, cfg)
	browser, state, code, _ = beginTestLogin(t, rsaOnly, idp, store, PurposeLogin)
	if _, err := rsaOnly.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrIdentityInvalid) {
		t.Fatalf("unconfigured signing algorithm accepted: %v", err)
	}
}

func TestOIDCInternalOriginNeedsExplicitCertificateTrust(t *testing.T) {
	idp := newTestIDP(t)
	cfg := idp.config()
	cfg.Outbound.RootCAs = nil
	if p, err := NewProvider(context.Background(), cfg); err == nil {
		p.Close()
		t.Fatal("private IdP certificate trusted without configured CA")
	}
}

func TestOIDCJWKSRefreshBudgetStopsSequentialUnknownKeys(t *testing.T) {
	idp := newTestIDP(t)
	p := testProvider(t, idp.config())
	for i := range 8 {
		store := testMemoryStore(t, 1)
		browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
		idp.mu.Lock()
		entry := idp.codes[code]
		entry.keyID = fmt.Sprintf("unknown-key-%d", i)
		idp.codes[code] = entry
		idp.mu.Unlock()
		if _, err := p.Complete(context.Background(), store, browser, state, code); !errors.Is(err, ErrIdentityInvalid) {
			t.Fatalf("unknown key %d: %v", i, err)
		}
	}
	idp.mu.Lock()
	calls := idp.jwksCalls
	idp.mu.Unlock()
	if calls != 2 {
		t.Fatalf("unknown-key storm made %d JWKS requests, want 2", calls)
	}
	// Fetch budget exhaustion must not reject a valid key already cached.
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	if _, err := p.Complete(context.Background(), store, browser, state, code); err != nil {
		t.Fatalf("cached valid key after storm: %v", err)
	}
	if idp.jwksCalls != 2 {
		t.Fatal("valid cached key triggered extra refresh")
	}
}

func TestOIDCJWKSInitialKeyAndFirstRotationBothSucceed(t *testing.T) {
	idp := newTestIDP(t)
	idp.userInfoSigned = true
	cfg := idp.config()
	cfg.UseUserInfo = true
	p := testProvider(t, cfg)
	store := testMemoryStore(t, 1)
	browser, state, code, _ := beginTestLogin(t, p, idp, store, PurposeLogin)
	if _, err := p.Complete(context.Background(), store, browser, state, code); err != nil {
		t.Fatalf("initial key: %v", err)
	}
	idp.mu.Lock()
	idp.key = idp.otherKey
	idp.keyID = "rotated-key"
	idp.mu.Unlock()
	browser, state, code, _ = beginTestLogin(t, p, idp, store, PurposeLogin)
	if _, err := p.Complete(context.Background(), store, browser, state, code); err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	if idp.jwksCalls != 2 {
		t.Fatalf("initial + rotation fetched %d times", idp.jwksCalls)
	}
}

func TestJWKSRefreshBudgetIsConcurrentAndResetsAtBoundedWindow(t *testing.T) {
	transport := &boundedTransport{jwksWindow: time.Minute, jwksBurst: 2}
	now := time.Now()
	start := make(chan struct{})
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			<-start
			if transport.allowJWKS(now) {
				allowed.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if allowed.Load() != 2 {
		t.Fatalf("concurrent refresh permits=%d", allowed.Load())
	}
	if transport.allowJWKS(now.Add(time.Minute - time.Nanosecond)) {
		t.Fatal("window reset early")
	}
	if !transport.allowJWKS(now.Add(time.Minute)) {
		t.Fatal("window did not reset")
	}
}

func ExampleCompletion() {
	// Authorization of an admitted User remains an application/SQL responsibility.
	result := Completion{Identity: VerifiedIdentity{ConnectionID: "work", Issuer: "https://idp.example.test", Subject: "opaque-subject"}, Purpose: PurposeLogin, ReturnPath: "/"}
	fmt.Println(result.Identity.ConnectionID, result.Identity.Subject, result.Purpose)
	// Output: work opaque-subject login
}
