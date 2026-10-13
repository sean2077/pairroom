package enterpriseservice

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// DeviceBackend is the real Store's device-only surface. Browser credentials,
// device access and refresh proofs are never substituted for one another.
type DeviceBackend interface {
	RequiresSessionAuth() bool
	BeginDeviceEnrollment(context.Context, sqlstore.DeviceEnrollmentCommand) (sqlstore.Result, error)
	CompleteDeviceEnrollment(context.Context, sqlstore.DeviceEnrollmentCompletion) (sqlstore.Result, error)
	RotateDeviceCredential(context.Context, sqlstore.DeviceRotationCommand) (sqlstore.Result, error)
	RecoverDeviceRotation(context.Context, sqlstore.DeviceRotationCommand) (sqlstore.Result, error)
	AuthenticateDevice(context.Context, [32]byte, string) (sqlstore.DeviceCredential, error)
	RegisterAgent(context.Context, sqlstore.AuthContext, string, string, model.RuntimeKind) (sqlstore.Result, error)
}

type DeviceBrowserBackend interface {
	GetDeviceEnrollment(context.Context, sqlstore.AuthContext, string) (sqlstore.DeviceEnrollmentInfo, error)
	DecideDeviceEnrollment(context.Context, sqlstore.DeviceEnrollmentDecision) (sqlstore.Result, error)
	RevokeDevice(context.Context, sqlstore.AuthContext, string, string) (sqlstore.Result, error)
	ResolveDeviceRevocation(context.Context, sqlstore.AuthContext, string, string) (sqlstore.Result, error)
}

var _ DeviceBackend = (*sqlstore.Store)(nil)
var _ DeviceBrowserBackend = (*sqlstore.Store)(nil)

type DeviceHandler struct {
	policy  httpPolicy
	backend DeviceBackend
}

func NewDeviceHandler(cfg Config, backend DeviceBackend) (*DeviceHandler, error) {
	policy, err := makeHTTPPolicy(cfg)
	if err != nil || nilDependency(backend) || !backend.RequiresSessionAuth() {
		return nil, ErrConfiguration
	}
	return &DeviceHandler{policy: policy, backend: backend}, nil
}

// privateDeviceInput is JSON input, never a diagnostic value. Request structs
// remain safe under ordinary fmt/String/GoString formatting; handlers never log
// raw bodies, Authorization headers or callback completion Locations.
type privateDeviceInput string

func (privateDeviceInput) String() string   { return "<redacted>" }
func (privateDeviceInput) GoString() string { return "<redacted>" }

type deviceCandidateRequest struct {
	GrantID     string `json:"grant_id"`
	AccessHash  string `json:"access_hash"`
	RefreshHash string `json:"refresh_hash"`
}

func (r deviceCandidateRequest) candidate() (sqlstore.DeviceCredentialCandidate, bool) {
	access, aok := deviceVerifierHash(r.AccessHash)
	refresh, rok := deviceVerifierHash(r.RefreshHash)
	if !validID(r.GrantID) || !aok || !rok || access == refresh {
		return sqlstore.DeviceCredentialCandidate{}, false
	}
	return sqlstore.DeviceCredentialCandidate{GrantID: r.GrantID, AccessHash: access, RefreshHash: refresh}, true
}

type deviceBeginRequest struct {
	RequestID   string                 `json:"request_id"`
	DeviceID    string                 `json:"device_id"`
	Label       string                 `json:"label"`
	CallbackURI string                 `json:"callback_uri"`
	ClientState privateDeviceInput     `json:"client_state"`
	ProofHash   string                 `json:"proof_hash"`
	Credential  deviceCandidateRequest `json:"credential"`
}

type deviceCompleteRequest struct {
	ClientID       string             `json:"client_id"`
	ClientProof    privateDeviceInput `json:"client_proof"`
	CompletionCode privateDeviceInput `json:"completion_code"`
}

type deviceRotationRequest struct {
	DeviceID       string                 `json:"device_id"`
	CurrentGrantID string                 `json:"current_grant_id"`
	RotationID     string                 `json:"rotation_id"`
	ClientProof    privateDeviceInput     `json:"client_proof"`
	Next           deviceCandidateRequest `json:"next"`
}

func (d *DeviceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set(DeviceDeploymentHeader, d.policy.deployment)
	if !d.policy.acceptsTransport(r) {
		fail(w, http.StatusBadRequest, "invalid_service_origin")
		return
	}
	if r.URL.Path == "/enterprise/device/v1/deployment" {
		if r.Method != http.MethodGet || !deviceNoBrowserHeaders(r) || len(r.Header.Values("Authorization")) != 0 || len(r.Header.Values(DeviceDeploymentHeader)) > 1 {
			fail(w, http.StatusBadRequest, "invalid_device_request")
			return
		}
		if expected := r.Header.Get(DeviceDeploymentHeader); expected != "" && expected != d.policy.deployment {
			fail(w, http.StatusConflict, "deployment_mismatch")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			DeploymentID string `json:"deployment_id"`
			Origin       string `json:"origin"`
		}{d.policy.deployment, d.policy.origin})
		return
	}
	if r.URL.Path == "/enterprise/device/v1/session" {
		d.session(w, r)
		return
	}
	authenticated := r.URL.Path == "/enterprise/device/v1/credentials/rotate" || r.URL.Path == "/enterprise/device/v1/credentials/recover" || r.URL.Path == "/enterprise/device/v1/agents"
	if !deviceEnvelope(r, d.policy.deployment, authenticated) {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	switch r.URL.Path {
	case "/enterprise/device/v1/enrollments":
		d.begin(w, r)
	case "/enterprise/device/v1/credentials/rotate":
		d.rotate(w, r, false)
	case "/enterprise/device/v1/credentials/recover":
		d.rotate(w, r, true)
	case "/enterprise/device/v1/agents":
		d.registerAgent(w, r)
	default:
		prefix := "/enterprise/device/v1/enrollments/"
		if strings.HasPrefix(r.URL.Path, prefix) && strings.HasSuffix(r.URL.Path, "/complete") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/complete")
			if validID(id) {
				d.complete(w, r, id)
				return
			}
		}
		fail(w, http.StatusNotFound, "not_found")
	}
}

func (d *DeviceHandler) session(w http.ResponseWriter, r *http.Request) {
	if !deviceReadEnvelope(r, d.policy.deployment, true) {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	hash, ok := deviceBearerHash(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "device_credential_invalid")
		return
	}
	credential, err := d.backend.AuthenticateDevice(r.Context(), hash, d.policy.origin)
	if err != nil {
		var kernel *sqlstore.KernelError
		if errors.As(err, &kernel) && kernel.Class == sqlstore.ClassAuthorization {
			fail(w, http.StatusUnauthorized, "device_credential_invalid")
		} else {
			writeDeviceError(w, err)
		}
		return
	}
	if credential.Auth.Deployment != d.policy.deployment || credential.Auth.UserID == "" || credential.Auth.SessionID != "" || credential.Auth.BindingGrantID != "" || credential.Auth.DeviceGrantID != credential.GrantID || !validID(credential.GrantID) || !validID(credential.DeviceID) || !credential.ExpiresAt.After(time.Now()) {
		fail(w, http.StatusUnauthorized, "device_credential_invalid")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		DeploymentID     string    `json:"deployment_id"`
		Origin           string    `json:"origin"`
		UserID           string    `json:"user_id"`
		DeviceID         string    `json:"device_id"`
		GrantID          string    `json:"grant_id"`
		ExpiresAt        time.Time `json:"expires_at"`
		RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	}{d.policy.deployment, d.policy.origin, credential.Auth.UserID, credential.DeviceID, credential.GrantID, credential.ExpiresAt, credential.RefreshExpiresAt})
}

func (d *DeviceHandler) begin(w http.ResponseWriter, r *http.Request) {
	var request deviceBeginRequest
	if !decodeRequest(w, r, &request) {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	candidate, ok := request.Credential.candidate()
	proof, pok := deviceVerifierHash(request.ProofHash)
	if !ok || !pok || !validID(request.RequestID) || !validID(request.DeviceID) || !validDeviceLabel(request.Label) || enterpriseauth.ValidateDeviceCallback(request.CallbackURI, string(request.ClientState)) != nil {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	result, err := d.backend.BeginDeviceEnrollment(r.Context(), sqlstore.DeviceEnrollmentCommand{RequestID: request.RequestID, DeviceID: request.DeviceID, Label: request.Label, Origin: d.policy.origin, CallbackURI: request.CallbackURI, ClientState: string(request.ClientState), ProofHash: proof, Credential: candidate})
	if err == nil && confirmed(result) && (!deviceReceiptMatches(result, sqlstore.ControlDeviceEnrollment) || result.Control.RequestID != request.RequestID || result.Control.DeviceID != request.DeviceID) {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	var location string
	if err == nil && confirmed(result) && deviceReceiptMatches(result, sqlstore.ControlDeviceEnrollment) && result.Control.RequestID == request.RequestID {
		location = d.policy.origin + "/enterprise/device-enrollments/" + request.RequestID
	}
	writeDeviceOperation(w, result, err, sqlstore.ControlDeviceEnrollment, location, "")
}

func (d *DeviceHandler) complete(w http.ResponseWriter, r *http.Request, requestID string) {
	var request deviceCompleteRequest
	if !decodeRequest(w, r, &request) || !validID(request.ClientID) || !validOpaqueSecret(string(request.ClientProof)) || !validOpaqueSecret(string(request.CompletionCode)) {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	result, err := d.backend.CompleteDeviceEnrollment(r.Context(), sqlstore.DeviceEnrollmentCompletion{RequestID: requestID, ClientID: request.ClientID, Origin: d.policy.origin, ProofHash: tokenHash(string(request.ClientProof)), CodeHash: tokenHash(string(request.CompletionCode))})
	if err == nil && confirmed(result) && (!deviceReceiptMatches(result, sqlstore.ControlDeviceComplete) || result.Control.RequestID != requestID) {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	writeDeviceOperation(w, result, err, sqlstore.ControlDeviceComplete, "", "")
}

func (d *DeviceHandler) rotate(w http.ResponseWriter, r *http.Request, recovery bool) {
	refresh, ok := deviceBearerHash(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "device_credential_invalid")
		return
	}
	var request deviceRotationRequest
	if !decodeRequest(w, r, &request) || !validID(request.DeviceID) || !validID(request.CurrentGrantID) || !validID(request.RotationID) || !validOpaqueSecret(string(request.ClientProof)) {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	next, ok := request.Next.candidate()
	if !ok || next.GrantID == request.CurrentGrantID {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	command := sqlstore.DeviceRotationCommand{DeviceID: request.DeviceID, CurrentGrantID: request.CurrentGrantID, RotationID: request.RotationID, Origin: d.policy.origin, ClientProofHash: tokenHash(string(request.ClientProof)), CurrentRefreshHash: refresh, Next: next}
	var result sqlstore.Result
	var err error
	kind := sqlstore.ControlDeviceRotate
	if recovery {
		result, err = d.backend.RecoverDeviceRotation(r.Context(), command)
		kind = sqlstore.ControlDeviceRecover
	} else {
		result, err = d.backend.RotateDeviceCredential(r.Context(), command)
	}
	if err == nil && confirmed(result) && (!deviceReceiptMatches(result, kind) || result.Control.DeviceID != request.DeviceID || result.Control.GrantID != next.GrantID) {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	// A recovery is one separately authorized SQL action. This handler never
	// turns Unknown into a retry or falls back from recovery to rotation.
	writeDeviceOperation(w, result, err, kind, "", "")
}

func (d *DeviceHandler) registerAgent(w http.ResponseWriter, r *http.Request) {
	hash, ok := deviceBearerHash(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "device_credential_invalid")
		return
	}
	var request struct {
		ClientID  string             `json:"client_id"`
		SourceKey privateDeviceInput `json:"source_key"`
		Runtime   model.RuntimeKind  `json:"runtime"`
	}
	if !decodeRequest(w, r, &request) || !validID(request.ClientID) || !validOpaqueSecret(string(request.SourceKey)) || !request.Runtime.Valid() || request.Runtime != request.Runtime.Canonical() {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	credential, err := d.backend.AuthenticateDevice(r.Context(), hash, d.policy.origin)
	if err != nil {
		writeDeviceError(w, err)
		return
	}
	if credential.Auth.Deployment != d.policy.deployment || credential.Auth.UserID == "" || credential.Auth.SessionID != "" || credential.Auth.BindingGrantID != "" || credential.Auth.DeviceGrantID != credential.GrantID || !validID(credential.GrantID) || !validID(credential.DeviceID) || !credential.ExpiresAt.After(time.Now()) {
		fail(w, http.StatusUnauthorized, "device_credential_invalid")
		return
	}
	result, err := d.backend.RegisterAgent(r.Context(), credential.Auth, request.ClientID, string(request.SourceKey), request.Runtime)
	writeDeviceOperation(w, result, err, sqlstore.ControlAgentRegister, "", "")
}

type devicePublicInfo struct {
	RequestID string    `json:"request_id"`
	DeviceID  string    `json:"device_id"`
	Label     string    `json:"label"`
	Origin    string    `json:"origin"`
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at"`
}

// deviceBrowser is dispatched only after the main browser authentication
// boundary. The SQL methods recheck the exact live Session at each effect.
func (h *Handler) deviceBrowser(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) {
	if r.Method == http.MethodPost && !h.browserMutationProof(r, session) {
		fail(w, http.StatusForbidden, "invalid_device_request")
		return
	}
	backend, ok := h.backend.(DeviceBrowserBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	const prefix = "/enterprise/api/v1/device-enrollments/"
	if strings.HasPrefix(r.URL.Path, prefix) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
		if len(parts) < 1 || len(parts) > 2 || !validID(parts[0]) {
			fail(w, http.StatusNotFound, "not_found")
			return
		}
		info, err := backend.GetDeviceEnrollment(r.Context(), session.Auth, parts[0])
		if err != nil {
			writeDeviceError(w, err)
			return
		}
		if info.RequestID != parts[0] || info.Origin != h.policy.origin || enterpriseauth.ValidateDeviceCallback(info.CallbackURI, info.ClientState) != nil {
			fail(w, http.StatusServiceUnavailable, "device_unavailable")
			return
		}
		if r.Method == http.MethodGet && len(parts) == 1 {
			writeJSON(w, http.StatusOK, devicePublicInfo{info.RequestID, info.DeviceID, info.Label, info.Origin, info.Status, info.ExpiresAt})
			return
		}
		if r.Method != http.MethodPost || len(parts) != 2 || parts[1] != "decision" || !h.browserMutationProof(r, session) {
			fail(w, http.StatusForbidden, "invalid_device_request")
			return
		}
		var request struct {
			ClientID string `json:"client_id"`
			Action   string `json:"action"`
		}
		if !decodeRequest(w, r, &request) || !validID(request.ClientID) || (request.Action != "approve" && request.Action != "deny") {
			fail(w, http.StatusBadRequest, "invalid_device_request")
			return
		}
		approve := request.Action == "approve"
		var code string
		var codeHash [32]byte
		if approve {
			code, err = enterpriseauth.NewBrowserSecret()
			if err != nil {
				fail(w, http.StatusServiceUnavailable, "device_unavailable")
				return
			}
			codeHash = tokenHash(code)
		}
		result, err := backend.DecideDeviceEnrollment(r.Context(), sqlstore.DeviceEnrollmentDecision{Auth: session.Auth, ClientID: request.ClientID, RequestID: info.RequestID, Approve: approve, CodeHash: codeHash})
		if err == nil && confirmed(result) && (!deviceReceiptMatches(result, sqlstore.ControlDeviceDecision) || result.Control.RequestID != info.RequestID || result.Control.DeviceID != info.DeviceID || result.Control.UserID != session.Auth.UserID || (approve && result.Control.Status != "approved")) {
			fail(w, http.StatusServiceUnavailable, "device_unavailable")
			return
		}
		var completion string
		if err == nil && confirmed(result) && deviceReceiptMatches(result, sqlstore.ControlDeviceDecision) && approve && result.Control.RequestID == info.RequestID && result.Control.DeviceID == info.DeviceID {
			completion, err = enterpriseauth.DeviceCompletionRedirect(info.CallbackURI, info.ClientState, code)
		}
		writeDeviceOperation(w, result, err, sqlstore.ControlDeviceDecision, "", completion)
		return
	}
	const revokePrefix = "/enterprise/api/v1/devices/"
	resolveRevoke := strings.HasSuffix(r.URL.Path, "/resolve-revoke")
	if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, revokePrefix) && (strings.HasSuffix(r.URL.Path, "/revoke") || resolveRevoke) && h.browserMutationProof(r, session) {
		suffix := "/revoke"
		if resolveRevoke {
			suffix = "/resolve-revoke"
		}
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, revokePrefix), suffix)
		var request struct {
			ClientID string `json:"client_id"`
		}
		if !validID(id) || !decodeRequest(w, r, &request) || !validID(request.ClientID) {
			fail(w, http.StatusBadRequest, "invalid_device_request")
			return
		}
		var result sqlstore.Result
		var err error
		if resolveRevoke {
			result, err = backend.ResolveDeviceRevocation(r.Context(), session.Auth, request.ClientID, id)
		} else {
			result, err = backend.RevokeDevice(r.Context(), session.Auth, request.ClientID, id)
		}
		writeDeviceOperation(w, result, err, sqlstore.ControlDeviceRevoke, "", "")
		return
	}
	fail(w, http.StatusNotFound, "not_found")
}

// This repeats only request proof checks, not a cached authorization decision.
// It also protects new browser handlers against accidental pre-middleware wiring.
func (h *Handler) browserMutationProof(r *http.Request, session sqlstore.BrowserSession) bool {
	if r.Method != http.MethodPost || r.URL.RawQuery != "" || !h.policy.accepts(r) || !h.policy.sameOrigin(r) || session.Auth.Deployment != h.policy.deployment || session.Auth.SessionID == "" || session.Auth.DeviceGrantID != "" || session.Auth.BindingGrantID != "" {
		return false
	}
	secret, ok := readCookie(r, h.policy.sessionCookie)
	if !ok {
		return false
	}
	csrf := deriveCSRF(secret, h.policy.deployment, session.Auth.SessionID)
	hash := tokenHash(csrf)
	return len(r.Header.Values(CSRFHeader)) == 1 && validOpaqueSecret(r.Header.Get(CSRFHeader)) && subtle.ConstantTimeCompare(hash[:], session.CSRFHash[:]) == 1 && subtle.ConstantTimeCompare([]byte(csrf), []byte(r.Header.Get(CSRFHeader))) == 1
}

func deviceReceiptMatches(result sqlstore.Result, kind string) bool {
	return result.Control != nil && result.Control.Version == 1 && result.Control.Kind == kind
}

func writeDeviceOperation(w http.ResponseWriter, result sqlstore.Result, err error, kind, verification, completion string) {
	if err != nil || !confirmed(result) {
		writeOperation(w, result, err)
		return
	}
	if !deviceReceiptMatches(result, kind) {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	c := result.Control
	writeJSON(w, http.StatusOK, struct {
		Outcome            sqlstore.Outcome `json:"outcome"`
		Kind               string           `json:"kind"`
		RequestID          string           `json:"request_id,omitempty"`
		DeviceID           string           `json:"device_id,omitempty"`
		UserID             string           `json:"user_id,omitempty"`
		AgentID            string           `json:"agent_id,omitempty"`
		GrantID            string           `json:"grant_id,omitempty"`
		Status             string           `json:"status,omitempty"`
		ExpiresAt          time.Time        `json:"expires_at,omitempty"`
		RefreshExpiresAt   time.Time        `json:"refresh_expires_at,omitempty"`
		VerificationURL    string           `json:"verification_url,omitempty"`
		CompletionLocation string           `json:"completion_location,omitempty"`
	}{result.Outcome, c.Kind, c.RequestID, c.DeviceID, c.UserID, c.AgentID, c.GrantID, c.Status, c.ExpiresAt, c.RefreshExpiresAt, verification, completion})
}

func writeDeviceError(w http.ResponseWriter, err error) {
	var kernel *sqlstore.KernelError
	if errors.As(err, &kernel) {
		writeOperation(w, sqlstore.Result{Outcome: kernel.Outcome, Class: kernel.Class}, err)
		return
	}
	fail(w, http.StatusServiceUnavailable, "device_unavailable")
}
