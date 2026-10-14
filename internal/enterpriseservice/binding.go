package enterpriseservice

import (
	"context"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// BindingBackend admits endpoint intent, Owner approval and endpoint proof as
// three separate typed operations. It never lets a browser select an Agent's
// Runtime or possess its preallocated Binding credential.
type BindingBackend interface {
	CommitBindingRequest(context.Context, sqlstore.BindingRequestCommand) (sqlstore.Result, error)
	ResolveBindingRequest(context.Context, sqlstore.BindingRequestCommand) (sqlstore.Result, error)
	DecideBindingRequest(context.Context, sqlstore.BindingDecisionCommand) (sqlstore.Result, error)
	ResolveBindingDecision(context.Context, sqlstore.BindingDecisionCommand) (sqlstore.Result, error)
	CompleteBindingRequest(context.Context, sqlstore.BindingCompletionCommand) (sqlstore.Result, error)
	ResolveBindingCompletion(context.Context, sqlstore.BindingCompletionCommand) (sqlstore.Result, error)
	ReadBindingRequest(context.Context, sqlstore.AuthContext, string, string) (sqlstore.BindingRequestInfo, error)
	ListBindingRequests(context.Context, sqlstore.AuthContext, string) ([]sqlstore.BindingRequestInfo, error)
	ReadBindingContext(context.Context, sqlstore.AuthContext, string) (sqlstore.BindingContext, error)
	RenewBindingCredential(context.Context, sqlstore.BindingRenewalCommand) (sqlstore.Result, error)
	ResolveBindingRenewal(context.Context, sqlstore.BindingRenewalCommand) (sqlstore.Result, error)
}

var _ BindingBackend = (*sqlstore.Store)(nil)

type bindingRequestBody struct {
	ClientID           string        `json:"client_id"`
	RequestID          string        `json:"request_id"`
	AgentID            string        `json:"agent_id"`
	BindingID          string        `json:"binding_id"`
	Slot               model.ActorID `json:"slot"`
	ObservedGeneration uint64        `json:"observed_generation"`
	Replace            bool          `json:"replace"`
	ParkEnabled        bool          `json:"park_enabled"`
	ProofHash          string        `json:"proof_hash"`
	Grant              struct {
		GrantID   string `json:"grant_id"`
		TokenHash string `json:"token_hash"`
	} `json:"grant"`
}

type bindingCompletionBody struct {
	ClientID    string             `json:"client_id"`
	ClientProof privateDeviceInput `json:"client_proof"`
}
type bindingDecisionBody struct {
	ClientID string `json:"client_id"`
	Action   string `json:"action"`
}

type bindingRenewalBody struct {
	ClientID   string `json:"client_id"`
	Generation uint64 `json:"generation"`
	Grant      struct {
		GrantID   string `json:"grant_id"`
		TokenHash string `json:"token_hash"`
	} `json:"grant"`
}

// bindingPath only claims the Binding subresource. Every selected path returns
// one Room and an optional request/action; unrelated Room endpoints stay with
// their owning handler and cannot be silently reinterpreted as Binding input.
func bindingPath(path, prefix string) (roomID, requestID, action string, matched, valid bool) {
	if !strings.HasPrefix(path, prefix) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) >= 2 && parts[1] == "binding-context" {
		matched = true
		if len(parts) == 2 && validID(parts[0]) {
			roomID = parts[0]
			action = "context"
			valid = true
		}
		return
	}
	if len(parts) < 2 || parts[1] != "binding-requests" {
		return
	}
	matched = true
	if len(parts) > 4 || !validID(parts[0]) {
		return
	}
	roomID = parts[0]
	if len(parts) >= 3 {
		if !validID(parts[2]) {
			return
		}
		requestID = parts[2]
	}
	if len(parts) == 4 {
		if !validID(parts[3]) {
			return
		}
		action = parts[3]
	}
	valid = true
	return
}

func bindingNativeEnvelope(r *http.Request, deployment string) bool {
	return deviceEnvelope(r, deployment, true) || deviceReadEnvelope(r, deployment, true)
}

func (d *DeviceHandler) bindingTransport(w http.ResponseWriter, r *http.Request) bool {
	roomID, requestID, action, matched, valid := bindingPath(r.URL.Path, "/enterprise/device/v1/rooms/")
	var bindingID string
	var renewal bool
	if !matched {
		const prefix = "/enterprise/device/v1/rooms/"
		if strings.HasPrefix(r.URL.Path, prefix) {
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
			if len(parts) >= 2 && parts[1] == "bindings" {
				matched = true
				renewal = true
				valid = len(parts) == 4 && validID(parts[0]) && validID(parts[2]) && (parts[3] == "renew" || parts[3] == "resolve-renewal")
				if valid {
					roomID = parts[0]
					bindingID = parts[2]
					action = parts[3]
				}
			}
		}
	}
	if !matched {
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set(DeviceDeploymentHeader, d.policy.deployment)
	if !valid || !d.policy.acceptsTransport(r) || !bindingNativeEnvelope(r, d.policy.deployment) {
		fail(w, http.StatusBadRequest, "invalid_binding_request")
		return true
	}
	backend, ok := d.backend.(BindingBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "binding_unavailable")
		return true
	}
	hash, ok := deviceBearerHash(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "device_credential_invalid")
		return true
	}
	credential, err := d.backend.AuthenticateDevice(r.Context(), hash, d.policy.origin)
	if err != nil {
		writeDeviceError(w, err)
		return true
	}
	if credential.Auth.Deployment != d.policy.deployment || credential.Auth.UserID == "" || credential.Auth.SessionID != "" || credential.Auth.BindingGrantID != "" || !validID(credential.GrantID) || credential.Auth.DeviceGrantID != credential.GrantID || !validID(credential.DeviceID) || !credential.ExpiresAt.After(time.Now()) {
		fail(w, http.StatusUnauthorized, "device_credential_invalid")
		return true
	}
	if renewal {
		d.renewBinding(w, r, backend, credential.Auth, roomID, bindingID, action == "resolve-renewal")
		return true
	}
	if action == "context" {
		if r.Method != http.MethodGet {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		} else {
			writeBindingContext(w, r, backend, credential.Auth, roomID)
		}
		return true
	}
	if r.Method == http.MethodGet {
		if action != "" {
			fail(w, http.StatusNotFound, "not_found")
			return true
		}
		writeBindingRead(w, r, backend, credential.Auth, roomID, requestID)
		return true
	}
	if requestID == "" || action == "resolve-request" {
		var input bindingRequestBody
		if !decodeRequest(w, r, &input) || !validID(input.ClientID) || !validID(input.RequestID) || !validID(input.AgentID) || !validID(input.BindingID) || !validID(input.Grant.GrantID) || !input.Slot.ValidParticipant() || input.ObservedGeneration == 0 || input.ObservedGeneration > math.MaxInt64 || (requestID != "" && requestID != input.RequestID) {
			fail(w, http.StatusBadRequest, "invalid_binding_request")
			return true
		}
		proof, pok := deviceVerifierHash(input.ProofHash)
		token, tok := deviceVerifierHash(input.Grant.TokenHash)
		if !pok || !tok || proof == token {
			fail(w, http.StatusBadRequest, "invalid_binding_request")
			return true
		}
		command := sqlstore.BindingRequestCommand{Auth: credential.Auth, ClientID: input.ClientID, RequestID: input.RequestID, RoomID: roomID, AgentID: input.AgentID, BindingID: input.BindingID, Slot: input.Slot, ObservedGeneration: input.ObservedGeneration, Replace: input.Replace, ParkEnabled: input.ParkEnabled, ProofHash: proof, Grant: sqlstore.BindingGrantCandidate{GrantID: input.Grant.GrantID, TokenHash: token}}
		var result sqlstore.Result
		var err error
		if action == "resolve-request" {
			result, err = backend.ResolveBindingRequest(r.Context(), command)
		} else {
			result, err = backend.CommitBindingRequest(r.Context(), command)
		}
		writeBindingOperation(w, result, err, "request", roomID, input.RequestID)
		return true
	}
	if action == "complete" || action == "resolve-completion" {
		var input bindingCompletionBody
		if !decodeRequest(w, r, &input) || !validID(input.ClientID) || !validOpaqueSecret(string(input.ClientProof)) {
			fail(w, http.StatusBadRequest, "invalid_binding_request")
			return true
		}
		command := sqlstore.BindingCompletionCommand{Auth: credential.Auth, ClientID: input.ClientID, RoomID: roomID, RequestID: requestID, ProofHash: tokenHash(string(input.ClientProof))}
		var result sqlstore.Result
		var err error
		if action == "resolve-completion" {
			result, err = backend.ResolveBindingCompletion(r.Context(), command)
		} else {
			result, err = backend.CompleteBindingRequest(r.Context(), command)
		}
		writeBindingOperation(w, result, err, "complete", roomID, requestID)
		return true
	}
	fail(w, http.StatusNotFound, "not_found")
	return true
}

func (d *DeviceHandler) renewBinding(w http.ResponseWriter, r *http.Request, backend BindingBackend, auth sqlstore.AuthContext, roomID, bindingID string, resolve bool) {
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	var input bindingRenewalBody
	if !decodeRequest(w, r, &input) || !validID(input.ClientID) || !validID(input.Grant.GrantID) || input.Generation == 0 || input.Generation > math.MaxInt64 {
		fail(w, http.StatusBadRequest, "invalid_binding_request")
		return
	}
	hash, ok := deviceVerifierHash(input.Grant.TokenHash)
	if !ok {
		fail(w, http.StatusBadRequest, "invalid_binding_request")
		return
	}
	command := sqlstore.BindingRenewalCommand{Auth: auth, ClientID: input.ClientID, RoomID: roomID, BindingID: bindingID, Generation: input.Generation, Grant: sqlstore.BindingGrantCandidate{GrantID: input.Grant.GrantID, TokenHash: hash}}
	var result sqlstore.Result
	var err error
	if resolve {
		result, err = backend.ResolveBindingRenewal(r.Context(), command)
	} else {
		result, err = backend.RenewBindingCredential(r.Context(), command)
	}
	if err != nil || !confirmed(result) {
		writeOperation(w, result, err)
		return
	}
	receipt := result.BindingAuthority
	if receipt == nil || receipt.Version != 1 || receipt.Kind != "renew" || receipt.Request != nil || receipt.Binding == nil || receipt.Binding.RoomID != roomID || receipt.Binding.BindID != bindingID || receipt.Binding.Generation != input.Generation || !receipt.Binding.Active || receipt.GrantID != input.Grant.GrantID || receipt.ExpiresAt.IsZero() || receipt.ExpiresAt.Year() < 1970 || receipt.ExpiresAt.Year() > 9999 {
		fail(w, http.StatusServiceUnavailable, "binding_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Outcome sqlstore.Outcome                  `json:"outcome"`
		Receipt *sqlstore.BindingAuthorityReceipt `json:"receipt"`
	}{result.Outcome, receipt})
}

func (h *Handler) bindingBrowser(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) bool {
	roomID, requestID, action, matched, valid := bindingPath(r.URL.Path, "/enterprise/api/v1/rooms/")
	if !matched {
		return false
	}
	if !valid {
		fail(w, http.StatusBadRequest, "invalid_binding_request")
		return true
	}
	backend, ok := h.backend.(BindingBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "binding_unavailable")
		return true
	}
	if action == "context" {
		if r.Method != http.MethodGet {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		} else {
			writeBindingContext(w, r, backend, session.Auth, roomID)
		}
		return true
	}
	if r.Method == http.MethodGet && action == "" {
		writeBindingRead(w, r, backend, session.Auth, roomID, requestID)
		return true
	}
	if r.Method != http.MethodPost || !h.browserMutationProof(r, session) {
		fail(w, http.StatusForbidden, "invalid_binding_request")
		return true
	}
	if requestID == "" || (action != "decision" && action != "resolve-decision") {
		fail(w, http.StatusNotFound, "not_found")
		return true
	}
	var input bindingDecisionBody
	if !decodeRequest(w, r, &input) || !validID(input.ClientID) || (input.Action != "approve" && input.Action != "deny") {
		fail(w, http.StatusBadRequest, "invalid_binding_request")
		return true
	}
	command := sqlstore.BindingDecisionCommand{Auth: session.Auth, ClientID: input.ClientID, RoomID: roomID, RequestID: requestID, Approve: input.Action == "approve"}
	var result sqlstore.Result
	var err error
	if action == "resolve-decision" {
		result, err = backend.ResolveBindingDecision(r.Context(), command)
	} else {
		result, err = backend.DecideBindingRequest(r.Context(), command)
	}
	writeBindingOperation(w, result, err, "decide", roomID, requestID)
	return true
}

func writeBindingRead(w http.ResponseWriter, r *http.Request, backend BindingBackend, auth sqlstore.AuthContext, roomID, requestID string) {
	if requestID != "" {
		info, err := backend.ReadBindingRequest(r.Context(), auth, roomID, requestID)
		if err != nil {
			writeDeviceError(w, err)
			return
		}
		if info.RoomID != roomID || info.RequestID != requestID {
			fail(w, http.StatusServiceUnavailable, "binding_unavailable")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Request sqlstore.BindingRequestInfo `json:"request"`
		}{info})
		return
	}
	requests, err := backend.ListBindingRequests(r.Context(), auth, roomID)
	if err != nil {
		writeDeviceError(w, err)
		return
	}
	if len(requests) > 32 {
		fail(w, http.StatusServiceUnavailable, "binding_unavailable")
		return
	}
	for _, info := range requests {
		if info.RoomID != roomID {
			fail(w, http.StatusServiceUnavailable, "binding_unavailable")
			return
		}
	}
	if requests == nil {
		requests = []sqlstore.BindingRequestInfo{}
	}
	writeJSON(w, http.StatusOK, struct {
		Requests []sqlstore.BindingRequestInfo `json:"requests"`
	}{requests})
}

func writeBindingOperation(w http.ResponseWriter, result sqlstore.Result, err error, kind, roomID, requestID string) {
	if err != nil || !confirmed(result) {
		writeOperation(w, result, err)
		return
	}
	receipt := result.BindingAuthority
	if receipt == nil || receipt.Version != 1 || receipt.Kind != kind || receipt.Request == nil || receipt.Request.RoomID != roomID || receipt.Request.RequestID != requestID {
		fail(w, http.StatusServiceUnavailable, "binding_unavailable")
		return
	}
	if receipt.Binding != nil && receipt.Binding.RoomID != roomID {
		fail(w, http.StatusServiceUnavailable, "binding_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Outcome sqlstore.Outcome                  `json:"outcome"`
		Receipt *sqlstore.BindingAuthorityReceipt `json:"receipt"`
	}{result.Outcome, receipt})
}

func writeBindingContext(w http.ResponseWriter, r *http.Request, backend BindingBackend, auth sqlstore.AuthContext, roomID string) {
	view, err := backend.ReadBindingContext(r.Context(), auth, roomID)
	if err != nil {
		writeDeviceError(w, err)
		return
	}
	if view.RoomID != roomID || view.UserID != auth.UserID || (view.Role != "owner" && view.Role != "member") || len(view.Slots) != 2 {
		fail(w, http.StatusServiceUnavailable, "binding_unavailable")
		return
	}
	for _, actor := range model.SlotActors() {
		slot, ok := view.Slots[actor]
		if !ok || slot.Slot != actor || slot.HeadGeneration == 0 || slot.HeadGeneration > math.MaxInt64 {
			fail(w, http.StatusServiceUnavailable, "binding_unavailable")
			return
		}
	}
	writeJSON(w, http.StatusOK, view)
}
