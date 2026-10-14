package enterpriseservice

import (
	"context"
	"net/http"

	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

// Queue controls are explicit browser commands. Live Owner authorization and
// the original receipt rules belong to the same SQL runner as Native Relay;
// a narrow Binding cannot invoke this capability through its token endpoint.
func (h *Handler) controlHumanQueue(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession, roomID string, resolve bool) {
	backend, ok := h.backend.(interface {
		CommitRelayOperation(context.Context, sqlstore.RelayCommand) (sqlstore.Result, error)
		ResolveRelayOperation(context.Context, sqlstore.RelayCommand) (sqlstore.Result, error)
	})
	if !ok {
		fail(w, http.StatusServiceUnavailable, "relay_unavailable")
		return
	}
	var request struct {
		ClientID         string                          `json:"client_id"`
		ExpectedRevision uint64                          `json:"expected_revision"`
		Intent           relay.EnterpriseOperationIntent `json:"intent"`
	}
	if !decodeRequest(w, r, &request) || !validID(request.ClientID) || request.ExpectedRevision == 0 || (request.Intent.Kind != relay.EnterpriseOperationRetry && request.Intent.Kind != relay.EnterpriseOperationCancel) || relay.ValidateEnterpriseOperationIntent(request.Intent) != nil {
		fail(w, http.StatusBadRequest, "invalid_queue_request")
		return
	}
	command := sqlstore.RelayCommand{Auth: session.Auth, RoomID: roomID, ClientID: request.ClientID, ExpectedRevision: request.ExpectedRevision, Intent: request.Intent}
	var result sqlstore.Result
	var err error
	if resolve {
		result, err = backend.ResolveRelayOperation(r.Context(), command)
	} else {
		result, err = backend.CommitRelayOperation(r.Context(), command)
	}
	// Browser results never carry claim receipts, envelope text or raw events.
	writeOperation(w, result, err)
}
