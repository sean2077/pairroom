package lanclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
)

type relayRequest struct {
	Review         *review.Anchor `json:"review,omitempty"`
	Cursor         string         `json:"cursor,omitempty"`
	Limit          int            `json:"limit,omitempty"`
	Pending        bool           `json:"pending,omitempty"`
	Since          time.Time      `json:"since,omitempty"`
	SessionID      string         `json:"session_id,omitempty"`
	TranscriptPath string         `json:"transcript_path,omitempty"`
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
}

type publicationReceipt struct {
	Accepted    bool              `json:"accepted"`
	Publication relay.Publication `json:"publication"`
}

func decodePayload(payload, result any) error {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > 2<<20 {
		return errors.New("invalid LAN operation payload")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(result) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid LAN operation payload")
	}
	return nil
}

func assignResult(result, value any) error {
	if result == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

// Relay validates the workspace's private local capability for every action.
// It rebuilds each remote payload from a whitelist; SessionID, transcript and
// local relay credentials are never copied to the host. "ack" means the
// authenticated caller has already written the returned envelope to stdout.
func (c *Client) Relay(ctx context.Context, auth relay.Auth, action string, payload, result any) error {
	r, err := c.authenticated(ctx, auth, action == "unbind")
	if err != nil {
		return err
	}
	var req relayRequest
	if err := decodePayload(payload, &req); err != nil {
		return err
	}
	var remotePayload, value any
	switch action {
	case "inspect", "confirm":
		if action == "confirm" && (req.SessionID != auth.SessionID || len(req.TranscriptPath) > 4096 || req.TranscriptPath != "" && !filepath.IsAbs(req.TranscriptPath)) {
			return relay.ErrAuth
		}
		if err := c.call(ctx, r, action, nil, nil); err != nil {
			return safeError(err)
		}
		if action == "confirm" {
			r, err = c.withRecord(ctx, func(next *record) error {
				if err := authenticate(*next, auth, false); err != nil {
					return err
				}
				next.TranscriptPath, next.LastActivity = req.TranscriptPath, time.Now().UTC()
				return nil
			})
			if err != nil {
				return err
			}
		}
		return assignResult(result, binding(r))
	case "wait":
		value, err = c.collect(ctx, req, auth)
		if err != nil {
			return err
		}
		return assignResult(result, value)
	case "send":
		remotePayload = relay.SendRequest{ID: req.ID, Text: req.Text, To: req.To, AttachmentIDs: req.AttachmentIDs, QuoteID: req.QuoteID, Review: req.Review}
		value = &relay.Message{}
	case "report":
		remotePayload = struct {
			ReportSeq uint64 `json:"report_seq"`
			Text      string `json:"text"`
		}{req.ReportSeq, req.Text}
		value = &relay.Publication{}
	case "publication":
		remotePayload = map[string]uint64{"report_seq": req.ReportSeq}
		value = &publicationReceipt{}
	case "ack":
		if err := c.acknowledgeDelivery(ctx, auth, req.ID, req.Receipt, auth.Generation, true); err != nil {
			return err
		}
		return assignResult(result, map[string]bool{"handed_off": true})
	case "status":
		value = &relay.TailSnapshot{}
	case "summary":
		value = &relay.Summary{}
	case "history":
		remotePayload = relay.HistoryQuery{ID: req.ID, Cursor: req.Cursor, Limit: req.Limit, Pending: req.Pending, Since: req.Since}
		value = &relay.HistoryPage{}
	case "doctor":
		value, err = c.doctor(ctx, r)
		if err != nil {
			return safeError(err)
		}
		return assignResult(result, value)
	case "peer":
		value = &relay.Binding{}
	case "failure":
		remotePayload, value = map[string]string{"error": req.Error}, &map[string]bool{}
	case "park":
		remotePayload, value = map[string]bool{"enabled": req.Enabled}, &map[string]bool{}
	case "unbind":
		if r.Status == "detached" {
			return ErrInactive
		}
		value = &map[string]bool{}
	default:
		return errors.New("unsupported LAN relay operation")
	}
	if err := c.call(ctx, r, action, remotePayload, value); err != nil {
		if action != "unbind" || !membershipDenied(err) {
			return safeError(err)
		}
		value = &map[string]bool{"unbound": true}
	}
	if action == "park" {
		_, err = c.withRecord(ctx, func(next *record) error {
			if err := authenticate(*next, auth, false); err != nil {
				return err
			}
			next.ParkEnabled = req.Enabled
			return nil
		})
		if err != nil {
			return err
		}
	} else if action == "unbind" {
		if err := c.retire(ctx, r, "left"); err != nil {
			return err
		}
	}
	switch v := value.(type) {
	case *relay.Publication:
		if r.Room == nil || v.BindID != r.Room.BindID || v.Generation != r.Room.Generation {
			return errors.New("LAN publication identity mismatch")
		}
		v.BindID = r.BindID
	case *publicationReceipt:
		if v.Accepted {
			if r.Room == nil || v.Publication.BindID != r.Room.BindID || v.Publication.Generation != r.Room.Generation {
				return errors.New("LAN publication receipt identity mismatch")
			}
			v.Publication.BindID = r.BindID
		}
	case *relay.TailSnapshot:
		v.RoomID = r.ID
		for slot, b := range v.Bindings {
			b.SessionID, b.TranscriptPath = "", ""
			v.Bindings[slot] = b
		}
	case *relay.Summary:
		v.RoomID = r.ID
	case *relay.Binding:
		v.SessionID, v.TranscriptPath = "", ""
	}
	return assignResult(result, value)
}

func (c *Client) ownerRecord(ctx context.Context, leaving bool) (record, error) {
	return c.withRecord(ctx, func(r *record) error {
		// Leaving may retire a record that never held a Room: a pending request
		// was never admitted, so it has no host membership to unbind.
		if r.Status == "detached" || r.Room == nil && !leaving || r.Status != "accepted" && !leaving {
			return relay.ErrAuth
		}
		if leaving {
			return nil
		}
		return c.store.identities.Check(ctx, reservation(*r))
	})
}

// Owner is restricted to the same human-facing Room view available through
// optional local Management authentication. It cannot configure a peer,
// operate a process, grant a tool approval or name an arbitrary file path.
func (c *Client) Owner(ctx context.Context, action string, payload, result any) error {
	if action == "detach" {
		var request struct{}
		if err := decodePayload(payload, &request); err != nil {
			return err
		}
		if err := c.detach(ctx, nil); err != nil {
			return err
		}
		return assignResult(result, map[string]bool{"detached": true})
	}
	r, err := c.ownerRecord(ctx, action == "leave")
	if err != nil {
		return err
	}
	var req relayRequest
	if err := decodePayload(payload, &req); err != nil {
		return err
	}
	var remotePayload, value any
	switch action {
	case "summary":
		value = &relay.Summary{}
	case "history":
		remotePayload = relay.HistoryQuery{ID: req.ID, Cursor: req.Cursor, Limit: req.Limit, Pending: req.Pending, Since: req.Since}
		value = &relay.HistoryPage{}
	case "receipt":
		action, remotePayload = "user-receipt", map[string]string{"id": req.ID}
		value = &struct {
			Accepted bool           `json:"accepted"`
			Message  *relay.Message `json:"message,omitempty"`
		}{}
	case "send":
		action = "user-send"
		remotePayload = relay.SendRequest{ID: req.ID, Text: req.Text, To: req.To, QuoteID: req.QuoteID, AttachmentIDs: req.AttachmentIDs, Review: req.Review}
		value = &relay.Message{}
	case "leave":
		if r.Room == nil {
			// A pending or denied request holds no host membership: leaving it
			// retires the local record exactly like an explicit local detach,
			// instead of reporting a remote refusal that never happened.
			if err := c.detach(ctx, nil); err != nil {
				return err
			}
			return assignResult(result, map[string]bool{"unbound": true})
		}
		action, value = "unbind", &map[string]bool{}
	default:
		return errors.New("unsupported joined Room view operation")
	}
	if err := c.call(ctx, r, action, remotePayload, value); err != nil {
		if action != "unbind" || !membershipDenied(err) {
			return safeError(err)
		}
		value = &map[string]bool{"unbound": true}
	}
	if action == "unbind" {
		if err := c.retire(ctx, r, "left"); err != nil {
			return err
		}
	}
	return assignResult(result, value)
}

func (c *Client) retire(ctx context.Context, original record, status string) error {
	_, err := c.withRecord(ctx, func(next *record) error {
		if next.BindID != original.BindID || next.RequestID != original.RequestID || next.Room == nil || original.Room == nil || next.Room.Generation != original.Room.Generation {
			return relay.ErrAuth
		}
		if next.Status == "detached" {
			return ErrInactive
		}
		next.Status = status
		if err := privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), *next); err != nil {
			return err
		}
		return c.store.identities.Release(ctx, reservation(*next))
	})
	return err
}

func (c *Client) doctor(ctx context.Context, r record) (any, error) {
	var remote struct {
		Relay          relay.Summary `json:"relay"`
		Protocol       string        `json:"protocol"`
		ServiceVersion string        `json:"service_version"`
	}
	if err := c.call(ctx, r, "doctor", nil, &remote); err != nil {
		return nil, safeError(err)
	}
	remote.Relay.RoomID = r.ID
	if r.Room == nil {
		return nil, relay.ErrAuth
	}
	slot := r.Room.Slot
	state := remote.Relay.Bindings[slot]
	reason, next := "no_pending_input", "none"
	if inbox := remote.Relay.Inboxes[slot]; inbox.Unknown > 0 {
		reason, next = "uncertain_delivery", "inspect_pending_evidence"
	} else if state.CollectorActive {
		reason, next = "collector_registered", "wait_for_collector_result"
	} else if inbox.Delivering > 0 {
		reason, next = "delivery_in_flight", "wait_for_receipt"
	} else if inbox.Queued > 0 {
		reason, next = "queued_input", "collect_in_native_session"
	}
	c.mu.Lock()
	connected, seen := c.connected, c.lastSeen
	c.mu.Unlock()
	return map[string]any{"schema": 1, "scope": "native_lan_direct", "service_version": remote.ServiceVersion, "protocol": remote.Protocol, "generated_at": time.Now().UTC(), "relay": remote.Relay, "participants": map[model.ActorID]any{slot: map[string]any{"runtime": r.Runtime, "capability": "tracked_wait_only", "hook_approval": "unknown", "model_acceptance": "unknown", "collector_active": state.CollectorActive, "reason": reason, "next_action": next}}, "connection": map[string]any{"connected": connected, "last_seen": seen}, "notice": "Direct LAN collection uses foreground commands and approved native hooks. Idle wake requires an optional local observer. This read-only check performs no claim, acknowledgement or native wake."}, nil
}
