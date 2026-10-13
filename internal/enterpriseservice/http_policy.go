// Package enterpriseservice exposes the enterprise browser boundary. It does
// not expose Local Management credentials or infer identity from request bodies.
package enterpriseservice

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	CSRFHeader      = "X-PairRoom-CSRF"
	maxRequestBytes = 64 << 10
)

var ErrConfiguration = errors.New("enterprise HTTP configuration invalid")

type Config struct {
	DeploymentID string
	// PublicOrigin is one canonical, operator-selected HTTPS origin with no path.
	PublicOrigin string
	// TrustedProxyPrefixes admit TLS termination only from the immediate peer.
	// Such a peer must preserve Host and send exactly X-Forwarded-Proto: https.
	// Forwarded host/client-address headers never determine authority or identity.
	TrustedProxyPrefixes []netip.Prefix
	SessionTTL           time.Duration
}

type httpPolicy struct {
	origin, host, deployment, sessionCookie, loginCookie string
	proxies                                              []netip.Prefix
	sessionTTL                                           time.Duration
}

// ValidateConfig checks browser authority policy before an entrypoint opens its
// database or listener. New repeats the same validation at dependency wiring.
func ValidateConfig(cfg Config) error { _, err := makeHTTPPolicy(cfg); return err }

func makeHTTPPolicy(cfg Config) (httpPolicy, error) {
	u, err := url.Parse(cfg.PublicOrigin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.ToLower(u.Host) != u.Host || cfg.PublicOrigin != "https://"+u.Host || !validID(cfg.DeploymentID) {
		return httpPolicy{}, ErrConfiguration
	}
	// Browsers serialize HTTPS's default port without :443 and use ASCII host
	// names. Reject ambiguous operator spelling instead of normalizing authority
	// differently from the Origin/Host checks at request time.
	if u.Port() == "443" || strings.HasSuffix(u.Host, ":") {
		return httpPolicy{}, ErrConfiguration
	}
	if u.Port() != "" {
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 {
			return httpPolicy{}, ErrConfiguration
		}
	}
	for _, r := range u.Hostname() {
		if r > 127 || r == '%' || r < 33 {
			return httpPolicy{}, ErrConfiguration
		}
	}
	if len(cfg.TrustedProxyPrefixes) > 16 {
		return httpPolicy{}, ErrConfiguration
	}
	proxies := make([]netip.Prefix, len(cfg.TrustedProxyPrefixes))
	for i, prefix := range cfg.TrustedProxyPrefixes {
		if !prefix.IsValid() || prefix.Bits() == 0 || prefix != prefix.Masked() {
			return httpPolicy{}, ErrConfiguration
		}
		proxies[i] = prefix
	}
	ttl := cfg.SessionTTL
	if ttl == 0 {
		ttl = 12 * time.Hour
	}
	if ttl < time.Minute || ttl > 24*time.Hour {
		return httpPolicy{}, ErrConfiguration
	}
	namespace := sha256.Sum256([]byte(cfg.DeploymentID))
	suffix := hex.EncodeToString(namespace[:16])
	return httpPolicy{origin: cfg.PublicOrigin, host: u.Host, deployment: cfg.DeploymentID, sessionCookie: "__Host-pairroom_session_" + suffix, loginCookie: "__Host-pairroom_login_" + suffix, proxies: proxies, sessionTTL: ttl}, nil
}

func (p httpPolicy) accepts(r *http.Request) bool {
	if r.Host != p.host || r.URL.User != nil || r.URL.Fragment != "" || r.URL.RawPath != "" || len(r.URL.RawQuery) > 16<<10 || r.Header.Get("Authorization") != "" {
		return false
	}
	if r.URL.Host != "" && r.URL.Host != p.host {
		return false
	}
	if r.URL.Scheme != "" && r.URL.Scheme != "https" {
		return false
	}
	if r.TLS != nil {
		return true
	}
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil || len(r.Header.Values("X-Forwarded-Proto")) != 1 || r.Header.Get("X-Forwarded-Proto") != "https" {
		return false
	}
	for _, prefix := range p.proxies {
		if prefix.Contains(peer.Addr().Unmap()) {
			return true
		}
	}
	return false
}

func (p httpPolicy) sameOrigin(r *http.Request) bool {
	return len(r.Header.Values("Origin")) == 1 && r.Header.Get("Origin") == p.origin
}

func readCookie(r *http.Request, name string) (string, bool) {
	var value string
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			if value != "" || !validOpaqueSecret(cookie.Value) {
				return "", false
			}
			value = cookie.Value
		}
	}
	return value, value != ""
}

func validOpaqueSecret(value string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(raw) == 32 && base64.RawURLEncoding.EncodeToString(raw) == value
}

func tokenHash(value string) [32]byte { return sha256.Sum256([]byte(value)) }

// CSRF is recoverable from the HttpOnly credential after a page reload or
// Service restart. Only its hash is persisted; the browser receives the derived
// token in an authenticated no-store response and keeps it in page memory.
func deriveCSRF(sessionSecret, deployment, sessionID string) string {
	key, err := base64.RawURLEncoding.DecodeString(sessionSecret)
	if err != nil || len(key) != 32 || !validID(deployment) || !validID(sessionID) {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("PairRoom/enterprise/browser/csrf/v1\x00" + deployment + "\x00" + sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func setCookie(w http.ResponseWriter, name, value string, expires time.Time, httpOnly bool) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: maxAge})
}

func clearCookie(w http.ResponseWriter, name string, httpOnly bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Path: "/", Secure: true, HttpOnly: httpOnly, SameSite: http.SameSiteLaxMode, Expires: time.Unix(1, 0), MaxAge: -1})
}

func decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return false
	}
	if r.ContentLength > maxRequestBytes {
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil || !utf8.Valid(body) || !uniqueJSONKeys(body) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return false
	}
	return decoder.Decode(new(any)) == io.EOF
}

// Request JSON has one interpretation: duplicate keys and case-folded aliases
// are rejected before the ordinary typed decoder handles field/type validation.
func uniqueJSONKeys(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 16 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || !canonicalJSONKey(name) || seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for decoder.More() {
				if !value(depth + 1) {
					return false
				}
			}
			end, err := decoder.Token()
			return err == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func canonicalJSONKey(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{code})
}

func validID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for i, r := range value {
		alphanumeric := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alphanumeric && (i == 0 || r != '-' && r != '_' && r != '.') {
			return false
		}
	}
	return true
}
