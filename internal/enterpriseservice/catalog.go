package enterpriseservice

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type CatalogBackend interface {
	ReadCatalog(context.Context, sqlstore.AuthContext, sqlstore.CatalogQuery) (sqlstore.CatalogPage, error)
	CommitCatalog(context.Context, sqlstore.CatalogCommand) (sqlstore.Result, error)
	ResolveCatalog(context.Context, sqlstore.CatalogCommand) (sqlstore.Result, error)
}

var _ CatalogBackend = (*sqlstore.Store)(nil)

const catalogPrefix = "/enterprise/api/v1/catalog/"

// Only these authenticated GET resources accept query parameters. Duplicates,
// unknown keys, oversized pages and selectors for another resource are refused.
func catalogQueryAllowed(r *http.Request) bool { _, ok := parseCatalogQuery(r); return ok }

func parseCatalogQuery(r *http.Request) (sqlstore.CatalogQuery, bool) {
	q := sqlstore.CatalogQuery{Resource: strings.TrimPrefix(r.URL.Path, catalogPrefix), Limit: 25}
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, catalogPrefix) || r.URL.ForceQuery {
		return q, false
	}
	allowed := []string{"after", "limit"}
	switch q.Resource {
	case "rooms":
		allowed = append(allowed, "project_id", "state")
	case "projects":
		allowed = append(allowed, "state")
	case "room", "room-members":
		allowed = append(allowed, "room_id")
	case "project-members":
		allowed = append(allowed, "project_id")
	case "devices", "agents", "sessions":
	default:
		return q, false
	}
	values, ok := strictQuery(r.URL.RawQuery, allowed...)
	if !ok {
		return q, false
	}
	q.After, q.RoomID, q.ProjectID, q.State = values.Get("after"), values.Get("room_id"), values.Get("project_id"), values.Get("state")
	if value, exists := values["limit"]; exists {
		limit, err := strconv.Atoi(value[0])
		if err != nil || limit < 1 || limit > 50 || strconv.Itoa(limit) != value[0] {
			return q, false
		}
		q.Limit = limit
	}
	for _, id := range []string{q.After, q.RoomID, q.ProjectID} {
		if id != "" && !validID(id) {
			return q, false
		}
	}
	if q.State != "" && q.State != "active" && q.State != "archived" && q.State != "all" {
		return q, false
	}
	if (q.Resource == "room" || q.Resource == "room-members") && q.RoomID == "" || q.Resource == "project-members" && q.ProjectID == "" {
		return q, false
	}
	return q, true
}

func (h *Handler) catalogBrowser(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) bool {
	if r.URL.Path == "/enterprise/api/v1/room-template" {
		if !h.browserMutationProof(r, session) {
			fail(w, http.StatusForbidden, "invalid_csrf")
			return true
		}
		var input struct {
			Name          string              `json:"name"`
			Collaboration model.Collaboration `json:"collaboration"`
		}
		if !decodeRequest(w, r, &input) {
			fail(w, http.StatusBadRequest, "invalid_room_template")
			return true
		}
		intent, err := model.PrepareEnterpriseRoomIntent(input.Name, input.Collaboration)
		if err != nil {
			fail(w, http.StatusBadRequest, "invalid_room_template")
			return true
		}
		writeJSON(w, http.StatusOK, struct {
			Intent model.EnterpriseRoomIntent `json:"intent"`
		}{intent})
		return true
	}
	if !strings.HasPrefix(r.URL.Path, catalogPrefix) {
		return false
	}
	backend, ok := h.backend.(CatalogBackend)
	if !ok || nilDependency(backend) {
		fail(w, http.StatusServiceUnavailable, "catalog_unavailable")
		return true
	}
	if r.Method == http.MethodGet {
		query, ok := parseCatalogQuery(r)
		if !ok {
			fail(w, http.StatusBadRequest, "invalid_catalog_query")
			return true
		}
		page, err := backend.ReadCatalog(r.Context(), session.Auth, query)
		if err != nil {
			writeDeviceError(w, err)
			return true
		}
		if page.Resource != query.Resource {
			fail(w, http.StatusServiceUnavailable, "catalog_unavailable")
			return true
		}
		writeJSON(w, http.StatusOK, page)
		return true
	}
	if r.URL.Path != catalogPrefix+"commands" && r.URL.Path != catalogPrefix+"resolve" {
		fail(w, http.StatusNotFound, "not_found")
		return true
	}
	if !h.browserMutationProof(r, session) {
		fail(w, http.StatusForbidden, "invalid_csrf")
		return true
	}
	var input struct {
		ClientID         string                 `json:"client_id"`
		ExpectedRevision uint64                 `json:"expected_revision"`
		Intent           sqlstore.CatalogIntent `json:"intent"`
	}
	if !decodeRequest(w, r, &input) || !validID(input.ClientID) {
		fail(w, http.StatusBadRequest, "invalid_catalog_command")
		return true
	}
	cmd := sqlstore.CatalogCommand{Auth: session.Auth, ClientID: input.ClientID, ExpectedRevision: input.ExpectedRevision, Intent: input.Intent}
	var result sqlstore.Result
	var err error
	if r.URL.Path == catalogPrefix+"resolve" {
		result, err = backend.ResolveCatalog(r.Context(), cmd)
	} else {
		result, err = backend.CommitCatalog(r.Context(), cmd)
	}
	if err != nil || !confirmed(result) {
		writeOperation(w, result, err)
		return true
	}
	if result.Catalog == nil || result.Catalog.Version != 1 || result.Catalog.Intent != input.Intent {
		fail(w, http.StatusServiceUnavailable, "catalog_unavailable")
		return true
	}
	writeJSON(w, http.StatusOK, struct {
		Outcome sqlstore.Outcome         `json:"outcome"`
		Receipt *sqlstore.CatalogReceipt `json:"receipt"`
	}{result.Outcome, result.Catalog})
	return true
}
