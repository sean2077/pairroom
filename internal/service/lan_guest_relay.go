package service

import (
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type lanGuestOwnerRequest struct {
	Cursor        string        `json:"cursor,omitempty"`
	Limit         int           `json:"limit,omitempty"`
	Pending       bool          `json:"pending,omitempty"`
	Since         time.Time     `json:"since,omitempty"`
	Text          string        `json:"text,omitempty"`
	ID            string        `json:"id,omitempty"`
	To            model.ActorID `json:"to,omitempty"`
	AttachmentIDs []string      `json:"attachment_ids,omitempty"`
	QuoteID       string        `json:"quote_id,omitempty"`
}

func (s *ManagementServer) lanGuestOwnerAction(w http.ResponseWriter, r *http.Request) {
	client, err := s.lanGuests.room(r.Context(), r.PathValue("room"))
	if err != nil {
		if errors.Is(err, errLANStoreUnavailable) {
			writeLANBridgeError(w, err)
			return
		}
		writeManagementError(w, http.StatusNotFound, "joined Room not found")
		return
	}
	var req lanGuestOwnerRequest
	if decodeNativeJSON(w, r, &req) != nil {
		return
	}
	action := r.PathValue("action")
	var payload, result any
	switch action {
	case "summary":
		result = &relay.Summary{}
	case "receipt":
		payload = map[string]string{"id": req.ID}
		result = &struct {
			Accepted bool           `json:"accepted"`
			Message  *relay.Message `json:"message,omitempty"`
		}{}
	case "history":
		payload = relay.HistoryQuery{ID: req.ID, Cursor: req.Cursor, Limit: req.Limit, Pending: req.Pending, Since: req.Since}
		result = &relay.HistoryPage{}
	case "send":
		payload = relay.SendRequest{ID: req.ID, Text: req.Text, To: req.To, QuoteID: req.QuoteID, AttachmentIDs: req.AttachmentIDs}
		result = &relay.Message{}
	case "leave", "detach":
		result = &map[string]bool{}
	default:
		writeManagementError(w, http.StatusNotFound, "unsupported joined Room view operation")
		return
	}
	if err := client.Owner(r.Context(), action, payload, result); err != nil {
		writeLANBridgeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, result)
}

func (s *ManagementServer) lanGuestAttachment(w http.ResponseWriter, r *http.Request) {
	client, err := s.lanGuests.room(r.Context(), r.PathValue("room"))
	if err != nil {
		if errors.Is(err, errLANStoreUnavailable) {
			writeLANBridgeError(w, err)
			return
		}
		writeManagementError(w, http.StatusNotFound, "joined Room not found")
		return
	}
	id := r.PathValue("attachment")
	if !lanshare.ValidID(id) {
		writeManagementError(w, http.StatusBadRequest, "invalid attachment ID")
		return
	}
	// Download reauthorizes metadata at the host even on a verified cache hit.
	metadata, path, err := client.Download(r.Context(), id)
	if err != nil {
		writeLANBridgeError(w, err)
		return
	}
	file, err := os.Open(path)
	if err != nil {
		writeLANBridgeError(w, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, metadata.Name, metadata.CreatedAt, file)
}

func writeLANBridgeError(w http.ResponseWriter, err error) {
	var failure *lanclient.Error
	if errors.As(err, &failure) {
		status := failure.Status
		if status < 400 || status >= 600 {
			status = http.StatusBadGateway
		}
		if status == http.StatusUnauthorized {
			// The hosting Room rejected the member credential; this browser
			// session belongs to the Service and stays valid.
			status = http.StatusForbidden
		}
		writeManagementJSON(w, status, map[string]string{"error": failure.Message, "code": failure.Code})
		return
	}
	if errors.Is(err, errLANStoreUnavailable) {
		writeManagementError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	if errors.Is(err, relay.ErrAuth) || errors.Is(err, lanclient.ErrInactive) {
		// A remote membership refusal is not a Service-session expiry: the
		// dashboard treats any 401 on these paths as an expired browser session
		// and demands the Service token again.
		writeManagementJSON(w, http.StatusForbidden, map[string]string{"error": "the hosting Room refused this operation: the membership is revoked, inactive or no longer authorized there", "code": "lan_membership_denied"})
		return
	}
	writeManagementError(w, http.StatusBadGateway, "Joined Room operation is unavailable; inspect its original client state before retrying")
}
