package enterpriseauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

type ReauthenticationPolicy string

const (
	ReauthenticatePromptLogin     ReauthenticationPolicy = "prompt_login"
	ReauthenticateMaxAgeZero      ReauthenticationPolicy = "max_age_zero"
	ReauthenticatePromptAndMaxAge ReauthenticationPolicy = "prompt_login_max_age_zero"
)

// ProviderConfig is trusted operator configuration. Tokens and ClientSecret are
// retained privately by Provider; callers receive only verified identity facts.
type ProviderConfig struct {
	ConnectionID       string
	Issuer             string
	ClientID           string
	ClientSecret       string
	RedirectURL        string
	AuthStyle          oauth2.AuthStyle
	SigningAlgorithms  []string
	AllowedReturnPaths []string
	Outbound           OutboundPolicy
	UseUserInfo        bool
	Reauthentication   ReauthenticationPolicy
	TransactionTTL     time.Duration
	ClockSkew          time.Duration
}

type Provider struct {
	connectionID     string
	issuer           string
	oauth            oauth2.Config
	discovered       *oidc.Provider
	verifier         *oidc.IDTokenVerifier
	client           *http.Client
	transport        *boundedTransport
	fingerprint      [32]byte
	returnPaths      map[string]struct{}
	useUserInfo      bool
	reauthentication ReauthenticationPolicy
	ttl              time.Duration
	skew             time.Duration
}

type Authorization struct {
	URL       string
	ExpiresAt time.Time
}

type VerifiedIdentity struct {
	ConnectionID string
	Issuer       string
	Subject      string
	AuthTime     time.Time
}

// Completion contains no provider credentials. FreshReauthentication is set only
// after verifying signed auth_time against the consumed reauthentication flow.
type Completion struct {
	Identity              VerifiedIdentity
	ReturnPath            string
	Purpose               Purpose
	CorrelationID         string
	FreshReauthentication bool
}

func NewProvider(ctx context.Context, cfg ProviderConfig) (*Provider, error) {
	if cfg.ConnectionID == "" || len(cfg.ConnectionID) > 128 || strings.ContainsAny(cfg.ConnectionID, "\x00\r\n") || cfg.ClientID == "" || len(cfg.ClientID) > 1024 ||
		(cfg.AuthStyle != oauth2.AuthStyleInHeader && cfg.AuthStyle != oauth2.AuthStyleInParams) {
		return nil, ErrConfiguration
	}
	issuerURL, err := parseHTTPSURL(cfg.Issuer)
	if err != nil || issuerURL.RawQuery != "" {
		return nil, ErrConfiguration
	}
	callbackURL, err := parseHTTPSURL(cfg.RedirectURL)
	if err != nil || callbackURL.RawQuery != "" {
		return nil, ErrConfiguration
	}
	if cfg.Reauthentication != "" && cfg.Reauthentication != ReauthenticatePromptLogin && cfg.Reauthentication != ReauthenticateMaxAgeZero && cfg.Reauthentication != ReauthenticatePromptAndMaxAge {
		return nil, ErrConfiguration
	}
	ttl := cfg.TransactionTTL
	if ttl == 0 {
		ttl = 5 * time.Minute
	}
	if ttl < time.Second || ttl > 10*time.Minute || cfg.ClockSkew < 0 || cfg.ClockSkew > time.Minute {
		return nil, ErrConfiguration
	}
	returns := make(map[string]struct{}, len(cfg.AllowedReturnPaths))
	if len(cfg.AllowedReturnPaths) == 0 || len(cfg.AllowedReturnPaths) > 128 {
		return nil, ErrConfiguration
	}
	for _, path := range cfg.AllowedReturnPaths {
		if !validReturnPath(path) {
			return nil, ErrConfiguration
		}
		returns[path] = struct{}{}
	}
	algorithms := slices.Clone(cfg.SigningAlgorithms)
	if len(algorithms) == 0 {
		algorithms = []string{oidc.RS256}
	}
	for _, algorithm := range algorithms {
		if !slices.Contains([]string{oidc.RS256, oidc.RS384, oidc.RS512, oidc.ES256, oidc.ES384, oidc.ES512, oidc.PS256, oidc.PS384, oidc.PS512, oidc.EdDSA}, algorithm) {
			return nil, ErrConfiguration
		}
	}
	client, transport, err := newHTTPClient(cfg.Outbound)
	if err != nil {
		return nil, err
	}
	if !transport.allowed(cfg.Issuer) {
		transport.base.CloseIdleConnections()
		return nil, ErrConfiguration
	}
	discovered, err := oidc.NewProvider(oidc.ClientContext(ctx, client), cfg.Issuer)
	if err != nil {
		transport.base.CloseIdleConnections()
		return nil, ErrProviderUnavailable
	}
	var metadata map[string]any
	if discovered.Claims(&metadata) != nil || validateDiscovery(metadata, transport) != nil || metadata["issuer"] != cfg.Issuer {
		transport.base.CloseIdleConnections()
		return nil, ErrConfiguration
	}
	if cfg.UseUserInfo && discovered.UserInfoEndpoint() == "" {
		transport.base.CloseIdleConnections()
		return nil, ErrConfiguration
	}
	endpoint := discovered.Endpoint()
	endpoint.AuthStyle = cfg.AuthStyle // Never auto-probe or retry an authorization code.
	transport.jwksURL = metadata["jwks_uri"].(string)
	p := &Provider{
		connectionID: cfg.ConnectionID, issuer: cfg.Issuer,
		oauth:      oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: endpoint, RedirectURL: cfg.RedirectURL, Scopes: []string{oidc.ScopeOpenID}},
		discovered: discovered,
		// The provider's common key cache retains the bounded client supplied to
		// NewProvider and is shared with signed UserInfo responses. A separate
		// VerifierContext cache would spend another initial JWKS refresh budget.
		verifier: discovered.Verifier(&oidc.Config{ClientID: cfg.ClientID, SupportedSigningAlgs: algorithms}),
		client:   client, transport: transport, returnPaths: returns, useUserInfo: cfg.UseUserInfo,
		reauthentication: cfg.Reauthentication, ttl: ttl, skew: cfg.ClockSkew,
	}
	// Changing the identity, endpoint, algorithm or freshness configuration
	// invalidates pending transactions. Return paths are rechecked at completion.
	// The client secret is never serialized into the fingerprint or transaction store.
	fingerprint, _ := json.Marshal(struct {
		Connection, Issuer, Client, Callback string
		JWKS, UserInfo                       string
		Endpoint                             oauth2.Endpoint
		Algorithms, Origins                  []string
		Reauthentication                     ReauthenticationPolicy
		UseUserInfo                          bool
		Skew                                 time.Duration
	}{cfg.ConnectionID, cfg.Issuer, cfg.ClientID, cfg.RedirectURL, transport.jwksURL, discovered.UserInfoEndpoint(), endpoint, algorithms, cfg.Outbound.AllowedOrigins, cfg.Reauthentication, cfg.UseUserInfo, cfg.ClockSkew})
	p.fingerprint = sha256.Sum256(fingerprint)
	return p, nil
}

func (p *Provider) Close() { p.transport.base.CloseIdleConnections() }

func (p *Provider) Begin(ctx context.Context, store TransactionStore, browserSecret string, intent LoginIntent) (Authorization, error) {
	if store == nil || !validSecret(browserSecret) || !validIntent(intent) {
		return Authorization{}, ErrTransactionInvalid
	}
	if _, ok := p.returnPaths[intent.ReturnPath]; !ok {
		return Authorization{}, ErrTransactionInvalid
	}
	if intent.Purpose == PurposeReauthenticate && p.reauthentication == "" {
		return Authorization{}, ErrConfiguration
	}
	state, err := randomSecret()
	if err != nil {
		return Authorization{}, err
	}
	nonce, err := randomSecret()
	if err != nil {
		return Authorization{}, err
	}
	verifier, err := randomSecret()
	if err != nil {
		return Authorization{}, err
	}
	now := time.Now().UTC()
	tx := LoginTransaction{StateHash: secretHash(state), BrowserHash: secretHash(browserSecret), ProviderFingerprint: p.fingerprint, ConnectionID: p.connectionID, Nonce: nonce, PKCEVerifier: verifier, Intent: intent, CreatedAt: now, ExpiresAt: now.Add(p.ttl)}
	if err := store.Put(ctx, tx); err != nil {
		return Authorization{}, transactionError(err)
	}
	options := []oauth2.AuthCodeOption{oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)}
	if intent.Purpose == PurposeReauthenticate {
		if p.reauthentication == ReauthenticatePromptLogin || p.reauthentication == ReauthenticatePromptAndMaxAge {
			options = append(options, oauth2.SetAuthURLParam("prompt", "login"))
		}
		if p.reauthentication == ReauthenticateMaxAgeZero || p.reauthentication == ReauthenticatePromptAndMaxAge {
			options = append(options, oauth2.SetAuthURLParam("max_age", "0"))
		}
	}
	return Authorization{URL: p.oauth.AuthCodeURL(state, options...), ExpiresAt: tx.ExpiresAt}, nil
}

func (p *Provider) Complete(ctx context.Context, store TransactionStore, browserSecret, state, code string) (Completion, error) {
	if store == nil || !validSecret(browserSecret) || !validSecret(state) {
		return Completion{}, ErrTransactionInvalid
	}
	now := time.Now().UTC()
	tx, err := store.Consume(ctx, secretHash(state), secretHash(browserSecret), now)
	if err != nil {
		return Completion{}, transactionError(err)
	}
	if !validTransaction(tx) || tx.StateHash != secretHash(state) || tx.BrowserHash != secretHash(browserSecret) || tx.ProviderFingerprint != p.fingerprint || tx.ConnectionID != p.connectionID || !tx.ExpiresAt.After(now) || tx.CreatedAt.After(now) {
		return Completion{}, ErrTransactionInvalid
	}
	if _, ok := p.returnPaths[tx.Intent.ReturnPath]; !ok {
		return Completion{}, ErrTransactionInvalid
	}
	if code == "" || len(code) > 8192 || strings.ContainsAny(code, "\x00\r\n") {
		return Completion{}, ErrIdentityInvalid
	}
	providerContext := oidc.ClientContext(ctx, p.client)
	token, err := p.oauth.Exchange(providerContext, code, oauth2.VerifierOption(tx.PKCEVerifier))
	if err != nil {
		return Completion{}, ErrProviderUnavailable
	}
	rawToken, ok := token.Extra("id_token").(string)
	if !ok || rawToken == "" {
		return Completion{}, ErrIdentityInvalid
	}
	verified, err := p.verifier.Verify(providerContext, rawToken)
	if err != nil || verified.Issuer != p.issuer || !validSubject(verified.Subject) || subtle.ConstantTimeCompare([]byte(verified.Nonce), []byte(tx.Nonce)) != 1 {
		return Completion{}, ErrIdentityInvalid
	}
	var claims struct {
		AuthorizedParty string `json:"azp"`
		AuthTime        *int64 `json:"auth_time"`
	}
	var rawClaims json.RawMessage
	if verified.Claims(&rawClaims) != nil || !utf8.Valid(rawClaims) || verified.Claims(&claims) != nil || (len(verified.Audience) > 1 && claims.AuthorizedParty == "") || (claims.AuthorizedParty != "" && claims.AuthorizedParty != p.oauth.ClientID) {
		return Completion{}, ErrIdentityInvalid
	}
	if verified.AccessTokenHash != "" && verified.VerifyAccessToken(token.AccessToken) != nil {
		return Completion{}, ErrIdentityInvalid
	}
	now = time.Now().UTC()
	var authTime time.Time
	if claims.AuthTime != nil {
		// Check integer seconds before conversion: hostile extremes must never
		// wrap time.Time's representable internal seconds and appear historical.
		if *claims.AuthTime <= 0 || *claims.AuthTime > now.Add(p.skew).Unix() {
			return Completion{}, ErrIdentityInvalid
		}
		authTime = time.Unix(*claims.AuthTime, 0).UTC()
	}
	fresh := tx.Intent.Purpose == PurposeReauthenticate
	if fresh && (p.reauthentication == "" || authTime.IsZero() || authTime.Before(tx.CreatedAt.Add(-p.skew).Truncate(time.Second))) {
		return Completion{}, ErrIdentityInvalid
	}
	if p.useUserInfo {
		info, err := p.discovered.UserInfo(providerContext, oauth2.StaticTokenSource(token))
		if err != nil || info.Subject != verified.Subject {
			return Completion{}, ErrIdentityInvalid
		}
	}
	return Completion{Identity: VerifiedIdentity{ConnectionID: p.connectionID, Issuer: p.issuer, Subject: verified.Subject, AuthTime: authTime}, ReturnPath: tx.Intent.ReturnPath, Purpose: tx.Intent.Purpose, CorrelationID: tx.Intent.CorrelationID, FreshReauthentication: fresh}, nil
}

func validSubject(subject string) bool {
	if subject == "" || len(subject) > 1024 || !utf8.ValidString(subject) {
		return false
	}
	return strings.IndexFunc(subject, unicode.IsControl) < 0
}
