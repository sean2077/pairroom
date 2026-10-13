// Package enterpriseclient is the native endpoint's HTTPS device authority
// client. It keeps no vendor session identity and owns no background process.
package enterpriseclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const DeploymentHeader = "X-PairRoom-Deployment"
const devicePath = "/enterprise/device/v1"

var (
	ErrConfiguration   = errors.New("enterprise client configuration is invalid")
	ErrPrivateState    = errors.New("enterprise client private state is unavailable or invalid")
	ErrPending         = errors.New("enterprise client has an unresolved operation; use explicit recovery")
	ErrNoSession       = errors.New("enterprise client has no active device credential")
	ErrAlreadyLoggedIn = errors.New("enterprise client already has a device credential")
	ErrRetainedLimit   = errors.New("enterprise retained journal is full; explicitly reconcile saved operations before another login")
)

type Config struct {
	Origin, DeploymentID, Directory, CAFile string
	// OpenBrowser is the explicit foreground browser effect. Production passes
	// the existing platform opener; tests may provide a controlled browser.
	OpenBrowser func(string) error
}

type Client struct {
	origin, deployment, dir string
	http                    *http.Client
	openBrowser             func(string) error
}

type Error struct{ Outcome, Class string }

func (e *Error) Error() string {
	if e.Outcome == "committed" || e.Outcome == "replayed" {
		switch e.Class {
		case "verification_exhausted", "polling_exhausted":
			return "enterprise enrollment limit reached; the counter transaction was confirmed and this request was invalidated. This observation did not issue a Device credential"
		case "enrollment_expired":
			return "enterprise enrollment request expired; the observation was confirmed. This observation did not issue a Device credential"
		case "confirmation_rejected":
			return "enterprise enrollment code did not match; the failed confirmation was recorded. This confirmation did not issue a Device credential"
		}
	}
	return "enterprise operation " + e.Outcome + " (" + e.Class + ")"
}
func unknown(class string) error { return &Error{Outcome: "unknown", Class: class} }

type secret string

func (secret) String() string   { return "<redacted>" }
func (secret) GoString() string { return "<redacted>" }
func newSecret() (secret, error) {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		return "", errors.New("enterprise random source unavailable")
	}
	return secret(base64.RawURLEncoding.EncodeToString(b[:])), nil
}
func newID(prefix string) (string, error) { s, e := newSecret(); return prefix + "-" + string(s), e }
func validSecret(s secret) bool {
	b, e := base64.RawURLEncoding.DecodeString(string(s))
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == string(s)
}
func verifier(s secret) string {
	sum := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func validID(s string) bool {
	if s == "" || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}
func validOrigin(origin string) bool {
	u, e := url.Parse(origin)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Opaque == "" && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && origin == "https://"+u.Host && strings.ToLower(u.Host) == u.Host
}

func New(cfg Config) (*Client, error) {
	if !validOrigin(cfg.Origin) || !validID(cfg.DeploymentID) {
		return nil, ErrConfiguration
	}
	roots, e := x509.SystemCertPool()
	if e != nil {
		return nil, ErrConfiguration
	}
	if cfg.CAFile != "" {
		f, e := os.Open(cfg.CAFile)
		if e != nil {
			return nil, ErrConfiguration
		}
		data, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		_ = f.Close()
		if e != nil || len(data) > 1<<20 || !roots.AppendCertsFromPEM(data) {
			return nil, ErrConfiguration
		}
	}
	dir, e := prepareDirectory(cfg.Directory, cfg.Origin, cfg.DeploymentID)
	if e != nil {
		return nil, ErrPrivateState
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, DisableKeepAlives: true, ResponseHeaderTimeout: 65 * time.Second}
	return &Client{origin: cfg.Origin, deployment: cfg.DeploymentID, dir: dir, openBrowser: cfg.OpenBrowser, http: &http.Client{Transport: transport, Timeout: 70 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Close() { c.http.CloseIdleConnections() }

type operationResponse struct {
	Outcome, Kind, Class, Error string
	RequestID                   string    `json:"request_id"`
	DeviceID                    string    `json:"device_id"`
	UserID                      string    `json:"user_id"`
	AgentID                     string    `json:"agent_id"`
	GrantID                     string    `json:"grant_id"`
	Status                      string    `json:"status"`
	ExpiresAt                   time.Time `json:"expires_at"`
	RefreshExpiresAt            time.Time `json:"refresh_expires_at"`
	VerificationURL             string    `json:"verification_url"`
	CompletionLocation          string    `json:"completion_location"`
	TransportMode               string    `json:"transport_mode,omitempty"`
	Disposition                 string    `json:"disposition,omitempty"`
	CandidateFingerprint        string    `json:"candidate_fingerprint,omitempty"`
	Sequence                    uint64    `json:"sequence,omitempty"`
	NextPollAt                  time.Time `json:"next_poll_at,omitzero"`
}

func (r operationResponse) confirmed() bool {
	return r.Outcome == "committed" || r.Outcome == "replayed"
}

// Preflight carries neither bearer credentials nor proof material. It checks
// configuration, not authorization; every subsequent operation still performs
// its own live, fenced server authentication.
func (c *Client) preflight(ctx context.Context) error {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, c.origin+devicePath+"/deployment", nil)
	if e != nil {
		return ErrConfiguration
	}
	req.Header.Set(DeploymentHeader, c.deployment)
	response, e := c.http.Do(req)
	if e != nil {
		return &Error{Outcome: "not_committed", Class: "deployment_unavailable"}
	}
	defer response.Body.Close()
	var metadata struct {
		DeploymentID string `json:"deployment_id"`
		Origin       string `json:"origin"`
	}
	if response.StatusCode != http.StatusOK || len(response.Header.Values(DeploymentHeader)) != 1 || response.Header.Get(DeploymentHeader) != c.deployment || decodeBounded(response.Body, &metadata) != nil || metadata.DeploymentID != c.deployment || metadata.Origin != c.origin {
		return &Error{Outcome: "refused", Class: "deployment_mismatch"}
	}
	return nil
}
func decodeBounded(reader io.Reader, value any) error {
	data, e := io.ReadAll(io.LimitReader(reader, (64<<10)+1))
	if e != nil || len(data) > 64<<10 {
		return errors.New("invalid response")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(value); e != nil {
		return errors.New("invalid response")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid response")
	}
	return nil
}

// request is private shared transport for closed typed Device, Binding and
// Relay clients. It does not interpret operations or retry an effect.
func (c *Client) request(ctx context.Context, method, path string, body any, bearer secret, maxBytes int) (int, []byte, error) {
	if (method != http.MethodGet && method != http.MethodPost) || !strings.HasPrefix(path, devicePath+"/") || strings.ContainsAny(path, "?#\\") || path == devicePath+"/deployment" || maxBytes <= 0 || maxBytes > 2<<20 {
		return 0, nil, ErrConfiguration
	}
	var encoded []byte
	if method == http.MethodGet {
		if body != nil {
			return 0, nil, ErrConfiguration
		}
	} else {
		var e error
		encoded, e = json.Marshal(body)
		if e != nil || len(encoded) > 2<<20 {
			return 0, nil, ErrConfiguration
		}
	}
	return c.requestEncoded(ctx, method, path, encoded, "application/json", bearer, maxBytes)
}

// requestEncoded retains one security envelope for JSON and bounded raw object
// transfers. Callers enforce their own closed method/path/body budgets first.
func (c *Client) requestEncoded(ctx context.Context, method, path string, encoded []byte, contentType string, bearer secret, maxBytes int) (int, []byte, error) {
	if e := c.preflight(ctx); e != nil {
		return 0, nil, e
	}
	var reader io.Reader
	if encoded != nil {
		reader = strings.NewReader(string(encoded))
	}
	req, e := http.NewRequestWithContext(ctx, method, c.origin+path, reader)
	if e != nil {
		return 0, nil, ErrConfiguration
	}
	req.GetBody = nil
	if encoded != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set(DeploymentHeader, c.deployment)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+string(bearer))
	}
	response, e := c.http.Do(req)
	if e != nil {
		return 0, nil, unknown("response_lost")
	}
	defer response.Body.Close()
	if len(response.Header.Values(DeploymentHeader)) != 1 || response.Header.Get(DeploymentHeader) != c.deployment {
		return 0, nil, unknown("deployment_mismatch")
	}
	data, e := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if e != nil || len(data) > maxBytes {
		return 0, nil, unknown("invalid_response")
	}
	return response.StatusCode, data, nil
}

func (c *Client) post(ctx context.Context, path string, body any, bearer secret) (operationResponse, error) {
	status, data, e := c.request(ctx, http.MethodPost, path, body, bearer, 64<<10)
	if e != nil {
		return operationResponse{}, e
	}
	var result operationResponse
	if decodeBounded(strings.NewReader(string(data)), &result) != nil {
		return operationResponse{}, unknown("invalid_response")
	}
	if status == http.StatusOK && result.confirmed() {
		return result, nil
	}
	if result.Outcome == "not_committed" || result.Outcome == "refused" || result.Outcome == "conflict" || result.Outcome == "unknown" {
		result.Class = safeClass(result.Class)
		return result, &Error{Outcome: result.Outcome, Class: result.Class}
	}
	return operationResponse{}, unknown("operation_unconfirmed")
}

// A gateway or malformed peer cannot turn an error string into credential
// output. Only the protocol's fixed public class vocabulary reaches the CLI.
func safeClass(class string) string {
	switch class {
	case "prepared_head_changed", "object_missing", "resource_limit", "poll_observation_unavailable", "enrollment_expired", "confirmation_rejected", "verification_exhausted", "polling_exhausted":
		return class
	case "peer_changed", "revision_conflict", "room_not_found", "room_taken", "unsupported_op", "unsupported_namespace", "invalid_fact", "authorization", "invalid_request", "invalid_control_state", "payload_conflict", "state_invalid", "unsupported_control_state", "last_administrator", "operation_budget_exceeded", "connection_failed", "commit_uncertain", "connection_exception", "canceled", "closed", "lease_lost", "stored_fact_unreadable", "constraint_violation", "statement_failed", "serialization_failure", "deadlock_detected", "discovery_restart_exceeded", "range_exceeded", "rotation_recovery_required", "rotation_recovery_unavailable", "enrollment_approval_limit":
		return class
	default:
		return "operation_unconfirmed"
	}
}
func digest(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func (c *Client) String() string {
	return fmt.Sprintf("enterprise client %s deployment %s", c.origin, c.deployment)
}
