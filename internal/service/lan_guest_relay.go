package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/prompt"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
)

type lanGuestPublicationReceipt struct {
	Accepted    bool              `json:"accepted"`
	Publication relay.Publication `json:"publication"`
}

type lanGuestRelayRequest struct {
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

// Native clients always call their local numeric-loopback endpoint. Their
// LOCAL relay credential is checked here and never copied into LAN headers.
func (s *ManagementServer) serveLANJoinedRelay(w http.ResponseWriter, r *http.Request, auth relay.Auth) bool {
	guest := s.lanGuests.get(r.PathValue("room"))
	if guest == nil {
		return false
	}
	if guest.authenticateLocal(auth, r.PathValue("action") == "unbind") != nil {
		nativeResult(w, nil, relay.ErrAuth)
		return true
	}
	s.observeCLIBuild(r)
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.streams, cancel)
	defer stop()
	defer cancel()
	if r.PathValue("action") == "upload" {
		guest.upload(w, r.WithContext(ctx))
		return true
	}
	var req lanGuestRelayRequest
	if decodeNativeJSON(w, r, &req) != nil {
		return true
	}
	if err := guest.relayAction(ctx, w, r.PathValue("action"), req, auth); err != nil {
		writeLANBridgeError(w, err)
	}
	return true
}

func (guest *lanGuest) relayAction(ctx context.Context, w http.ResponseWriter, action string, req lanGuestRelayRequest, auth relay.Auth) error {
	var result any
	var payload any
	switch action {
	case "inspect", "confirm":
		// The peer receives neither official native session metadata nor local
		// transcript paths. Session confirmation remains a local hook boundary.
		if action == "confirm" && (req.SessionID != auth.SessionID || len(req.TranscriptPath) > 4096 || req.TranscriptPath != "" && !filepath.IsAbs(req.TranscriptPath)) {
			return relay.ErrAuth
		}
		if err := guest.call(ctx, action, nil, nil); err != nil {
			return err
		}
		guest.mu.Lock()
		if action == "confirm" {
			next := guest.record
			next.TranscriptPath = req.TranscriptPath
			next.LastActivity = time.Now().UTC()
			if err := privatefile.WriteJSON(filepath.Join(guest.dir, "guest.json"), next); err != nil {
				guest.mu.Unlock()
				return err
			}
			guest.record = next
		}
		result = guest.bindingLocked()
		guest.mu.Unlock()
		writeManagementJSON(w, http.StatusOK, result)
		return nil
	case "wait":
		return guest.collect(ctx, w, req, auth)
	case "send":
		payload = relay.SendRequest{ID: req.ID, Text: req.Text, To: req.To, AttachmentIDs: req.AttachmentIDs, QuoteID: req.QuoteID, Review: req.Review}
		result = &relay.Message{}
	case "report":
		payload = struct {
			ReportSeq uint64 `json:"report_seq"`
			Text      string `json:"text"`
		}{req.ReportSeq, req.Text}
		result = &relay.Publication{}
	case "publication":
		payload = map[string]uint64{"report_seq": req.ReportSeq}
		result = &lanGuestPublicationReceipt{}
	case "ack":
		if err := guest.acknowledgeDelivery(ctx, req.ID, req.Receipt, auth.Generation, true); err != nil {
			return err
		}
		writeManagementJSON(w, http.StatusOK, map[string]bool{"handed_off": true})
		return nil
	case "status":
		result = &relay.Snapshot{}
	case "summary":
		result = &relay.Summary{}
	case "history":
		payload = relay.HistoryQuery{ID: req.ID, Cursor: req.Cursor, Limit: req.Limit, Pending: req.Pending, Since: req.Since}
		result = &relay.HistoryPage{}
	case "doctor":
		return guest.doctor(ctx, w)
	case "peer":
		result = &relay.Binding{}
	case "failure":
		payload = map[string]string{"error": req.Error}
		result = &map[string]bool{}
	case "park":
		payload = map[string]bool{"enabled": req.Enabled}
		result = &map[string]bool{}
	case "unbind":
		result = &map[string]bool{}
	default:
		writeManagementError(w, http.StatusNotFound, "unsupported LAN relay operation")
		return nil
	}
	if err := guest.call(ctx, action, payload, result); err != nil {
		if action != "unbind" || !lanMembershipDenied(err) {
			return err
		}
		result = &map[string]bool{"unbound": true}
	}
	guest.mu.Lock()
	localID := guest.record.ID
	localBindID := guest.record.BindID
	remoteRoom := guest.record.Room
	if action == "park" || action == "unbind" {
		next := guest.record
		if action == "park" {
			next.ParkEnabled = req.Enabled
		} else {
			next.Status = "left"
		}
		if err := privatefile.WriteJSON(filepath.Join(guest.dir, "guest.json"), next); err != nil {
			guest.mu.Unlock()
			return err
		}
		guest.record = next
	}
	guest.mu.Unlock()
	switch v := result.(type) {
	case *relay.Publication:
		if remoteRoom == nil || v.BindID != remoteRoom.BindID || v.Generation != remoteRoom.Generation {
			return errors.New("LAN publication identity mismatch")
		}
		v.BindID = localBindID
	case *lanGuestPublicationReceipt:
		if v.Accepted {
			if remoteRoom == nil || v.Publication.BindID != remoteRoom.BindID || v.Publication.Generation != remoteRoom.Generation {
				return errors.New("LAN publication receipt identity mismatch")
			}
			v.Publication.BindID = localBindID
		}
	case *relay.Snapshot:
		v.RoomID = localID
		for slot, b := range v.Bindings {
			b.SessionID, b.TranscriptPath = "", ""
			v.Bindings[slot] = b
		}
	case *relay.Summary:
		v.RoomID = localID
	case *relay.Binding:
		v.SessionID, v.TranscriptPath = "", ""
	}
	writeManagementJSON(w, http.StatusOK, result)
	return nil
}

func (guest *lanGuest) collect(ctx context.Context, w http.ResponseWriter, req lanGuestRelayRequest, auth relay.Auth) error {
	seconds := req.TimeoutSeconds
	if seconds <= 0 {
		seconds = int(relay.DefaultPark / time.Second)
	}
	if seconds > int(relay.MaxPark/time.Second) {
		writeManagementError(w, http.StatusBadRequest, "wait timeout exceeds Native park limit")
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	for {
		var head lanshare.HeadResponse
		if err := guest.call(ctx, "head", lanshare.HeadRequest{Park: req.Park, TimeoutSeconds: seconds}, &head); err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				writeManagementJSON(w, http.StatusOK, map[string]any{"claim": nil})
				return nil
			}
			return err
		}
		if head.Head == nil {
			writeManagementJSON(w, http.StatusOK, map[string]any{"claim": nil})
			return nil
		}
		guest.mu.Lock()
		runtime := guest.record.Runtime
		guest.mu.Unlock()
		if req.Park && runtime == model.RuntimeGrok {
			writeManagementJSON(w, http.StatusOK, map[string]any{"claim": nil, "foreground_required": true})
			return nil
		}
		m := head.Head.Message
		if m.To != auth.Slot || m.TargetGeneration != auth.Generation || m.State != "queued" || !lanshare.ValidID(m.ID) || !lanshare.ValidFingerprint(head.Head.Digest) {
			return errors.New("LAN head identity mismatch")
		}
		// All bytes are present and verified before the ten-second delivery
		// lease begins. A slow/disconnected transfer has no consumption effect.
		envelope, err := guest.prepareEnvelope(ctx, m)
		if err != nil {
			return err
		}
		if err := guest.authenticate(auth); err != nil {
			return err
		}
		if err := guest.deliveryCapacity(); err != nil {
			return err
		}
		var response lanshare.ClaimResponse
		if err := guest.call(ctx, "claim", lanshare.ClaimRequest{ID: m.ID, Digest: head.Head.Digest, Generation: auth.Generation, Park: req.Park}, &response); err != nil {
			return err
		}
		if response.Claim == nil {
			if ctx.Err() != nil {
				writeManagementJSON(w, http.StatusOK, map[string]any{"claim": nil})
				return nil
			}
			continue // head changed; never claim the unprepared successor
		}
		claim := response.Claim
		if claim.ID != m.ID || claim.Receipt == "" || claim.Message.ID != m.ID || claim.Message.To != auth.Slot || claim.Message.TargetGeneration != auth.Generation || !sameLANClaimContent(m, claim.Message) {
			return errors.New("LAN claim receipt identity mismatch")
		}
		if err := guest.retainDelivery(claim, auth.Generation); err != nil {
			return err
		}
		// Forward the ORIGINAL receipt. Existing CLI and hooks acknowledge it
		// only after they have successfully written the final local envelope.
		writeManagementJSON(w, http.StatusOK, map[string]any{"claim": &relay.Claim{ID: claim.ID, Receipt: claim.Receipt, Envelope: envelope}})
		return nil
	}
}

func sameLANClaimContent(head, claimed relay.Message) bool {
	// The host transitions timing/state during claim. Every content field must
	// match the prefetched manifest before its local paths enter stdout.
	claimed.State, claimed.UpdatedAt, claimed.ClaimedAt = head.State, head.UpdatedAt, head.ClaimedAt
	claimed.Receipt = head.Receipt
	a, errA := json.Marshal(head)
	b, errB := json.Marshal(claimed)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func (guest *lanGuest) prepareEnvelope(ctx context.Context, m relay.Message) (string, error) {
	guest.mu.Lock()
	room := guest.record.Room
	localKey, _ := guest.record.Identity.Fingerprint()
	guest.mu.Unlock()
	if room == nil || len(m.Attachments) > 8 {
		return "", errors.New("LAN attachment manifest exceeds limit")
	}
	var handle string
	if m.From == model.ActorUser {
		switch m.Author {
		case "host_owner":
			handle = "@user (room host owner)"
		case "lan:" + localKey:
			handle = "@user (local owner)"
		default:
			handle = "@user (room participant)"
		}
	} else {
		handle = model.ParticipantIdentities(room.Runtimes)[m.From].MentionHandle
		if handle == "" {
			return "", errors.New("LAN sender has no admitted Runtime identity")
		}
	}
	input := model.AgentInput{From: m.From, To: m.To, FromHandle: handle, Text: m.Text, Quote: m.Quote}
	if m.Review != nil {
		input.Text += m.Review.Envelope()
	}
	var total int64
	for _, expected := range m.Attachments {
		total += expected.Size
		if total > attachment.MaxTotalImageBytes {
			return "", errors.New("LAN attachment manifest exceeds total limit")
		}
		metadata, path, err := guest.media.Resolve(expected.ID)
		if err != nil || metadata != expected {
			if err := guest.download(ctx, expected); err != nil {
				return "", err
			}
			metadata, path, err = guest.media.Resolve(expected.ID)
		}
		if err != nil || metadata != expected {
			return "", errors.New("LAN attachment does not match accepted message")
		}
		input.Attachments = append(input.Attachments, model.AgentAttachment{Attachment: metadata, Path: path})
	}
	return prompt.Envelope(input), nil
}

func (guest *lanGuest) download(ctx context.Context, expected model.Attachment) error {
	if !lanshare.ValidID(expected.ID) || expected.Size <= 0 || expected.Size > attachment.MaxImageBytes || !lanshare.ValidFingerprint(expected.SHA256) {
		return errors.New("invalid LAN attachment manifest")
	}
	guest.mu.Lock()
	invite := guest.record.Invite
	guest.mu.Unlock()
	body, err := json.Marshal(map[string]string{"id": expected.ID})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, invite.Endpoint+"/lan/v1/rooms/"+invite.RoomID+"/download", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := guest.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > expected.Size {
		return errors.New("LAN evidence download was not authorized or exceeded its manifest")
	}
	_, err = guest.media.ImportVerified(expected, io.LimitReader(response.Body, expected.Size+1))
	return err
}

func (guest *lanGuest) upload(w http.ResponseWriter, r *http.Request) {
	// Only the fixed upload route and one explicit attachment-kind header are
	// forwarded. Local relay/Management authentication and arbitrary headers
	// never reach the remote Service.
	kind := r.Header.Get("X-PairRoom-Attachment-Kind")
	if kind != "" && kind != "image" && kind != "file" {
		writeManagementError(w, http.StatusBadRequest, "invalid evidence kind")
		return
	}
	const maxUpload = (10 << 20) + (64 << 10)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxUpload))
	if err != nil {
		writeManagementError(w, http.StatusBadRequest, "attachment upload exceeds limit")
		return
	}
	guest.mu.Lock()
	invite := guest.record.Invite
	guest.mu.Unlock()
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, invite.Endpoint+"/lan/v1/rooms/"+invite.RoomID+"/upload", bytes.NewReader(body))
	if err != nil {
		writeLANBridgeError(w, err)
		return
	}
	request.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	request.Header.Set("X-PairRoom-Attachment-Kind", kind)
	response, err := guest.client.Do(request)
	if err != nil {
		writeLANBridgeError(w, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		writeManagementError(w, http.StatusBadRequest, "LAN attachment rejected by Room validation")
		return
	}
	var metadata model.Attachment
	if json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&metadata) != nil || !lanshare.ValidID(metadata.ID) || !lanshare.ValidFingerprint(metadata.SHA256) {
		writeManagementError(w, http.StatusBadGateway, "invalid LAN attachment receipt")
		return
	}
	writeManagementJSON(w, response.StatusCode, metadata)
}

// Management browser actions are scoped to this admitted Room. The remote
// human author is derived by the host from the authenticated member key.
func (s *ManagementServer) lanGuestOwnerAction(w http.ResponseWriter, r *http.Request) {
	guest := s.lanGuests.get(r.PathValue("room"))
	if guest == nil {
		writeManagementError(w, http.StatusNotFound, "joined Room not found")
		return
	}
	guest.mu.Lock()
	active := guest.record.Status == "accepted" || r.PathValue("action") == "leave"
	guest.mu.Unlock()
	if !active {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	var req lanGuestRelayRequest
	if decodeNativeJSON(w, r, &req) != nil {
		return
	}
	action := r.PathValue("action")
	var payload, result any
	switch action {
	case "summary":
		result = &relay.Summary{}
	case "receipt":
		action = "user-receipt"
		payload = map[string]string{"id": req.ID}
		result = &struct {
			Accepted bool           `json:"accepted"`
			Message  *relay.Message `json:"message,omitempty"`
		}{}
	case "history":
		payload = relay.HistoryQuery{ID: req.ID, Cursor: req.Cursor, Limit: req.Limit, Pending: req.Pending, Since: req.Since}
		result = &relay.HistoryPage{}
	case "send":
		action = "user-send"
		payload = relay.SendRequest{ID: req.ID, Text: req.Text, To: req.To, QuoteID: req.QuoteID, AttachmentIDs: req.AttachmentIDs}
		result = &relay.Message{}
	case "leave":
		action = "unbind"
		result = &map[string]bool{}
	default:
		writeManagementError(w, http.StatusNotFound, "unsupported joined Room view operation")
		return
	}
	if err := guest.call(r.Context(), action, payload, result); err != nil {
		if action != "unbind" || !lanMembershipDenied(err) {
			writeLANBridgeError(w, err)
			return
		}
		result = &map[string]bool{"unbound": true}
	}
	if action == "unbind" {
		guest.mu.Lock()
		next := guest.record
		next.Status = "left"
		if err := privatefile.WriteJSON(filepath.Join(guest.dir, "guest.json"), next); err != nil {
			guest.mu.Unlock()
			writeLANBridgeError(w, err)
			return
		}
		guest.record = next
		guest.mu.Unlock()
	}
	// No filesystem selector, callback URL, process, config or approval action
	// can be relayed by this Management surface.
	writeManagementJSON(w, http.StatusOK, result)
}

func (s *ManagementServer) lanGuestAttachment(w http.ResponseWriter, r *http.Request) {
	guest := s.lanGuests.get(r.PathValue("room"))
	if guest == nil {
		writeManagementError(w, http.StatusNotFound, "joined Room not found")
		return
	}
	id := r.PathValue("attachment")
	if !lanshare.ValidID(id) {
		writeManagementError(w, http.StatusBadRequest, "invalid attachment ID")
		return
	}
	// Even a cache hit needs a fresh remote authorization for this metadata;
	// knowledge of an object ID never grants another Room's file access.
	var expected model.Attachment
	if err := guest.call(r.Context(), "attachment", map[string]string{"id": id}, &expected); err != nil {
		writeLANBridgeError(w, err)
		return
	}
	if expected.ID != id {
		writeManagementError(w, http.StatusBadGateway, "attachment receipt mismatch")
		return
	}
	metadata, _, err := guest.media.Resolve(id)
	if err != nil || metadata != expected {
		if err := guest.download(r.Context(), expected); err != nil {
			writeLANBridgeError(w, err)
			return
		}
	}
	metadata, file, err := guest.media.OpenFile(id)
	if err != nil || metadata != expected {
		if file != nil {
			_ = file.Close()
		}
		writeManagementError(w, http.StatusBadGateway, "local evidence verification failed")
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, metadata.Name, metadata.CreatedAt, file)
}

func lanMembershipDenied(err error) bool {
	var failure *lanshare.Error
	return errors.As(err, &failure) && (failure.Status == http.StatusUnauthorized || failure.Status == http.StatusForbidden)
}
