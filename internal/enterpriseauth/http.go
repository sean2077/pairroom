package enterpriseauth

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OutboundPolicy is operator configuration. Internal IdPs are supported by
// explicitly naming their HTTPS origin and, if necessary, private certificate CA.
// Request input and discovery never add origins to this allowlist.
type OutboundPolicy struct {
	AllowedOrigins   []string
	RootCAs          *x509.CertPool
	Timeout          time.Duration
	MaxResponseBytes int64
	// JWKS fetches include initial load and unknown-key refresh attempts. The
	// default budget permits two attempts per minute: initial load plus rotation.
	// Exhaustion fails closed until the next window; cached valid keys still work.
	JWKSRefreshWindow time.Duration
	JWKSRefreshBurst  int
}

type boundedTransport struct {
	base            *http.Transport
	origins         map[string]struct{}
	maxBytes        int64
	jwksURL         string
	jwksMu          sync.Mutex
	jwksWindow      time.Duration
	jwksBurst       int
	jwksWindowStart time.Time
	jwksAttempts    int
}

func newHTTPClient(policy OutboundPolicy) (*http.Client, *boundedTransport, error) {
	if len(policy.AllowedOrigins) == 0 || len(policy.AllowedOrigins) > 32 {
		return nil, nil, ErrConfiguration
	}
	origins := make(map[string]struct{}, len(policy.AllowedOrigins))
	for _, origin := range policy.AllowedOrigins {
		u, err := parseHTTPSURL(origin)
		if err != nil || u.Path != "" || u.RawQuery != "" || origin != "https://"+u.Host {
			return nil, nil, ErrConfiguration
		}
		origins[origin] = struct{}{}
	}
	timeout := policy.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	limit := policy.MaxResponseBytes
	if limit == 0 {
		limit = 1 << 20
	}
	if timeout <= 0 || timeout > time.Minute || limit < 1024 || limit > 4<<20 {
		return nil, nil, ErrConfiguration
	}
	window, burst := policy.JWKSRefreshWindow, policy.JWKSRefreshBurst
	if window == 0 {
		window = time.Minute
	}
	if burst == 0 {
		burst = 2
	}
	if window < time.Second || window > 5*time.Minute || burst < 2 || burst > 8 {
		return nil, nil, ErrConfiguration
	}
	var roots *x509.CertPool
	if policy.RootCAs != nil {
		roots = policy.RootCAs.Clone()
	}
	base := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		IdleConnTimeout:        30 * time.Second,
		MaxConnsPerHost:        4,
		MaxIdleConnsPerHost:    2,
		MaxIdleConns:           8,
		MaxResponseHeaderBytes: 32 << 10,
		ForceAttemptHTTP2:      true,
	}
	transport := &boundedTransport{base: base, origins: origins, maxBytes: limit, jwksWindow: window, jwksBurst: burst}
	client := &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return client, transport, nil
}

func parseHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.ForceQuery || strings.ContainsAny(raw, "\r\n\t\\") {
		return nil, ErrConfiguration
	}
	return u, nil
}

func (t *boundedTransport) allowed(raw string) bool {
	u, err := parseHTTPSURL(raw)
	if err != nil {
		return false
	}
	_, ok := t.origins["https://"+u.Host]
	return ok
}

// Read the bounded body before passing it to upstream decoders. This enforces
// the byte limit even for chunked, compressed, or missing/false Content-Length.
// No provider response body or URL is included in the errors leaving this layer.
func (t *boundedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.allowed(req.URL.String()) {
		return nil, ErrConfiguration
	}
	if req.URL.String() == t.jwksURL && !t.allowJWKS(time.Now()) {
		return nil, ErrProviderUnavailable
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, ErrProviderUnavailable
	}
	defer resp.Body.Close()
	if resp.ContentLength > t.maxBytes {
		return nil, ErrProviderUnavailable
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, t.maxBytes+1))
	if err != nil || int64(len(body)) > t.maxBytes {
		return nil, ErrProviderUnavailable
	}
	if resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		return nil, ErrProviderUnavailable
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

func (t *boundedTransport) allowJWKS(now time.Time) bool {
	t.jwksMu.Lock()
	defer t.jwksMu.Unlock()
	if t.jwksWindowStart.IsZero() || !now.Before(t.jwksWindowStart.Add(t.jwksWindow)) {
		t.jwksWindowStart = now
		t.jwksAttempts = 0
	}
	if t.jwksAttempts >= t.jwksBurst {
		return false
	}
	t.jwksAttempts++
	return true
}

func validateDiscovery(metadata map[string]any, transport *boundedTransport) error {
	for _, key := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri"} {
		value, ok := metadata[key].(string)
		if !ok || !transport.allowed(value) {
			return ErrConfiguration
		}
	}
	for key, value := range metadata {
		if strings.HasSuffix(key, "_endpoint") || strings.HasSuffix(key, "_uri") || key == "check_session_iframe" {
			endpoint, ok := value.(string)
			if !ok || !transport.allowed(endpoint) {
				return ErrConfiguration
			}
		}
		if key == "mtls_endpoint_aliases" {
			aliases, ok := value.(map[string]any)
			if !ok {
				return ErrConfiguration
			}
			for _, alias := range aliases {
				endpoint, ok := alias.(string)
				if !ok || !transport.allowed(endpoint) {
					return ErrConfiguration
				}
			}
		}
	}
	return nil
}

func transactionError(err error) error {
	if errors.Is(err, ErrTransactionInvalid) {
		return ErrTransactionInvalid
	}
	if errors.Is(err, ErrTransactionCapacity) {
		return ErrTransactionCapacity
	}
	return ErrTransactionUnavailable
}
