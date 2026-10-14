package enterpriseservice

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"github.com/sean2077/pairroom/internal/sqlstore"
)

type AdministrationBackend interface {
	ReadAdministration(context.Context, sqlstore.AuthContext, sqlstore.AdministrationQuery) (sqlstore.AdministrationPage, error)
	CommitAdministration(context.Context, sqlstore.AdministrationCommand) (sqlstore.Result, error)
	ResolveAdministration(context.Context, sqlstore.AdministrationCommand) (sqlstore.Result, error)
}

var _ AdministrationBackend = (*sqlstore.Store)(nil)

const administrationPrefix = "/enterprise/api/v1/administration/"

func administrationQueryAllowed(r *http.Request) bool {
	_, ok := parseAdministrationQuery(r)
	return ok
}
func parseAdministrationQuery(r *http.Request) (sqlstore.AdministrationQuery, bool) {
	query := sqlstore.AdministrationQuery{Resource: strings.TrimPrefix(r.URL.Path, administrationPrefix), Limit: 25}
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, administrationPrefix) || r.URL.ForceQuery {
		return query, false
	}
	switch query.Resource {
	case "users", "admissions", "administrators", "connections":
	default:
		return query, false
	}
	values, ok := strictQuery(r.URL.RawQuery, "after", "limit")
	if !ok {
		return query, false
	}
	query.After = values.Get("after")
	if len(query.After) > 8192 {
		return query, false
	}
	if query.After != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(query.After)
		if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != query.After {
			return query, false
		}
	}
	if values.Has("limit") {
		limit, err := strconv.Atoi(values.Get("limit"))
		if err != nil || limit < 1 || limit > 50 || strconv.Itoa(limit) != values.Get("limit") {
			return query, false
		}
		query.Limit = limit
	}
	return query, true
}

func (h *Handler) administrationBrowser(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) bool {
	if !strings.HasPrefix(r.URL.Path, administrationPrefix) {
		return false
	}
	if !session.Administrator || session.Auth.SessionID == "" || session.Auth.DeviceGrantID != "" || session.Auth.BindingGrantID != "" {
		fail(w, http.StatusForbidden, "administrator_required")
		return true
	}
	backend, ok := h.backend.(AdministrationBackend)
	if !ok || nilDependency(backend) {
		fail(w, http.StatusServiceUnavailable, "administration_unavailable")
		return true
	}
	if r.Method == http.MethodGet {
		query, ok := parseAdministrationQuery(r)
		if !ok {
			fail(w, http.StatusBadRequest, "invalid_administration_query")
			return true
		}
		page, err := backend.ReadAdministration(r.Context(), session.Auth, query)
		if err != nil {
			writeDeviceError(w, err)
			return true
		}
		if page.Resource != query.Resource || len(page.Users)+len(page.Administrators)+len(page.Admissions)+len(page.Connections) > query.Limit {
			fail(w, http.StatusServiceUnavailable, "administration_unavailable")
			return true
		}
		writeJSON(w, http.StatusOK, page)
		return true
	}
	if r.URL.Path != administrationPrefix+"commands" && r.URL.Path != administrationPrefix+"resolve" {
		fail(w, http.StatusNotFound, "not_found")
		return true
	}
	if !h.browserMutationProof(r, session) {
		fail(w, http.StatusForbidden, "invalid_csrf")
		return true
	}
	var input struct {
		ClientID string `json:"client_id"`
		Intent   struct {
			Kind         string                          `json:"kind"`
			TargetUserID string                          `json:"target_user_id,omitempty"`
			Identity     sqlstore.AdministrationIdentity `json:"identity,omitzero"`
			Enabled      *bool                           `json:"enabled"`
		} `json:"intent"`
	}
	if !decodeRequest(w, r, &input) || !validID(input.ClientID) || input.Intent.Enabled == nil {
		fail(w, http.StatusBadRequest, "invalid_administration_command")
		return true
	}
	intent := sqlstore.AdministrationIntent{Kind: input.Intent.Kind, TargetUserID: input.Intent.TargetUserID, Identity: input.Intent.Identity, Enabled: *input.Intent.Enabled}
	command := sqlstore.AdministrationCommand{Auth: session.Auth, ClientID: input.ClientID, Intent: intent}
	var result sqlstore.Result
	var err error
	if r.URL.Path == administrationPrefix+"resolve" {
		result, err = backend.ResolveAdministration(r.Context(), command)
	} else {
		result, err = backend.CommitAdministration(r.Context(), command)
	}
	if err != nil || !confirmed(result) {
		writeOperation(w, result, err)
		return true
	}
	if !administrationReceiptMatches(result.Control, session.Auth.UserID, intent) {
		fail(w, http.StatusServiceUnavailable, "administration_unavailable")
		return true
	}
	writeJSON(w, http.StatusOK, struct {
		Outcome sqlstore.Outcome         `json:"outcome"`
		Receipt *sqlstore.ControlReceipt `json:"receipt"`
	}{result.Outcome, result.Control})
	return true
}

func administrationReceiptMatches(receipt *sqlstore.ControlReceipt, actor string, intent sqlstore.AdministrationIntent) bool {
	if receipt == nil || receipt.Version != 1 || receipt.Seq == 0 {
		return false
	}
	expected := sqlstore.ControlReceipt{Version: 1, Kind: intent.Kind, Seq: receipt.Seq, UserID: actor, Enabled: intent.Enabled}
	switch intent.Kind {
	case "user.status", "administrator.set":
		expected.TargetUserID = intent.TargetUserID
	case "identity.admission":
		identity := intent.Identity
		hash := sha256.Sum256([]byte(identity.ConnectionID + "\x00" + identity.Issuer + "\x00" + identity.Subject))
		expected.IdentityHash = hex.EncodeToString(hash[:])
	default:
		return false
	}
	return *receipt == expected
}
