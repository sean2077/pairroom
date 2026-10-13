package enterpriseservice

import (
	"context"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
	"net/http"
)

type roomReadAuthorizationBackend interface {
	AuthorizeRoomRead(context.Context, sqlstore.AuthContext, string) error
}

// finalBrowserSession repeats current session/User authority after projection
// construction. Stored history is never an authorization grant at release time.
func (h *Handler) finalBrowserSession(w http.ResponseWriter, r *http.Request, original sqlstore.BrowserSession, roomID string) bool {
	current, _, ok := h.authenticate(w, r)
	if !ok {
		return false
	}
	if current.Auth != original.Auth {
		fail(w, http.StatusUnauthorized, "session_invalid")
		return false
	}
	backend, ok := h.backend.(roomReadAuthorizationBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "room_unavailable")
		return false
	}
	if err := backend.AuthorizeRoomRead(r.Context(), original.Auth, roomID); err != nil {
		writeRelayFailure(w, err)
		return false
	}
	return true
}

func (h *Handler) browserHistory(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession, roomID string) {
	var query relay.HistoryQuery
	if !decodeRequestLimit(w, r, &query, 8192) {
		fail(w, http.StatusBadRequest, "invalid_history_request")
		return
	}
	snapshot, err := h.backend.ReadonlySnapshot(r.Context(), session.Auth, roomID)
	if err != nil {
		writeRelayFailure(w, err)
		return
	}
	page, err := relay.ProjectEnterpriseHistory(relay.EnterpriseRoomSnapshot{RoomID: snapshot.RoomID, DeploymentID: snapshot.DeploymentID, OrgID: snapshot.OrgID, CreationUserID: snapshot.CreationUserID, HostMode: snapshot.HostMode, Events: snapshot.Events, Seq: snapshot.Seq, Revision: snapshot.Revision}, query)
	if err != nil {
		fail(w, http.StatusBadRequest, "history_unavailable")
		return
	}
	if !h.finalBrowserSession(w, r, session, roomID) {
		return
	}
	writeJSON(w, http.StatusOK, page)
}
