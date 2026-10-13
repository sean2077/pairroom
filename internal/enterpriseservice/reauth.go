package enterpriseservice

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// GovernanceBackend carries only fixed action metadata and verified identity.
// It exposes no Room-content read through administrator governance authority.
type GovernanceBackend interface {
	PrepareGovernanceReauthentication(context.Context, sqlstore.AuthContext, string, sqlstore.GovernanceIntent) (sqlstore.GovernanceAction, error)
	FinishGovernanceReauthentication(context.Context, sqlstore.AuthContext, enterpriseauth.Completion) (sqlstore.GovernanceAction, error)
	ReadGovernanceAction(context.Context, sqlstore.AuthContext, string) (sqlstore.GovernanceAction, error)
	ListGovernanceActions(context.Context, sqlstore.AuthContext) ([]sqlstore.GovernanceAction, error)
	TransferRoomOwnership(context.Context, sqlstore.OwnershipTransferCommand) (sqlstore.Result, error)
	ResolveRoomOwnership(context.Context, sqlstore.OwnershipTransferCommand) (sqlstore.Result, error)
}

var _ GovernanceBackend = (*sqlstore.Store)(nil)

func validGovernanceIntent(intent sqlstore.GovernanceIntent) bool {
	return validID(intent.RoomID) && validID(intent.TargetUserID) && validID(intent.ExpectedOwnerUserID) && intent.TargetUserID != intent.ExpectedOwnerUserID && intent.ExpectedRevision <= math.MaxInt64
}

// beginReauth runs after browser authentication/CSRF middleware and checks the
// request proof again before preparing an action. SQL locks are released before
// OIDC discovery or browser redirection can begin.
func (h *Handler) beginReauth(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) {
	if !h.browserMutationProof(r, session) {
		fail(w, http.StatusForbidden, "invalid_reauthentication_request")
		return
	}
	backend, ok := h.backend.(GovernanceBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
		return
	}
	var request struct {
		ClientID string                    `json:"client_id"`
		Intent   sqlstore.GovernanceIntent `json:"intent"`
	}
	if !decodeRequest(w, r, &request) || !validID(request.ClientID) || !validGovernanceIntent(request.Intent) {
		fail(w, http.StatusBadRequest, "invalid_reauthentication_request")
		return
	}
	action, err := backend.PrepareGovernanceReauthentication(r.Context(), session.Auth, request.ClientID, request.Intent)
	if err != nil {
		writeGovernanceError(w, err)
		return
	}
	if !validID(action.ID) || action.Intent != request.Intent || !action.ExpiresAt.After(time.Now()) {
		fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
		return
	}
	if action.Ready {
		writeJSON(w, http.StatusOK, struct {
			Action sqlstore.GovernanceAction `json:"action"`
		}{action})
		return
	}
	browser, ok := readCookie(r, h.policy.loginCookie)
	if !ok {
		browser, err = enterpriseauth.NewBrowserSecret()
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
			return
		}
	}
	authorization, err := h.provider.Begin(r.Context(), h.transactions, browser, enterpriseauth.LoginIntent{Purpose: enterpriseauth.PurposeReauthenticate, ReturnPath: "/enterprise/", CorrelationID: action.ID})
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
		return
	}
	setCookie(w, h.policy.loginCookie, browser, authorization.ExpiresAt, true)
	writeJSON(w, http.StatusOK, struct {
		Action           sqlstore.GovernanceAction `json:"action"`
		AuthorizationURL string                    `json:"authorization_url"`
	}{action, authorization.URL})
}

// finishReauth is called only with Provider.Complete's verified result, after
// the common callback consumed its browser-bound transaction exactly once.
// This path never admits an account, rotates a session or sets a new cookie.
func (h *Handler) finishReauth(w http.ResponseWriter, r *http.Request, completion enterpriseauth.Completion) {
	if completion.Purpose != enterpriseauth.PurposeReauthenticate || !completion.FreshReauthentication || !validID(completion.CorrelationID) || completion.Identity.AuthTime.IsZero() || completion.Identity.AuthTime.After(time.Now().Add(time.Minute)) || completion.ReturnPath != "/enterprise/" {
		fail(w, http.StatusUnauthorized, "reauthentication_invalid")
		return
	}
	session, _, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if session.Auth.SessionID == "" || session.Auth.DeviceGrantID != "" || session.Auth.BindingGrantID != "" {
		fail(w, http.StatusUnauthorized, "reauthentication_invalid")
		return
	}
	backend, ok := h.backend.(GovernanceBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
		return
	}
	action, err := backend.FinishGovernanceReauthentication(r.Context(), session.Auth, completion)
	if err != nil {
		writeGovernanceError(w, err)
		return
	}
	if !action.Ready || action.ID != completion.CorrelationID || !action.ExpiresAt.After(time.Now()) || !validGovernanceIntent(action.Intent) {
		fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
		return
	}
	w.Header().Set("Location", "/enterprise/")
	w.WriteHeader(http.StatusSeeOther)
}

// governanceActions returns only actions bound by SQL to this exact live
// browser Session. A caller returning from the IdP explicitly chooses among
// these bounded records; no latest-action guess or proof-bearing URL is used.
func (h *Handler) governanceActions(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) {
	backend, ok := h.backend.(GovernanceBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
		return
	}
	const prefix = "/enterprise/api/v1/governance/actions"
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.URL.Path == prefix {
		actions, err := backend.ListGovernanceActions(r.Context(), session.Auth)
		if err != nil {
			writeGovernanceError(w, err)
			return
		}
		if len(actions) > 32 {
			fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
			return
		}
		if actions == nil {
			actions = []sqlstore.GovernanceAction{}
		}
		writeJSON(w, http.StatusOK, struct {
			Actions []sqlstore.GovernanceAction `json:"actions"`
		}{actions})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, prefix+"/")
	if id == r.URL.Path || !validID(id) {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	action, err := backend.ReadGovernanceAction(r.Context(), session.Auth, id)
	if err != nil {
		writeGovernanceError(w, err)
		return
	}
	if action.ID != id {
		fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Action sqlstore.GovernanceAction `json:"action"`
	}{action})
}

func (h *Handler) transferOwnership(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession, roomID string) {
	h.ownershipOperation(w, r, session, roomID, false)
}

func (h *Handler) resolveOwnership(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession, roomID string) {
	h.ownershipOperation(w, r, session, roomID, true)
}

func (h *Handler) ownershipOperation(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession, roomID string, resolve bool) {
	if !h.browserMutationProof(r, session) {
		fail(w, http.StatusForbidden, "invalid_ownership_request")
		return
	}
	backend, ok := h.backend.(GovernanceBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "ownership_unavailable")
		return
	}
	var request struct {
		ClientID string                    `json:"client_id"`
		Intent   sqlstore.GovernanceIntent `json:"intent"`
		ActionID string                    `json:"action_id,omitempty"`
	}
	if !decodeRequest(w, r, &request) || !validID(request.ClientID) || !validGovernanceIntent(request.Intent) || request.Intent.RoomID != roomID || (request.ActionID != "" && !validID(request.ActionID)) {
		fail(w, http.StatusBadRequest, "invalid_ownership_request")
		return
	}
	command := sqlstore.OwnershipTransferCommand{Auth: session.Auth, ClientID: request.ClientID, Intent: request.Intent, GovernanceActionID: request.ActionID}
	var result sqlstore.Result
	var err error
	if resolve {
		result, err = backend.ResolveRoomOwnership(r.Context(), command)
	} else {
		result, err = backend.TransferRoomOwnership(r.Context(), command)
	}
	writeOperation(w, result, err)
}

func writeGovernanceError(w http.ResponseWriter, err error) {
	var kernel *sqlstore.KernelError
	if errors.As(err, &kernel) {
		writeOperation(w, sqlstore.Result{Outcome: kernel.Outcome, Class: kernel.Class}, err)
		return
	}
	fail(w, http.StatusServiceUnavailable, "reauthentication_unavailable")
}
