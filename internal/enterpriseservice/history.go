package enterpriseservice

import (
	"net/http"

	"github.com/sean2077/pairroom/internal/relay"
)

// relayHistory is a read-only POST so the closed HistoryQuery travels without
// query-string credentials or a second cursor implementation. Authentication
// is checked inside ReadonlySnapshot and again before any body is released.
func (d *DeviceHandler) relayHistory(w http.ResponseWriter, r *http.Request, roomID string) {
	if !validID(roomID) || !d.policy.acceptsTransport(r) || !deviceEnvelope(r, d.policy.deployment, true) {
		fail(w, http.StatusBadRequest, "invalid_history_request")
		return
	}
	backend, ok := d.backend.(relayBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "relay_unavailable")
		return
	}
	hash, ok := deviceBearerHash(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "binding_invalid")
		return
	}
	credential, err := backend.AuthenticateBinding(r.Context(), hash, d.policy.origin)
	if err != nil {
		writeRelayFailure(w, err)
		return
	}
	if credential.RoomID != roomID || credential.Auth.Deployment != d.policy.deployment || credential.Auth.BindingGrantID == "" || credential.Auth.DeviceGrantID != "" || credential.Auth.SessionID != "" {
		fail(w, http.StatusForbidden, "binding_scope_mismatch")
		return
	}
	var query relay.HistoryQuery
	if !decodeRequestLimit(w, r, &query, 8192) {
		fail(w, http.StatusBadRequest, "invalid_history_request")
		return
	}
	snapshot, err := backend.ReadonlySnapshot(r.Context(), credential.Auth, roomID)
	if err != nil {
		writeRelayFailure(w, err)
		return
	}
	page, err := relay.ProjectEnterpriseHistory(relay.EnterpriseRoomSnapshot{RoomID: snapshot.RoomID, DeploymentID: snapshot.DeploymentID, OrgID: snapshot.OrgID, CreationUserID: snapshot.CreationUserID, HostMode: snapshot.HostMode, Events: snapshot.Events, Seq: snapshot.Seq, Revision: snapshot.Revision}, query)
	if err != nil {
		fail(w, http.StatusBadRequest, "history_unavailable")
		return
	}
	current, err := backend.AuthenticateBinding(r.Context(), hash, d.policy.origin)
	if err != nil {
		writeRelayFailure(w, err)
		return
	}
	if current.Auth != credential.Auth || current.RoomID != credential.RoomID || current.BindingID != credential.BindingID || current.Generation != credential.Generation {
		fail(w, http.StatusUnauthorized, "binding_invalid")
		return
	}
	writeJSON(w, http.StatusOK, page)
}
