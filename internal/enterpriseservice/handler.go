package enterpriseservice

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// LoginProvider is implemented by the configured OIDC Provider. There is no
// request-selectable issuer, client, callback, token endpoint or identity.
type LoginProvider interface {
	Begin(context.Context, enterpriseauth.TransactionStore, string, enterpriseauth.LoginIntent) (enterpriseauth.Authorization, error)
	Complete(context.Context, enterpriseauth.TransactionStore, string, string, string) (enterpriseauth.Completion, error)
}

// Backend is the SQL Store's typed, fenced surface. Authentication is repeated
// inside every operation, using the exact SessionID returned by the first lookup.
type Backend interface {
	RequiresSessionAuth() bool
	AdmitIdentity(context.Context, sqlstore.IdentityLoginCommand) (sqlstore.Result, error)
	AuthenticateSession(context.Context, [32]byte) (sqlstore.BrowserSession, error)
	RevokeSession(context.Context, sqlstore.AuthContext, string) (sqlstore.Result, error)
	CommitRoomCreate(context.Context, sqlstore.RoomCreateCommand) (sqlstore.Result, error)
	ResolveRoomCreate(context.Context, sqlstore.AuthContext, string, string, model.EnterpriseRoomIntent) (sqlstore.Result, error)
	ReadonlySnapshot(context.Context, sqlstore.AuthContext, string) (sqlstore.RoomSnapshot, error)
	CommitRoomMembership(context.Context, sqlstore.RoomMembershipCommand) (sqlstore.Result, error)
	ResolveRoomMembership(context.Context, sqlstore.AuthContext, string, string, sqlstore.RoomMembershipIntent) (sqlstore.Result, error)
}

var _ Backend = (*sqlstore.Store)(nil)

type Handler struct {
	policy       httpPolicy
	provider     LoginProvider
	transactions enterpriseauth.TransactionStore
	backend      Backend
}

func New(cfg Config, provider LoginProvider, transactions enterpriseauth.TransactionStore, backend Backend) (*Handler, error) {
	policy, err := makeHTTPPolicy(cfg)
	if err != nil || nilDependency(provider) || nilDependency(transactions) || nilDependency(backend) || !backend.RequiresSessionAuth() {
		return nil, ErrConfiguration
	}
	return &Handler{policy: policy, provider: provider, transactions: transactions, backend: backend}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if !h.policy.accepts(r) {
		fail(w, http.StatusBadRequest, "invalid_service_origin")
		return
	}
	if r.Method == http.MethodGet && h.serveLanding(w, r) {
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/enterprise/auth/login" {
		h.login(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/enterprise/auth/callback" {
		h.callback(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/enterprise/api/v1/") {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	if r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest, "invalid_query")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.Method == http.MethodPost && !h.policy.sameOrigin(r) {
		fail(w, http.StatusForbidden, "invalid_origin")
		return
	}
	session, csrf, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost && (len(r.Header.Values(CSRFHeader)) != 1 || !validOpaqueSecret(r.Header.Get(CSRFHeader)) || subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(csrf)) != 1) {
		fail(w, http.StatusForbidden, "invalid_csrf")
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/enterprise/api/v1/session":
		writeJSON(w, http.StatusOK, struct {
			UserID        string    `json:"user_id"`
			Administrator bool      `json:"administrator"`
			ExpiresAt     time.Time `json:"expires_at"`
			CSRF          string    `json:"csrf"`
		}{session.Auth.UserID, session.Administrator, session.ExpiresAt, csrf})
	case r.Method == http.MethodPost && r.URL.Path == "/enterprise/api/v1/session/logout":
		h.logout(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/enterprise/api/v1/rooms":
		h.createRoom(w, r, session)
	case strings.HasPrefix(r.URL.Path, "/enterprise/api/v1/rooms/"):
		h.room(w, r, session)
	default:
		fail(w, http.StatusNotFound, "not_found")
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	query, ok := strictQuery(r.URL.RawQuery, "return")
	if !ok {
		fail(w, http.StatusBadRequest, "invalid_login_request")
		return
	}
	returnPath := query.Get("return")
	if returnPath == "" {
		returnPath = "/enterprise/"
	}
	browser, ok := readCookie(r, h.policy.loginCookie)
	if !ok {
		var err error
		browser, err = enterpriseauth.NewBrowserSecret()
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "login_unavailable")
			return
		}
	}
	authorization, err := h.provider.Begin(r.Context(), h.transactions, browser, enterpriseauth.LoginIntent{Purpose: enterpriseauth.PurposeLogin, ReturnPath: returnPath})
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, enterpriseauth.ErrTransactionInvalid) {
			status = http.StatusBadRequest
		} else if errors.Is(err, enterpriseauth.ErrTransactionCapacity) {
			status = http.StatusTooManyRequests
		}
		fail(w, status, "login_unavailable")
		return
	}
	setCookie(w, h.policy.loginCookie, browser, authorization.ExpiresAt, true)
	w.Header().Set("Location", authorization.URL)
	w.WriteHeader(http.StatusSeeOther)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	query, ok := strictQuery(r.URL.RawQuery, "state", "code", "error", "error_description", "error_uri")
	if !ok || query.Get("state") == "" || (query.Get("error") != "" && query.Get("code") != "") {
		fail(w, http.StatusBadRequest, "invalid_login_callback")
		return
	}
	browser, ok := readCookie(r, h.policy.loginCookie)
	if !ok {
		fail(w, http.StatusUnauthorized, "login_invalid")
		return
	}
	// An IdP error still consumes the browser-bound transaction with an empty
	// code. Provider.Complete never exchanges an empty code or retries a failure.
	completion, err := h.provider.Complete(r.Context(), h.transactions, browser, query.Get("state"), query.Get("code"))
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, enterpriseauth.ErrProviderUnavailable) || errors.Is(err, enterpriseauth.ErrTransactionUnavailable) {
			status = http.StatusServiceUnavailable
		}
		fail(w, status, "login_invalid")
		return
	}
	if query.Get("error") != "" || completion.Purpose != enterpriseauth.PurposeLogin || completion.FreshReauthentication || !safeReturnPath(completion.ReturnPath) {
		fail(w, http.StatusUnauthorized, "login_invalid")
		return
	}
	secret, err := enterpriseauth.NewBrowserSecret()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "login_unavailable")
		return
	}
	id, err := enterpriseauth.NewBrowserSecret()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "login_unavailable")
		return
	}
	client, err := enterpriseauth.NewBrowserSecret()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "login_unavailable")
		return
	}
	sessionID := "session-" + id
	expires := time.Now().UTC().Add(h.policy.sessionTTL).Truncate(time.Microsecond)
	candidate := sqlstore.BrowserSessionCandidate{SessionID: sessionID, TokenHash: tokenHash(secret), CSRFHash: tokenHash(deriveCSRF(secret, h.policy.deployment, sessionID)), ExpiresAt: expires}
	result, err := h.backend.AdmitIdentity(r.Context(), sqlstore.IdentityLoginCommand{ClientID: "login-" + client, Completion: completion, Session: candidate})
	if err != nil || !confirmed(result) || result.Control == nil || result.Control.Version != 1 || result.Control.Kind != "identity.login" || result.Control.UserID == "" || result.Control.SessionID != sessionID || !result.Control.ExpiresAt.Equal(expires) {
		status := http.StatusServiceUnavailable
		if result.Outcome == sqlstore.OutcomeRefused {
			status = http.StatusForbidden
		}
		fail(w, status, "login_not_committed")
		return
	}
	setCookie(w, h.policy.sessionCookie, secret, expires, true)
	w.Header().Set("Location", completion.ReturnPath)
	w.WriteHeader(http.StatusSeeOther)
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (sqlstore.BrowserSession, string, bool) {
	secret, ok := readCookie(r, h.policy.sessionCookie)
	if !ok {
		fail(w, http.StatusUnauthorized, "session_invalid")
		return sqlstore.BrowserSession{}, "", false
	}
	session, err := h.backend.AuthenticateSession(r.Context(), tokenHash(secret))
	if err != nil {
		status := http.StatusServiceUnavailable
		var kernel *sqlstore.KernelError
		if errors.As(err, &kernel) && kernel.Class == sqlstore.ClassAuthorization {
			status = http.StatusUnauthorized
		}
		fail(w, status, "session_unavailable")
		return sqlstore.BrowserSession{}, "", false
	}
	if session.Auth.Deployment != h.policy.deployment || session.Auth.UserID == "" || !validID(session.Auth.SessionID) || !session.ExpiresAt.After(time.Now()) {
		fail(w, http.StatusUnauthorized, "session_invalid")
		return sqlstore.BrowserSession{}, "", false
	}
	csrf := deriveCSRF(secret, h.policy.deployment, session.Auth.SessionID)
	hash := tokenHash(csrf)
	if csrf == "" || subtle.ConstantTimeCompare(hash[:], session.CSRFHash[:]) != 1 {
		fail(w, http.StatusUnauthorized, "session_invalid")
		return sqlstore.BrowserSession{}, "", false
	}
	return session, csrf, true
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) {
	var request struct {
		ClientID string `json:"client_id"`
	}
	if !decodeRequest(w, r, &request) || !validID(request.ClientID) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := h.backend.RevokeSession(r.Context(), session.Auth, request.ClientID)
	if err == nil && confirmed(result) {
		clearCookie(w, h.policy.sessionCookie, true)
	}
	writeOperation(w, result, err)
}

type createRequest struct {
	RoomID   string                     `json:"room_id"`
	ClientID string                     `json:"client_id"`
	Intent   model.EnterpriseRoomIntent `json:"intent"`
}

func (h *Handler) createRoom(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) {
	var request createRequest
	if !decodeRequest(w, r, &request) || !validID(request.RoomID) || !validID(request.ClientID) || model.ValidateEnterpriseRoomIntent(request.Intent) != nil {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := h.backend.CommitRoomCreate(r.Context(), sqlstore.RoomCreateCommand{Auth: session.Auth, RoomID: request.RoomID, ClientID: request.ClientID, Intent: request.Intent})
	writeOperation(w, result, err)
}

func (h *Handler) room(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/enterprise/api/v1/rooms/"), "/")
	if len(parts) < 1 || len(parts) > 2 || !validID(parts[0]) {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	roomID := parts[0]
	if r.Method == http.MethodGet && len(parts) == 1 {
		snapshot, err := h.backend.ReadonlySnapshot(r.Context(), session.Auth, roomID)
		if err != nil {
			var kernel *sqlstore.KernelError
			if errors.As(err, &kernel) {
				writeOperation(w, sqlstore.Result{Outcome: kernel.Outcome, Class: kernel.Class}, err)
			} else {
				fail(w, http.StatusServiceUnavailable, "room_unavailable")
			}
			return
		}
		projection, err := relay.ProjectEnterpriseSnapshot(relay.EnterpriseRoomSnapshot{DeploymentID: snapshot.DeploymentID, OrgID: snapshot.OrgID, CreationUserID: snapshot.CreationUserID, RoomID: snapshot.RoomID, HostMode: snapshot.HostMode, Events: snapshot.Events, Seq: snapshot.Seq, Revision: snapshot.Revision})
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "room_unavailable")
			return
		}
		writeJSON(w, http.StatusOK, struct {
			Revision uint64 `json:"revision"`
			relay.Snapshot
		}{Revision: snapshot.Revision, Snapshot: projection})
		return
	}
	if r.Method != http.MethodPost || len(parts) != 2 {
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	switch parts[1] {
	case "resolve-create":
		var request struct {
			ClientID string                     `json:"client_id"`
			Intent   model.EnterpriseRoomIntent `json:"intent"`
		}
		if !decodeRequest(w, r, &request) || !validID(request.ClientID) || model.ValidateEnterpriseRoomIntent(request.Intent) != nil {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
		result, err := h.backend.ResolveRoomCreate(r.Context(), session.Auth, roomID, request.ClientID, request.Intent)
		writeOperation(w, result, err)
	case "memberships", "resolve-membership":
		var request struct {
			ClientID         string                        `json:"client_id"`
			ExpectedRevision uint64                        `json:"expected_revision"`
			Intent           sqlstore.RoomMembershipIntent `json:"intent"`
		}
		if !decodeRequest(w, r, &request) || !validID(request.ClientID) || model.ValidateEnterpriseMembershipIntent(request.Intent) != nil {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
		var result sqlstore.Result
		var err error
		if parts[1] == "memberships" {
			result, err = h.backend.CommitRoomMembership(r.Context(), sqlstore.RoomMembershipCommand{Auth: session.Auth, RoomID: roomID, ClientID: request.ClientID, ExpectedRevision: request.ExpectedRevision, Intent: request.Intent})
		} else {
			result, err = h.backend.ResolveRoomMembership(r.Context(), session.Auth, roomID, request.ClientID, request.Intent)
		}
		writeOperation(w, result, err)
	default:
		fail(w, http.StatusNotFound, "not_found")
	}
}

func confirmed(result sqlstore.Result) bool {
	return result.Outcome == sqlstore.OutcomeCommitted || result.Outcome == sqlstore.OutcomeReplayed
}

func writeOperation(w http.ResponseWriter, result sqlstore.Result, err error) {
	status := http.StatusOK
	reply := struct {
		Outcome  sqlstore.Outcome `json:"outcome"`
		Class    string           `json:"class,omitempty"`
		Seq      uint64           `json:"seq,omitempty"`
		Revision uint64           `json:"revision,omitempty"`
		Fact     *publicFact      `json:"fact,omitempty"`
	}{Outcome: result.Outcome, Class: result.Class}
	if err == nil && confirmed(result) {
		reply.Seq = result.Seq
		reply.Revision = result.Revision
		if result.Fact.ID != "" {
			reply.Fact = &publicFact{ID: result.Fact.ID, RoomID: result.Fact.RoomID, Kind: result.Fact.Kind, Seq: result.Fact.Seq, Actor: result.Fact.Actor, CreatedAt: result.Fact.CreatedAt}
		}
	} else {
		switch result.Outcome {
		case sqlstore.OutcomeRefused:
			status = http.StatusForbidden
			if result.Class == sqlstore.ClassInvalidRequest {
				status = http.StatusBadRequest
			} else if result.Class == sqlstore.ClassRoomNotFound {
				status = http.StatusNotFound
			} else if result.Class == sqlstore.ClassLeaseLost {
				status = http.StatusServiceUnavailable
			}
		case sqlstore.OutcomeConflict, sqlstore.OutcomeNotCommitted:
			status = http.StatusConflict
		default:
			status = http.StatusServiceUnavailable
			reply.Outcome = sqlstore.OutcomeUnknown
		}
	}
	writeJSON(w, status, reply)
}

// Raw Event.Data is private persistence encoding and may contain delivery or
// binding capabilities. Mutation receipts expose canonical identity only;
// authenticated Room contents use the relay's shared public projection.
type publicFact struct {
	ID        string        `json:"id"`
	RoomID    string        `json:"room_id"`
	Kind      string        `json:"kind"`
	Seq       uint64        `json:"seq"`
	Actor     model.ActorID `json:"actor"`
	CreatedAt time.Time     `json:"created_at"`
}

func strictQuery(raw string, allowed ...string) (url.Values, bool) {
	query, err := url.ParseQuery(raw)
	if err != nil {
		return nil, false
	}
	for key, values := range query {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
			}
		}
		if !found || len(values) != 1 {
			return nil, false
		}
	}
	return query, true
}

func safeReturnPath(value string) bool {
	return strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") && !strings.ContainsAny(value, "\\?#%\x00\r\n\t")
}

func nilDependency(value any) bool {
	if value == nil {
		return true
	}
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return rv.IsNil()
	default:
		return false
	}
}
