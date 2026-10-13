package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/enterpriseservice"
	"golang.org/x/oauth2"
)

// enterpriseFileConfig is trusted operator input. Secret values are read from
// explicitly named process environment variables and never serialized here.
type enterpriseFileConfig struct {
	Version              int                  `json:"version"`
	DeploymentID         string               `json:"deployment_id"`
	OrganizationID       string               `json:"organization_id"`
	DatabaseSchema       string               `json:"database_schema"`
	DatabaseDSNEnv       string               `json:"database_dsn_env"`
	Listen               string               `json:"listen"`
	PublicOrigin         string               `json:"public_origin"`
	TLSCertificateFile   string               `json:"tls_certificate_file,omitempty"`
	TLSKeyFile           string               `json:"tls_key_file,omitempty"`
	TrustedProxyPrefixes []string             `json:"trusted_proxy_prefixes,omitempty"`
	SessionTTL           string               `json:"session_ttl,omitempty"`
	PendingLoginLimit    int                  `json:"pending_login_limit,omitempty"`
	OIDC                 enterpriseOIDCConfig `json:"oidc"`
}

type enterpriseOIDCConfig struct {
	ConnectionID      string   `json:"connection_id"`
	Issuer            string   `json:"issuer"`
	ClientID          string   `json:"client_id"`
	ClientSecretEnv   string   `json:"client_secret_env,omitempty"`
	ClientAuthMethod  string   `json:"client_auth_method"`
	AllowedOrigins    []string `json:"allowed_origins"`
	UseUserInfo       bool     `json:"use_userinfo,omitempty"`
	CACertificateFile string   `json:"ca_certificate_file,omitempty"`
	AdmittedSubjects  []string `json:"admitted_subjects,omitempty"`
}

var (
	enterpriseIDPattern     = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
	enterpriseSchemaPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	enterpriseEnvPattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

func loadEnterpriseConfig(path string) (enterpriseFileConfig, error) {
	var cfg enterpriseFileConfig
	if path == "" {
		return cfg, errors.New("enterprise --config is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return cfg, errors.New("cannot open enterprise configuration file")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return cfg, errors.New("enterprise configuration exceeds its read budget")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF {
		return cfg, errors.New("enterprise configuration must be one supported JSON object")
	}
	if err := cfg.validate(); err != nil {
		return enterpriseFileConfig{}, err
	}
	if err := enterpriseservice.ValidateConfig(cfg.httpConfig()); err != nil {
		return enterpriseFileConfig{}, errors.New("enterprise HTTP origin or proxy policy is invalid")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return enterpriseFileConfig{}, errors.New("cannot resolve enterprise configuration directory")
	}
	for _, value := range []*string{&cfg.TLSCertificateFile, &cfg.TLSKeyFile, &cfg.OIDC.CACertificateFile} {
		if *value != "" && !filepath.IsAbs(*value) {
			*value = filepath.Join(base, *value)
		}
	}
	return cfg, nil
}

func (cfg *enterpriseFileConfig) validate() error {
	if cfg.Version != 1 || !enterpriseIDPattern.MatchString(cfg.DeploymentID) || !enterpriseIDPattern.MatchString(cfg.OrganizationID) || !enterpriseSchemaPattern.MatchString(cfg.DatabaseSchema) || !enterpriseEnvPattern.MatchString(cfg.DatabaseDSNEnv) {
		return errors.New("enterprise configuration requires version 1 and valid deployment, organization, schema and database environment reference")
	}
	listen, err := netip.ParseAddrPort(cfg.Listen)
	if err != nil || listen.Port() == 0 || listen.Addr().IsMulticast() || listen.Addr().Is4In6() {
		return errors.New("enterprise listen must be a numeric address with a nonzero port")
	}
	origin, err := url.Parse(cfg.PublicOrigin)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.Opaque != "" || strings.ToLower(origin.Host) != origin.Host || cfg.PublicOrigin != "https://"+origin.Host {
		return errors.New("enterprise public_origin must be one canonical HTTPS origin")
	}
	if (cfg.TLSCertificateFile == "") != (cfg.TLSKeyFile == "") {
		return errors.New("enterprise TLS certificate and key must be configured together")
	}
	if _, err := cfg.proxyPrefixes(); err != nil {
		return err
	}
	if cfg.TLSCertificateFile == "" && len(cfg.TrustedProxyPrefixes) == 0 {
		return errors.New("enterprise requires direct TLS or explicitly trusted TLS proxy peers")
	}
	if cfg.SessionTTL == "" {
		cfg.SessionTTL = "12h"
	}
	ttl, err := time.ParseDuration(cfg.SessionTTL)
	if err != nil || ttl < 5*time.Minute || ttl > 24*time.Hour {
		return errors.New("enterprise session_ttl must be between 5m and 24h")
	}
	if cfg.PendingLoginLimit == 0 {
		cfg.PendingLoginLimit = 1024
	}
	if cfg.PendingLoginLimit < 1 || cfg.PendingLoginLimit > 100000 {
		return errors.New("enterprise pending_login_limit is out of range")
	}
	if !enterpriseIDPattern.MatchString(cfg.OIDC.ConnectionID) || cfg.OIDC.ClientID == "" || len(cfg.OIDC.ClientID) > 1024 || strings.ContainsAny(cfg.OIDC.ClientID, "\x00\r\n") || cfg.OIDC.ClientSecretEnv != "" && !enterpriseEnvPattern.MatchString(cfg.OIDC.ClientSecretEnv) {
		return errors.New("enterprise OIDC client configuration is invalid")
	}
	issuer, err := url.Parse(cfg.OIDC.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Hostname() == "" || issuer.User != nil || issuer.RawQuery != "" || issuer.ForceQuery || issuer.Fragment != "" || issuer.Opaque != "" || strings.ContainsAny(cfg.OIDC.Issuer, "\\\r\n\t") {
		return errors.New("enterprise OIDC issuer must be a configured HTTPS URL")
	}
	if cfg.OIDC.ClientAuthMethod != "basic" && cfg.OIDC.ClientAuthMethod != "post" {
		return errors.New("enterprise OIDC client_auth_method must explicitly select basic or post")
	}
	if len(cfg.OIDC.AllowedOrigins) == 0 || len(cfg.OIDC.AllowedOrigins) > 32 || len(cfg.OIDC.AdmittedSubjects) > 1000 {
		return errors.New("enterprise OIDC origin or admission configuration exceeds its budget")
	}
	issuerAllowed := false
	for _, allowed := range cfg.OIDC.AllowedOrigins {
		u, err := url.Parse(allowed)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || allowed != "https://"+u.Host {
			return errors.New("enterprise OIDC allowed_origins must contain exact HTTPS origins")
		}
		issuerAllowed = issuerAllowed || allowed == "https://"+issuer.Host
	}
	if !issuerAllowed {
		return errors.New("enterprise OIDC issuer origin must be explicitly allowed")
	}
	seenSubjects := make(map[string]bool, len(cfg.OIDC.AdmittedSubjects))
	for _, subject := range cfg.OIDC.AdmittedSubjects {
		if subject == "" || len(subject) > 1024 || !utf8.ValidString(subject) || strings.ContainsFunc(subject, unicode.IsControl) || seenSubjects[subject] {
			return errors.New("enterprise admission subjects must be distinct valid exact identities")
		}
		seenSubjects[subject] = true
	}
	return nil
}

func (cfg enterpriseFileConfig) proxyPrefixes() ([]netip.Prefix, error) {
	if len(cfg.TrustedProxyPrefixes) > 16 {
		return nil, errors.New("too many enterprise trusted proxy peers")
	}
	result := make([]netip.Prefix, 0, len(cfg.TrustedProxyPrefixes))
	for _, raw := range cfg.TrustedProxyPrefixes {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() == 0 || prefix != prefix.Masked() || prefix.Addr().Is4In6() {
			return nil, errors.New("enterprise proxy peers must be explicit canonical CIDR prefixes")
		}
		result = append(result, prefix)
	}
	return result, nil
}

func enterpriseSecretFromEnv(name string, required bool) (string, error) {
	if name == "" && !required {
		return "", nil
	}
	if !enterpriseEnvPattern.MatchString(name) {
		return "", errors.New("enterprise secret environment reference is invalid")
	}
	value, present := os.LookupEnv(name)
	if !present || strings.TrimSpace(value) == "" || len(value) > 64<<10 || strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("enterprise secret environment reference %s is unavailable", name)
	}
	return value, nil
}

func (cfg enterpriseFileConfig) providerConfig(secret string) enterpriseauth.ProviderConfig {
	style := oauth2.AuthStyleInHeader
	if cfg.OIDC.ClientAuthMethod == "post" {
		style = oauth2.AuthStyleInParams
	}
	return enterpriseauth.ProviderConfig{
		ConnectionID: cfg.OIDC.ConnectionID, Issuer: cfg.OIDC.Issuer, ClientID: cfg.OIDC.ClientID, ClientSecret: secret,
		RedirectURL: cfg.PublicOrigin + "/enterprise/auth/callback", AuthStyle: style,
		AllowedReturnPaths: []string{"/enterprise/"}, UseUserInfo: cfg.OIDC.UseUserInfo,
		Reauthentication: enterpriseauth.ReauthenticatePromptAndMaxAge,
		Outbound:         enterpriseauth.OutboundPolicy{AllowedOrigins: cfg.OIDC.AllowedOrigins},
	}
}

func (cfg enterpriseFileConfig) httpConfig() enterpriseservice.Config {
	prefixes, _ := cfg.proxyPrefixes() // The caller validates the complete file first.
	ttl, _ := time.ParseDuration(cfg.SessionTTL)
	return enterpriseservice.Config{DeploymentID: cfg.DeploymentID, PublicOrigin: cfg.PublicOrigin, TrustedProxyPrefixes: prefixes, SessionTTL: ttl}
}

// A private IdP CA is an explicit operator reference. It extends system roots
// and never disables hostname or certificate validation.
func (cfg enterpriseFileConfig) providerRoots() (*x509.CertPool, error) {
	if cfg.OIDC.CACertificateFile == "" {
		return nil, nil
	}
	f, err := os.Open(cfg.OIDC.CACertificateFile)
	if err != nil {
		return nil, errors.New("enterprise OIDC CA certificate file is unavailable")
	}
	defer f.Close()
	pem, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(pem) > 1<<20 {
		return nil, errors.New("enterprise OIDC CA certificate file exceeds its read budget")
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return nil, errors.New("enterprise system CA roots are unavailable")
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("enterprise OIDC CA certificate file contains no usable certificates")
	}
	return roots, nil
}
