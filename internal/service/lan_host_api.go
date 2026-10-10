package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
	"github.com/sean2077/pairroom/internal/version"
)

type lanMemberRequest struct {
	Review         *review.Anchor `json:"review,omitempty"`
	Cursor         string         `json:"cursor,omitempty"`
	Limit          int            `json:"limit,omitempty"`
	Pending        bool           `json:"pending,omitempty"`
	Since          time.Time      `json:"since,omitempty"`
	ReportSeq      uint64         `json:"report_seq,omitempty"`
	Text           string         `json:"text,omitempty"`
	ID             string         `json:"id,omitempty"`
	Receipt        string         `json:"receipt,omitempty"`
	To             model.ActorID  `json:"to,omitempty"`
	AttachmentIDs  []string       `json:"attachment_ids,omitempty"`
	QuoteID        string         `json:"quote_id,omitempty"`
	Park           bool           `json:"park,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds,omitempty"`
	Enabled        bool           `json:"enabled,omitempty"`
	Error          string         `json:"error,omitempty"`
	Digest         string         `json:"digest,omitempty"`
	Generation     uint64         `json:"generation,omitempty"`
	Outcome        string         `json:"outcome,omitempty"`
	Reason         string         `json:"reason,omitempty"`
}

func publicLANBinding(b relay.Binding) relay.Binding {
	b.SessionID = ""
	b.TranscriptPath = ""
	return b
}
func (h *lanHostServer) serveMember(w http.ResponseWriter, r *http.Request, n *nativeHostRuntime, a relay.Auth, action string) {
	if action == "upload" {
		h.upload(w, r, n, a)
		return
	}
	var req lanMemberRequest
	if decodeNativeJSON(w, r, &req) != nil {
		return
	}
	send := relay.SendRequest{Review: req.Review, ID: req.ID, Text: req.Text, To: req.To, AttachmentIDs: req.AttachmentIDs, QuoteID: req.QuoteID}
	switch action {
	case "inspect":
		b, err := n.engine.Inspect(a)
		nativeResult(w, publicLANBinding(b), err)
	case "confirm":
		b, err := n.engine.ConfirmLAN(a)
		nativeResult(w, publicLANBinding(b), err)
	case "room":
		b, err := n.engine.Inspect(a)
		if err != nil {
			nativeResult(w, nil, err)
			return
		}
		nativeResult(w, h.owner.lanRoomInfo(n, b), nil)
	case "report":
		p, err := n.engine.Report(a, req.ReportSeq, req.Text)
		if err == nil && p.Message != nil {
			n.scheduleWake(p.Message.ID)
		}
		nativeResult(w, p, err)
	case "publication":
		p, accepted, err := n.engine.Publication(a, req.ReportSeq)
		nativeResult(w, map[string]any{"accepted": accepted, "publication": p}, err)
	case "send":
		m, err := n.engine.Send(a, send)
		if err == nil {
			n.scheduleWake(m.ID)
		}
		nativeResult(w, m, err)
	case "user-send":
		m, err := n.engine.SendLANUser(a, send)
		if err == nil {
			n.scheduleWake(m.ID)
		}
		nativeResult(w, m, err)
	case "user-receipt":
		m, err := n.engine.LANUserReceipt(a, req.ID)
		nativeResult(w, map[string]any{"accepted": m != nil, "message": m}, err)
	case "head":
		seconds := req.TimeoutSeconds
		if seconds <= 0 {
			seconds = int(relay.DefaultPark / time.Second)
		}
		if seconds > int(relay.MaxPark/time.Second) {
			writeManagementError(w, 400, "head wait exceeds maximum")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(seconds)*time.Second)
		defer cancel()
		head, err := n.engine.PrepareHead(ctx, a, req.Park)
		if errors.Is(err, context.DeadlineExceeded) {
			err = nil
		}
		nativeResult(w, map[string]any{"head": head}, err)
	case "claim":
		if req.Generation != a.Generation {
			nativeResult(w, nil, relay.ErrAuth)
			return
		}
		claim, err := n.engine.ClaimPrepared(r.Context(), a, req.ID, req.Digest, req.Park)
		nativeResult(w, map[string]any{"claim": claim}, err)
	case "ack":
		nativeResult(w, map[string]bool{"handed_off": true}, n.engine.Ack(a, req.ID, req.Receipt))
	case "status":
		snapshot, err := n.engine.AuthSnapshotTail(a)
		nativeResult(w, snapshot, err)
	case "doctor":
		summary, err := n.engine.AuthSummary(a)
		nativeResult(w, map[string]any{"relay": summary, "protocol": protocol.NativeVersion, "service_version": version.Current}, err)
	case "summary":
		summary, err := n.engine.AuthSummary(a)
		nativeResult(w, summary, err)
	case "history":
		page, err := n.engine.AuthHistory(a, relay.HistoryQuery{ID: req.ID, Cursor: req.Cursor, Limit: req.Limit, Pending: req.Pending, Since: req.Since})
		nativeResult(w, page, err)
	case "peer":
		b, err := n.engine.Peer(a)
		b.Runtime = n.engine.Runtimes()[model.OtherParticipant(a.Slot)]
		nativeResult(w, publicLANBinding(b), err)
	case "failure":
		nativeResult(w, map[string]bool{"recorded": true}, n.engine.Failure(a, req.Error))
	case "park":
		nativeResult(w, map[string]bool{"enabled": req.Enabled}, n.engine.ParkAs(a, req.Enabled))
	case "leave", "unbind":
		nativeResult(w, map[string]bool{"unbound": true}, n.engine.UnbindAs(a))
	case "attachment":
		value, err := n.engine.SharedAttachment(a, req.ID)
		nativeResult(w, value, err)
	case "download":
		h.download(w, r, n, a, req.ID)
	case "wake-candidate":
		candidate, err := n.engine.LANWakeCandidate(a, req.ID)
		nativeResult(w, map[string]any{"candidate": candidate}, err)
	case "wake-reserve":
		err := n.engine.ReserveLANWake(a, req.ID)
		if errors.Is(err, relay.ErrWakeReserved) {
			writeManagementJSON(w, 409, map[string]string{"error": err.Error(), "code": relay.WakeReservedCode})
			return
		}
		if errors.Is(err, relay.ErrWakeIneligible) {
			writeManagementJSON(w, 409, map[string]string{"error": err.Error(), "code": relay.WakeIneligibleCode})
			return
		}
		nativeResult(w, map[string]bool{"reserved": true}, err)
	case "wake-record":
		nativeResult(w, map[string]bool{"recorded": true}, n.engine.RecordLANWake(a, req.ID, req.Outcome, req.Reason))
	default:
		writeManagementError(w, 404, "unknown LAN member operation")
	}
}
func (h *lanHostServer) upload(w http.ResponseWriter, r *http.Request, n *nativeHostRuntime, a relay.Auth) {
	release, ok := h.acquireTransfer(n.room.ID)
	if !ok {
		writeManagementError(w, 429, "LAN attachment transfer capacity reached")
		return
	}
	defer release()
	kind := r.Header.Get("X-PairRoom-Attachment-Kind")
	if kind != "" && kind != "image" && kind != "file" {
		writeManagementError(w, 400, "unsupported attachment kind")
		return
	}
	if kind == "" {
		kind = "image"
	}
	limit := int64(attachment.MaxImageBytes)
	if kind == "file" {
		limit = attachment.MaxEvidenceBytes
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit+(64<<10))
	transfer, reset, err := beginLANHTTPTransfer(w, r, n, a)
	if err != nil {
		nativeResult(w, nil, err)
		return
	}
	defer reset()
	defer transfer.Close()
	parts, err := r.MultipartReader()
	if err != nil {
		writeManagementError(w, 400, "multipart attachment required")
		return
	}
	part, err := parts.NextPart()
	if err != nil {
		writeManagementError(w, 400, "attachment part required")
		return
	}
	defer part.Close()
	staged, err := n.media.StageShared(kind, part.FileName(), part, "lan")
	if err != nil {
		nativeResult(w, nil, lanAttachmentError(err, kind))
		return
	}
	defer staged.Close()
	if _, err = parts.NextPart(); !errors.Is(err, io.EOF) {
		writeManagementError(w, 400, "upload exactly one attachment")
		return
	}
	var value model.Attachment
	// The transfer barrier joins this disk commit before revocation, without
	// holding the Room lock while the attachment store writes and syncs.
	err = transfer.Effect(func() error {
		var saveErr error
		value, saveErr = staged.Commit()
		return saveErr
	})
	nativeResult(w, value, lanAttachmentError(err, kind))
}

// Storage errors may contain the host's private data root. Expose only fixed
// actionable quota/authentication errors or validation guidance on LAN.
func lanAttachmentError(err error, kind string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, relay.ErrAuth):
		return relay.ErrAuth
	case errors.Is(err, relay.ErrClosed):
		return relay.ErrClosed
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, attachment.ErrSharedQuota):
		return attachment.ErrSharedQuota
	case errors.Is(err, attachment.ErrTemporaryQuota):
		return attachment.ErrTemporaryQuota
	case kind == "file":
		return errors.New("cannot store attachment: use a UTF-8 text file without NUL bytes, 1 byte–5 MiB, with a valid filename; ask the host owner to check attachment storage if it still fails")
	default:
		return errors.New("cannot store attachment: use a PNG, JPEG, GIF or WebP image, 1 byte–5 MiB, at most 8000 pixels per side and 64 million pixels; ask the host owner to check attachment storage if it still fails")
	}
}

func (h *lanHostServer) download(w http.ResponseWriter, r *http.Request, n *nativeHostRuntime, a relay.Auth, id string) {
	release, ok := h.acquireTransfer(n.room.ID)
	if !ok {
		writeManagementError(w, 429, "LAN attachment transfer capacity reached")
		return
	}
	defer release()
	accepted, err := n.engine.SharedAttachment(a, id)
	if err != nil {
		nativeResult(w, nil, err)
		return
	}
	value, _, err := n.media.Resolve(id)
	if err != nil || value.SHA256 != accepted.SHA256 {
		writeManagementError(w, 409, "shared attachment integrity check failed")
		return
	}
	_, f, err := n.media.OpenFile(id)
	if err != nil {
		writeManagementError(w, 409, "shared attachment integrity check failed")
		return
	}
	defer f.Close()
	transfer, reset, err := beginLANHTTPTransfer(w, r, n, a)
	if err != nil {
		nativeResult(w, nil, err)
		return
	}
	defer reset()
	defer transfer.Close()
	w.Header().Set("Content-Type", value.MediaType)
	w.Header().Set("Content-Length", fmt.Sprint(value.Size))
	w.Header().Set("Content-Disposition", "attachment")
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		if transfer.Context().Err() != nil {
			return
		}
		count, readErr := f.Read(buf)
		if count > 0 {
			// Revalidate each network effect without retaining the Room lock.
			// Revocation interrupts and joins this independent transfer before
			// its fact commits, so no subsequent byte crosses that boundary.
			err = transfer.Effect(func() error {
				_ = rc.SetWriteDeadline(time.Now().Add(2 * time.Second))
				_, err := w.Write(buf[:count])
				if err != nil {
					return err
				}
				// Flush inside the transfer barrier too: buffered final bytes
				// must not be emitted by net/http after Close releases it.
				return rc.Flush()
			})
			if err != nil {
				return
			}
		}
		if readErr != nil {
			return
		}
	}
}
