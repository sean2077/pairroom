package enterpriseservice

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type relayBackend interface {
	AuthenticateBinding(context.Context, [32]byte, string) (sqlstore.BindingCredential, error)
	ReadonlySnapshot(context.Context, sqlstore.AuthContext, string) (sqlstore.RoomSnapshot, error)
	CommitRelayOperation(context.Context, sqlstore.RelayCommand) (sqlstore.Result, error)
	ResolveRelayOperation(context.Context, sqlstore.RelayCommand) (sqlstore.Result, error)
}

var _ relayBackend = (*sqlstore.Store)(nil)

// relayTransport only accepts a narrow Binding token for the exact Room and
// generation. Device access, browser cookies and caller-supplied User/Actor
// fields cannot authorize this surface. Raw stored facts never leave it.
func (d *DeviceHandler) relayTransport(w http.ResponseWriter, r *http.Request) bool {
	const prefix = "/enterprise/device/v1/rooms/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) < 2 || parts[1] != "relay" {
		return false
	}
	if len(parts) == 3 && parts[2] == "context" {
		d.relayContext(w, r, parts[0])
		return true
	}
	if len(parts) == 3 && parts[2] == "history" {
		d.relayHistory(w, r, parts[0])
		return true
	}
	if len(parts) > 3 || !validID(parts[0]) || len(parts) == 3 && parts[2] != "resolve" || !d.policy.acceptsTransport(r) || !deviceEnvelope(r, d.policy.deployment, true) {
		fail(w, http.StatusBadRequest, "invalid_relay_request")
		return true
	}
	backend, ok := d.backend.(relayBackend)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "relay_unavailable")
		return true
	}
	hash, ok := deviceBearerHash(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "binding_invalid")
		return true
	}
	credential, err := backend.AuthenticateBinding(r.Context(), hash, d.policy.origin)
	if err != nil {
		writeRelayFailure(w, err)
		return true
	}
	var request struct {
		ClientID   string                          `json:"client_id"`
		BindingID  string                          `json:"binding_id"`
		Generation uint64                          `json:"generation"`
		Intent     relay.EnterpriseOperationIntent `json:"intent"`
	}
	if !decodeRequestLimit(w, r, &request, 2<<20) || !validID(request.ClientID) || relay.ValidateEnterpriseOperationIntent(request.Intent) != nil || credential.RoomID != parts[0] || credential.BindingID != request.BindingID || credential.Generation != request.Generation || credential.Auth.BindingGrantID == "" || credential.Auth.DeviceGrantID != "" || credential.Auth.SessionID != "" || credential.Auth.Deployment != d.policy.deployment {
		fail(w, http.StatusForbidden, "binding_scope_mismatch")
		return true
	}
	command := sqlstore.RelayCommand{Auth: credential.Auth, RoomID: parts[0], ClientID: request.ClientID, BindingID: request.BindingID, Generation: request.Generation, Intent: request.Intent}
	// Accepted recovery comes before reading current history or peer state.
	// An incomplete verification never authorizes another effect.
	result, err := backend.ResolveRelayOperation(r.Context(), command)
	if len(parts) == 2 && err == nil && result.Outcome == sqlstore.OutcomeNotCommitted {
		for attempt := 0; attempt < 2; attempt++ {
			snapshot, readErr := backend.ReadonlySnapshot(r.Context(), credential.Auth, command.RoomID)
			if readErr != nil {
				writeRelayFailure(w, readErr)
				return true
			}
			command.ExpectedRevision = snapshot.Revision
			result, err = backend.CommitRelayOperation(r.Context(), command)
			// This is a bounded rebuild after a proven pre-effect CAS refusal,
			// never a retry of Unknown, a receipt or a provider/native effect.
			if result.Outcome != sqlstore.OutcomeRefused || result.Class != sqlstore.ClassRevisionConflict {
				break
			}
		}
	}
	if err == nil && confirmed(result) && (result.Relay == nil || result.Relay.Kind != request.Intent.Kind) {
		fail(w, http.StatusServiceUnavailable, "relay_unavailable")
		return true
	}
	if err == nil && confirmed(result) && result.Relay != nil && (result.Relay.Claim != nil || result.Relay.StructuredClaim != nil) {
		// Claim is persisted before this final, content-free release check.
		// A lost fence or revoked grant releases no envelope and never requeues.
		current, checkErr := backend.AuthenticateBinding(r.Context(), hash, d.policy.origin)
		if checkErr != nil {
			writeRelayFailure(w, checkErr)
			return true
		}
		if current.Auth != credential.Auth || current.RoomID != credential.RoomID || current.BindingID != credential.BindingID || current.Generation != credential.Generation {
			fail(w, http.StatusUnauthorized, "binding_invalid")
			return true
		}
	}
	writeRelayOperation(w, result, err)
	return true
}

func (d *DeviceHandler) relayContext(w http.ResponseWriter, r *http.Request, roomID string) {
	if !validID(roomID) || !d.policy.acceptsTransport(r) || !deviceReadEnvelope(r, d.policy.deployment, true) {
		fail(w, http.StatusBadRequest, "invalid_relay_request")
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
	snapshot, err := backend.ReadonlySnapshot(r.Context(), credential.Auth, roomID)
	if err != nil {
		writeRelayFailure(w, err)
		return
	}
	view, err := relay.DescribeEnterpriseSnapshot(relay.EnterpriseRoomSnapshot{DeploymentID: snapshot.DeploymentID, OrgID: snapshot.OrgID, CreationUserID: snapshot.CreationUserID, RoomID: snapshot.RoomID, HostMode: snapshot.HostMode, Events: snapshot.Events, Seq: snapshot.Seq, Revision: snapshot.Revision}, model.ActorID(credential.Slot), credential.BindingID, credential.Generation, time.Now().UTC())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "relay_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func writeRelayFailure(w http.ResponseWriter, err error) {
	var kernel *sqlstore.KernelError
	if errors.As(err, &kernel) {
		writeOperation(w, sqlstore.Result{Outcome: kernel.Outcome, Class: kernel.Class}, err)
		return
	}
	fail(w, http.StatusServiceUnavailable, "relay_unavailable")
}

func writeRelayOperation(w http.ResponseWriter, result sqlstore.Result, err error) {
	if err != nil || !confirmed(result) {
		writeOperation(w, result, err)
		return
	}
	if result.Relay == nil || relay.ValidateEnterpriseOperationResult(*result.Relay) != nil {
		fail(w, http.StatusServiceUnavailable, "relay_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Outcome  sqlstore.Outcome                 `json:"outcome"`
		Seq      uint64                           `json:"seq"`
		Revision uint64                           `json:"revision"`
		Result   *relay.EnterpriseOperationResult `json:"result"`
	}{result.Outcome, result.Seq, result.Revision, result.Relay})
}

// humanSend uses the shared pure Relay producer inside the fenced SQL runner.
// Text and attachment publications keep the original human_send key/table.
func (h *Handler) humanSend(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession, roomID string) {
	store, ok := h.backend.(*sqlstore.Store)
	if !ok {
		fail(w, http.StatusServiceUnavailable, "relay_unavailable")
		return
	}
	var request struct {
		ClientID      string                        `json:"client_id"`
		To            model.ActorID                 `json:"to"`
		Text          string                        `json:"text"`
		AttachmentIDs model.EnterpriseAttachmentIDs `json:"attachment_ids,omitzero"`
		QuoteID       string                        `json:"quote_id,omitempty"`
		Review        review.Anchor                 `json:"review,omitzero"`
	}
	if !decodeRequestLimit(w, r, &request, 2<<20) || !validID(request.ClientID) || !request.To.ValidParticipant() || relay.ValidateEnterpriseOperationIntent(relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: request.To, Text: request.Text, AttachmentIDs: request.AttachmentIDs, QuoteID: request.QuoteID, Review: request.Review}) != nil {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	intent := sqlstore.HumanSendIntent{To: string(request.To), Text: request.Text, AttachmentIDs: request.AttachmentIDs, QuoteID: request.QuoteID, Review: request.Review}
	result, err := store.ResolveHumanSend(r.Context(), session.Auth, roomID, request.ClientID, intent)
	if err == nil && result.Outcome == sqlstore.OutcomeNotCommitted {
		for attempt := 0; attempt < 2; attempt++ {
			snapshot, readErr := store.ReadonlySnapshot(r.Context(), session.Auth, roomID)
			if readErr != nil {
				writeRelayFailure(w, readErr)
				return
			}
			result, err = store.CommitHumanMessage(r.Context(), sqlstore.HumanMessageCommand{Auth: session.Auth, RoomID: roomID, ClientID: request.ClientID, ExpectedRevision: snapshot.Revision, Intent: intent})
			if result.Outcome != sqlstore.OutcomeRefused || result.Class != sqlstore.ClassRevisionConflict {
				break
			}
		}
	}
	if err != nil || !confirmed(result) {
		writeOperation(w, result, err)
		return
	}
	message, err := relay.EnterpriseHumanSendMessage(result.Fact, session.Auth.UserID, request.ClientID, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: request.To, Text: request.Text, AttachmentIDs: request.AttachmentIDs, QuoteID: request.QuoteID, Review: request.Review})
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "publication_not_confirmed")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Message relay.Message `json:"message"`
	}{message})
}

func (h *Handler) resolveHumanMessage(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession, roomID string) {
	backend, ok := h.backend.(interface {
		ResolveHumanSend(context.Context, sqlstore.AuthContext, string, string, sqlstore.HumanSendIntent) (sqlstore.Result, error)
	})
	if !ok {
		fail(w, http.StatusServiceUnavailable, "relay_unavailable")
		return
	}
	var request struct {
		ClientID      string                        `json:"client_id"`
		To            model.ActorID                 `json:"to"`
		Text          string                        `json:"text"`
		AttachmentIDs model.EnterpriseAttachmentIDs `json:"attachment_ids,omitzero"`
		QuoteID       string                        `json:"quote_id,omitempty"`
		Review        review.Anchor                 `json:"review,omitzero"`
	}
	if !decodeRequestLimit(w, r, &request, 2<<20) || !validID(request.ClientID) || !request.To.ValidParticipant() {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	result, err := backend.ResolveHumanSend(r.Context(), session.Auth, roomID, request.ClientID, sqlstore.HumanSendIntent{To: string(request.To), Text: request.Text, AttachmentIDs: request.AttachmentIDs, QuoteID: request.QuoteID, Review: request.Review})
	writeOperation(w, result, err)
}
