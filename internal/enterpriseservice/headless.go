package enterpriseservice

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

const headlessPollKind = "device.enrollment.headless.poll"

type HeadlessDeviceBackend interface {
	PollHeadlessDeviceEnrollment(context.Context, sqlstore.HeadlessEnrollmentPoll) (sqlstore.Result, error)
	ResolveHeadlessDeviceEnrollmentPoll(context.Context, sqlstore.HeadlessEnrollmentPoll) (sqlstore.Result, error)
	CompleteHeadlessDeviceEnrollment(context.Context, sqlstore.HeadlessEnrollmentCompletion) (sqlstore.Result, error)
}

type HeadlessBrowserBackend interface {
	DecideHeadlessDeviceEnrollment(context.Context, sqlstore.HeadlessEnrollmentDecision) (sqlstore.Result, error)
}

var _ HeadlessDeviceBackend = (*sqlstore.Store)(nil)
var _ HeadlessBrowserBackend = (*sqlstore.Store)(nil)

func validHeadlessRequestID(value string) bool {
	if !strings.HasPrefix(value, "request-") || len(value) != len("request-")+32 {
		return false
	}
	data, err := hex.DecodeString(strings.TrimPrefix(value, "request-"))
	return err == nil && len(data) == 16 && hex.EncodeToString(data) == value[len("request-"):]
}

func validCandidateFingerprint(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 16 && hex.EncodeToString(data) == value
}

func (d *DeviceHandler) headlessPoll(w http.ResponseWriter, r *http.Request, id string, resolve bool) {
	backend, ok := d.backend.(HeadlessDeviceBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	var request struct {
		ClientProof privateDeviceInput `json:"client_proof"`
		Sequence    uint64             `json:"sequence"`
	}
	if !decodeRequest(w, r, &request) || !validOpaqueSecret(string(request.ClientProof)) || request.Sequence < 1 || request.Sequence > 120 {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	command := sqlstore.HeadlessEnrollmentPoll{RequestID: id, Origin: d.policy.origin, ProofHash: tokenHash(string(request.ClientProof)), Sequence: request.Sequence}
	var result sqlstore.Result
	var err error
	if resolve {
		result, err = backend.ResolveHeadlessDeviceEnrollmentPoll(r.Context(), command)
	} else {
		result, err = backend.PollHeadlessDeviceEnrollment(r.Context(), command)
	}
	if err == nil && confirmed(result) {
		metadata, exists := result.HeadlessEnrollmentResult()
		if !exists || metadata.RequestID != id || metadata.Sequence != request.Sequence {
			fail(w, http.StatusServiceUnavailable, "device_unavailable")
			return
		}
	}
	writeHeadlessOperation(w, result, err, headlessPollKind, "", "")
}

func (d *DeviceHandler) headlessComplete(w http.ResponseWriter, r *http.Request, id string) {
	backend, ok := d.backend.(HeadlessDeviceBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	var request struct {
		ClientID         string             `json:"client_id"`
		ClientProof      privateDeviceInput `json:"client_proof"`
		ConfirmationCode privateDeviceInput `json:"confirmation_code"`
	}
	if !decodeRequest(w, r, &request) || !validID(request.ClientID) || !validOpaqueSecret(string(request.ClientProof)) {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	confirmation, err := enterpriseauth.HeadlessConfirmationHash(string(request.ConfirmationCode))
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	result, err := backend.CompleteHeadlessDeviceEnrollment(r.Context(), sqlstore.HeadlessEnrollmentCompletion{RequestID: id, ClientID: request.ClientID, Origin: d.policy.origin, ProofHash: tokenHash(string(request.ClientProof)), ConfirmationHash: confirmation})
	if err == nil && confirmed(result) && (!deviceReceiptMatches(result, sqlstore.ControlHeadlessComplete) || result.Control.RequestID != id) {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	writeHeadlessOperation(w, result, err, sqlstore.ControlHeadlessComplete, "", "")
}

func (h *Handler) headlessDecision(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession, info sqlstore.DeviceEnrollmentInfo, clientID string, approve bool, code string) {
	backend, ok := h.backend.(HeadlessBrowserBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	var confirmation [32]byte
	var err error
	if approve {
		confirmation, err = enterpriseauth.HeadlessConfirmationHash(code)
	} else if code != "" {
		err = enterpriseauth.ErrDeviceRequestInvalid
	}
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_device_request")
		return
	}
	result, err := backend.DecideHeadlessDeviceEnrollment(r.Context(), sqlstore.HeadlessEnrollmentDecision{Auth: session.Auth, RequestID: info.RequestID, ClientID: clientID, Approve: approve, ConfirmationHash: confirmation})
	if err == nil && confirmed(result) && (!deviceReceiptMatches(result, sqlstore.ControlHeadlessDecision) || result.Control.RequestID != info.RequestID || result.Control.DeviceID != info.DeviceID || result.Control.UserID != session.Auth.UserID) {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	writeHeadlessOperation(w, result, err, sqlstore.ControlHeadlessDecision, "", "")
}

func writeHeadlessOperation(w http.ResponseWriter, result sqlstore.Result, err error, kind, verification, fingerprint string) {
	if err != nil || !confirmed(result) {
		writeOperation(w, result, err)
		return
	}
	metadata, exists := result.HeadlessEnrollmentResult()
	if !exists || !validHeadlessRequestID(metadata.RequestID) || metadata.ExpiresAt.IsZero() || metadata.Sequence > 120 {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	status := http.StatusOK
	switch metadata.Disposition {
	case "":
	case "confirmation_rejected":
		status = http.StatusBadRequest
	case "verification_exhausted", "polling_exhausted":
		status = http.StatusTooManyRequests
	default:
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	switch metadata.Status {
	case "pending", "approved", "completed", "denied", "invalidated", "expired":
	case "slow_down":
		status = http.StatusTooManyRequests
	default:
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	if kind != headlessPollKind && !deviceReceiptMatches(result, kind) {
		fail(w, http.StatusServiceUnavailable, "device_unavailable")
		return
	}
	response := struct {
		Outcome              sqlstore.Outcome `json:"outcome"`
		Kind                 string           `json:"kind"`
		TransportMode        string           `json:"transport_mode"`
		RequestID            string           `json:"request_id"`
		DeviceID             string           `json:"device_id,omitempty"`
		UserID               string           `json:"user_id,omitempty"`
		GrantID              string           `json:"grant_id,omitempty"`
		Status               string           `json:"status"`
		Disposition          string           `json:"disposition,omitempty"`
		Sequence             uint64           `json:"sequence"`
		ExpiresAt            time.Time        `json:"expires_at"`
		NextPollAt           time.Time        `json:"next_poll_at,omitzero"`
		RefreshExpiresAt     time.Time        `json:"refresh_expires_at,omitzero"`
		VerificationURL      string           `json:"verification_url,omitempty"`
		CandidateFingerprint string           `json:"candidate_fingerprint,omitempty"`
	}{Outcome: result.Outcome, Kind: kind, TransportMode: sqlstore.DeviceTransportHeadless, RequestID: metadata.RequestID, Status: metadata.Status, Disposition: metadata.Disposition, Sequence: metadata.Sequence, ExpiresAt: metadata.ExpiresAt, NextPollAt: metadata.NextPollAt, VerificationURL: verification, CandidateFingerprint: fingerprint}
	if c := result.Control; c != nil {
		response.DeviceID, response.UserID, response.GrantID = c.DeviceID, c.UserID, c.GrantID
		if kind == sqlstore.ControlHeadlessComplete {
			response.ExpiresAt, response.RefreshExpiresAt = c.ExpiresAt, c.RefreshExpiresAt
		}
	}
	writeJSON(w, status, response)
}
