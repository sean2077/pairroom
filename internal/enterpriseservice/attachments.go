package enterpriseservice

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type attachmentBackend interface {
	AuthenticateBinding(context.Context, [32]byte, string) (sqlstore.BindingCredential, error)
	ReserveAttachment(context.Context, sqlstore.AttachmentReserveCommand) (sqlstore.Result, error)
	ResolveAttachmentReservation(context.Context, sqlstore.AttachmentReserveCommand) (sqlstore.Result, error)
	VerifyAttachment(context.Context, sqlstore.AttachmentVerifyCommand) (sqlstore.Result, error)
	ResolveAttachmentVerification(context.Context, sqlstore.AttachmentObjectCommand) (sqlstore.Result, error)
	CancelAttachment(context.Context, sqlstore.AttachmentObjectCommand) (sqlstore.Result, error)
	ResolveAttachmentCancellation(context.Context, sqlstore.AttachmentObjectCommand) (sqlstore.Result, error)
	ReadUploadReservation(context.Context, sqlstore.AuthContext, string, string) (sqlstore.AttachmentReservation, error)
	ReadAttachment(context.Context, sqlstore.AuthContext, string, string) (model.Attachment, error)
}

var _ attachmentBackend = (*sqlstore.Store)(nil)

// NewWithObjectStore enables the separately configured enterprise object root;
// an existing Local attachment directory is never adopted by this constructor.
func NewWithObjectStore(cfg Config, provider LoginProvider, transactions enterpriseauth.TransactionStore, backend Backend, objects *enterpriseobject.Store) (*Handler, error) {
	if objects == nil || objects.DeploymentID() != cfg.DeploymentID {
		return nil, ErrConfiguration
	}
	handler, err := New(cfg, provider, transactions, backend)
	if err != nil {
		return nil, err
	}
	if _, ok := backend.(attachmentBackend); !ok {
		return nil, ErrConfiguration
	}
	handler.objects = objects
	handler.objectSlots = make(chan struct{}, 8)
	return handler, nil
}

func browserAttachmentUpload(r *http.Request) bool {
	_, _, action, matched, valid := attachmentRoute(r.URL.Path, "/enterprise/api/v1/rooms/")
	return r.Method == http.MethodPut && matched && valid && action == "content"
}

func attachmentRoute(path, prefix string) (room, object, action string, matched, valid bool) {
	if !strings.HasPrefix(path, prefix) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 2 || parts[1] != "attachments" {
		return
	}
	matched = true
	if !validID(parts[0]) {
		return
	}
	room = parts[0]
	if len(parts) == 2 {
		return room, "", "reserve", true, true
	}
	if len(parts) == 3 && parts[2] == "resolve-reservation" {
		return room, "", "resolve-reservation", true, true
	}
	if len(parts) != 4 || !model.ValidEnterpriseAttachmentID(parts[2]) {
		return
	}
	switch parts[3] {
	case "content", "verify", "resolve-verification", "cancel", "resolve-cancellation":
		return room, parts[2], parts[3], true, true
	}
	return
}

func (h *Handler) attachmentNative(w http.ResponseWriter, r *http.Request) bool {
	room, object, action, matched, valid := attachmentRoute(r.URL.Path, "/enterprise/device/v1/rooms/")
	if !matched {
		return false
	}
	if !valid || !h.policy.acceptsTransport(r) || !deviceNativeHeaders(r, h.policy.deployment, true) {
		fail(w, http.StatusBadRequest, "invalid_attachment_request")
		return true
	}
	b, ok := h.backend.(attachmentBackend)
	if !ok || h.objects == nil {
		fail(w, http.StatusServiceUnavailable, "attachments_unavailable")
		return true
	}
	hash, ok := deviceBearerHash(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "binding_invalid")
		return true
	}
	c, err := b.AuthenticateBinding(r.Context(), hash, h.policy.origin)
	if err != nil {
		writeRelayFailure(w, err)
		return true
	}
	if c.RoomID != room || c.Auth.Deployment != h.policy.deployment || c.Auth.BindingGrantID == "" || c.Auth.DeviceGrantID != "" || c.Auth.SessionID != "" {
		fail(w, http.StatusForbidden, "binding_scope_mismatch")
		return true
	}
	h.attachmentOperation(w, r, b, c.Auth, room, object, action)
	return true
}

func (h *Handler) attachmentBrowser(w http.ResponseWriter, r *http.Request, session sqlstore.BrowserSession) bool {
	room, object, action, matched, valid := attachmentRoute(r.URL.Path, "/enterprise/api/v1/rooms/")
	if !matched {
		return false
	}
	if !valid {
		fail(w, http.StatusBadRequest, "invalid_attachment_request")
		return true
	}
	b, ok := h.backend.(attachmentBackend)
	if !ok || h.objects == nil {
		fail(w, http.StatusServiceUnavailable, "attachments_unavailable")
		return true
	}
	h.attachmentOperation(w, r, b, session.Auth, room, object, action)
	return true
}

func (h *Handler) attachmentOperation(w http.ResponseWriter, r *http.Request, b attachmentBackend, auth sqlstore.AuthContext, room, object, action string) {
	if action == "content" {
		// Each object is bounded at 5 MiB, and only eight simultaneous byte
		// operations may buffer/verify/stream. Waiting clients hold no SQL lock.
		if !h.acquireObjectCapacity(w) {
			return
		}
		defer func() { <-h.objectSlots }()
		switch r.Method {
		case http.MethodPut:
			h.uploadAttachment(w, r, b, auth, room, object)
		case http.MethodGet:
			h.downloadAttachment(w, r, b, auth, room, object)
		default:
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		}
		return
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if action == "reserve" || action == "resolve-reservation" {
		var request struct {
			ClientID string                  `json:"client_id"`
			Spec     sqlstore.AttachmentSpec `json:"spec"`
		}
		if !decodeRequest(w, r, &request) || !validID(request.ClientID) || request.Spec.Validate() != nil {
			fail(w, http.StatusBadRequest, "invalid_attachment_request")
			return
		}
		command := sqlstore.AttachmentReserveCommand{Auth: auth, RoomID: room, ClientID: request.ClientID, Spec: request.Spec}
		var result sqlstore.Result
		var err error
		if action == "reserve" {
			result, err = b.ReserveAttachment(r.Context(), command)
		} else {
			result, err = b.ResolveAttachmentReservation(r.Context(), command)
		}
		writeAttachmentOperation(w, result, err)
		return
	}
	var request struct {
		ClientID string `json:"client_id"`
	}
	if !decodeRequest(w, r, &request) || !validID(request.ClientID) {
		fail(w, http.StatusBadRequest, "invalid_attachment_request")
		return
	}
	command := sqlstore.AttachmentObjectCommand{Auth: auth, RoomID: room, ClientID: request.ClientID, ObjectID: object}
	var result sqlstore.Result
	var err error
	switch action {
	case "resolve-verification":
		result, err = b.ResolveAttachmentVerification(r.Context(), command)
	case "cancel":
		result, err = b.CancelAttachment(r.Context(), command)
	case "resolve-cancellation":
		result, err = b.ResolveAttachmentCancellation(r.Context(), command)
	case "verify":
		// Recover an accepted verification without consulting current bytes.
		// New verification alone requires a still-live reservation and a full
		// trusted ObjectStore read, never an HTTP assertion of verification.
		result, err = b.ResolveAttachmentVerification(r.Context(), command)
		if err == nil && result.Outcome == sqlstore.OutcomeNotCommitted {
			if _, err = b.ReadUploadReservation(r.Context(), auth, room, object); err != nil {
				writeRelayFailure(w, err)
				return
			}
			if !h.acquireObjectCapacity(w) {
				return
			}
			defer func() { <-h.objectSlots }()
			verified, inspectErr := h.objects.Inspect(r.Context(), object)
			if inspectErr != nil {
				if errors.Is(inspectErr, os.ErrNotExist) {
					// Recheck after the object read before certifying a retryable
					// missing upload; expired/revoked objects never get this class.
					if _, err = b.ReadUploadReservation(r.Context(), auth, room, object); err != nil {
						writeRelayFailure(w, err)
						return
					}
					writeOperation(w, sqlstore.Result{Outcome: sqlstore.OutcomeNotCommitted, Class: "object_missing"}, nil)
				} else {
					fail(w, http.StatusServiceUnavailable, "object_unavailable")
				}
				return
			}
			result, err = b.VerifyAttachment(r.Context(), sqlstore.AttachmentVerifyCommand{Auth: auth, RoomID: room, ClientID: request.ClientID, ObjectID: object, Verified: verified})
		}
	default:
		fail(w, http.StatusNotFound, "not_found")
		return
	}
	writeAttachmentOperation(w, result, err)
}

func (h *Handler) acquireObjectCapacity(w http.ResponseWriter) bool {
	select {
	case h.objectSlots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"outcome": "not_committed", "class": "object_capacity"})
		return false
	}
}

func writeAttachmentOperation(w http.ResponseWriter, result sqlstore.Result, err error) {
	if err != nil || !confirmed(result) && result.Outcome != sqlstore.OutcomeNotCommitted {
		writeOperation(w, result, err)
		return
	}
	if confirmed(result) && (result.Attachment == nil || result.Attachment.Version != 1) {
		fail(w, http.StatusServiceUnavailable, "attachment_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Outcome sqlstore.Outcome            `json:"outcome"`
		Receipt *sqlstore.AttachmentReceipt `json:"receipt,omitempty"`
	}{result.Outcome, result.Attachment})
}

func (h *Handler) uploadAttachment(w http.ResponseWriter, r *http.Request, b attachmentBackend, auth sqlstore.AuthContext, room, object string) {
	if r.ContentLength <= 0 || r.ContentLength > attachment.MaxImageBytes || len(r.TransferEncoding) > 0 || r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("Content-Encoding") != "" || r.Header.Get("Range") != "" {
		fail(w, http.StatusBadRequest, "invalid_upload_envelope")
		return
	}
	reservation, err := b.ReadUploadReservation(r.Context(), auth, room, object)
	if err != nil {
		writeRelayFailure(w, err)
		return
	}
	if reservation.Spec.Size != r.ContentLength {
		fail(w, http.StatusBadRequest, "attachment_length_mismatch")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, attachment.MaxImageBytes)
	verified, err := h.objects.PutImmutable(r.Context(), object, reservation.Spec, r.Body)
	if err != nil {
		if errors.Is(err, enterpriseobject.ErrInvalid) || errors.Is(err, enterpriseobject.ErrConflict) {
			fail(w, http.StatusBadRequest, "attachment_content_mismatch")
		} else {
			fail(w, http.StatusServiceUnavailable, "object_unavailable")
		}
		return
	}
	if _, err = b.ReadUploadReservation(r.Context(), auth, room, object); err != nil {
		writeRelayFailure(w, err)
		return
	}
	meta := verified.Metadata()
	writeJSON(w, http.StatusOK, struct {
		Status   string `json:"status"`
		ObjectID string `json:"object_id"`
		Size     int64  `json:"size"`
		SHA256   string `json:"sha256"`
	}{"uploaded", object, meta.Size, meta.SHA256})
}

func attachmentRange(value string, size int64) (start, end int64, partial bool, err error) {
	if value == "" {
		return 0, size, false, nil
	}
	if !strings.HasPrefix(value, "bytes=") || strings.ContainsAny(value, ", \t\r\n") {
		return 0, 0, false, errors.New("invalid attachment range")
	}
	left, right, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok || left == "" && right == "" {
		return 0, 0, false, errors.New("invalid attachment range")
	}
	if left == "" {
		count, e := strconv.ParseInt(right, 10, 64)
		if e != nil || count <= 0 {
			return 0, 0, false, errors.New("invalid attachment range")
		}
		if count > size {
			count = size
		}
		return size - count, size, true, nil
	}
	start, err = strconv.ParseInt(left, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("invalid attachment range")
	}
	end = size
	if right != "" {
		last, e := strconv.ParseInt(right, 10, 64)
		if e != nil || last < start {
			return 0, 0, false, errors.New("invalid attachment range")
		}
		if last < size-1 {
			end = last + 1
		}
	}
	return start, end, true, nil
}

func (h *Handler) downloadAttachment(w http.ResponseWriter, r *http.Request, b attachmentBackend, auth sqlstore.AuthContext, room, object string) {
	if len(r.Header.Values("Range")) > 1 || r.Header.Get("If-Range") != "" {
		fail(w, http.StatusBadRequest, "invalid_attachment_range")
		return
	}
	meta, err := b.ReadAttachment(r.Context(), auth, room, object)
	if err != nil {
		writeRelayFailure(w, err)
		return
	}
	data, err := h.objects.ReadVerified(r.Context(), meta)
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "object_unavailable")
		return
	}
	start, end, partial, err := attachmentRange(r.Header.Get("Range"), int64(len(data)))
	if err != nil {
		w.Header().Set("Content-Range", "bytes */"+strconv.Itoa(len(data)))
		fail(w, http.StatusRequestedRangeNotSatisfiable, "invalid_attachment_range")
		return
	}
	check, err := b.ReadAttachment(r.Context(), auth, room, object)
	if err != nil {
		writeRelayFailure(w, err)
		return
	}
	if check != meta {
		fail(w, http.StatusServiceUnavailable, "object_unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": meta.Name}))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start, 10))
	if partial {
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end-1, 10)+"/"+strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	for offset := start; offset < end; {
		// No database connection/lock spans the network write. Reauthorization
		// bounds late revocation exposure to one already released 32 KiB chunk.
		check, err = b.ReadAttachment(r.Context(), auth, room, object)
		if err != nil || check != meta {
			return
		}
		next := min(offset+(32<<10), end)
		if n, e := w.Write(data[offset:next]); e != nil || int64(n) != next-offset {
			return
		}
		offset = next
	}
}
