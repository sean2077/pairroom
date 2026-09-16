package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
	"github.com/sean2077/pairroom/internal/relay"
)

func (s *ManagementServer) mountNativeRelay(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/rooms/{room}/native-bindings/{slot}", s.bindNative)
	mux.HandleFunc("DELETE /api/v1/rooms/{room}/native-bindings/{slot}", s.unbindNative)
	mux.HandleFunc("POST /api/v1/relay/{room}/{slot}/{action}", s.nativeRelay)
}
func (s *ManagementServer) nativeRuntime(ctx context.Context, id string) (*nativeHostRuntime, error) {
	room, ok := s.registry.Room(id)
	if !ok {
		return nil, ErrRoomNotFound
	}
	if room.HostMode != model.HostNative || room.Archived() {
		return nil, errors.New("relay requires an active native Room")
	}
	runtime, _, err := s.runtimes.Activate(ctx, id)
	if err != nil {
		return nil, err
	}
	native, ok := runtime.(*nativeHostRuntime)
	if !ok {
		return nil, errors.New("Room is not a native relay runtime")
	}
	return native, nil
}
func (s *ManagementServer) bindNative(w http.ResponseWriter, r *http.Request) {
	var req relay.BindRequest
	if decodeNativeJSON(w, r, &req) != nil {
		return
	}
	slot := canonicalInputSlot(model.ActorID(r.PathValue("slot")))
	if !slot.ValidParticipant() {
		writeManagementError(w, 400, "invalid slot")
		return
	}
	runtime, err := s.nativeRuntime(r.Context(), r.PathValue("room"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	release := runtime.acquire()
	defer release()
	binding, err := runtime.engine.Bind(slot, req)
	if err != nil {
		nativeResult(w, nil, err)
		return
	}
	bootstrap := protocol.NativeBootstrap(slot, runtime.room.Agents[slot].Runtime, runtime.room.Agents[model.OtherParticipant(slot)].Runtime)
	nativeResult(w, map[string]any{"binding": binding, "bootstrap": bootstrap, "collaboration": protocol.CollaborationInstructions(slot, runtime.room.Collaboration), "workspace": runtime.project.Root, "runtime": runtime.room.Agents[slot].Runtime, "notice": "Provider/model/effort/permissions are display-only. Native work is not stopped by replace. This session is associated from its harness environment at bind; the approved Stop hook re-confirms the same session and relays replies."}, nil)
}
func (s *ManagementServer) unbindNative(w http.ResponseWriter, r *http.Request) {
	runtime, err := s.nativeRuntime(r.Context(), r.PathValue("room"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	release := runtime.acquire()
	defer release()
	slot := canonicalInputSlot(model.ActorID(r.PathValue("slot")))
	if !slot.ValidParticipant() {
		writeManagementError(w, 400, "invalid slot")
		return
	}
	nativeResult(w, map[string]bool{"unbound": true}, runtime.engine.Unbind(slot))
}

func (s *ManagementServer) nativeRelay(w http.ResponseWriter, r *http.Request) {
	// This route never treats a browser cookie or a management token as relay
	// identity. The secret stays in the header and is never reflected in errors.
	if r.URL.Query().Has("token") {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	slot := canonicalInputSlot(model.ActorID(r.PathValue("slot")))
	if !slot.ValidParticipant() {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	auth, err := parseRelayAuth(r, slot)
	if err != nil {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	durable, ok := s.registry.Room(r.PathValue("room"))
	if !ok || durable.HostMode != model.HostNative || durable.Archived() {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	// Active Rooms already own the authoritative binding projection. Avoid
	// rereading the complete Event Log on every 30-second poll. A suspended
	// Room still authenticates from durable facts before consuming capacity.
	active, activeErr := s.runtimes.runtimeForCompletion(durable.ID)
	if activeErr != nil && !errors.Is(activeErr, ErrRuntimeNotReady) {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	if authenticateNativeRelay(active, durable.DataDir, auth, readEventsReadOnly) != nil {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	var runtime *nativeHostRuntime
	if r.PathValue("action") == "ack" {
		var active RoomRuntime
		active, err = s.runtimes.runtimeForCompletion(durable.ID)
		if err == nil {
			var ok bool
			runtime, ok = active.(*nativeHostRuntime)
			if !ok {
				err = errors.New("Room is not a native relay runtime")
			}
		}
	} else {
		runtime, err = s.nativeRuntime(r.Context(), durable.ID)
	}
	if err != nil {
		s.writeError(w, err)
		return
	}
	release := runtime.acquire()
	defer release()
	action := r.PathValue("action")
	if action == "upload" {
		binding, err := runtime.engine.Inspect(auth)
		if err != nil || binding.SessionID == "" {
			nativeResult(w, nil, relay.ErrAuth)
			return
		}
		runtime.upload(w, r)
		return
	}
	var req struct {
		SessionID      string        `json:"session_id,omitempty"`
		TranscriptPath string        `json:"transcript_path,omitempty"`
		ReportSeq      uint64        `json:"report_seq,omitempty"`
		Text           string        `json:"text,omitempty"`
		ID             string        `json:"id,omitempty"`
		Receipt        string        `json:"receipt,omitempty"`
		To             model.ActorID `json:"to,omitempty"`
		AttachmentIDs  []string      `json:"attachment_ids,omitempty"`
		QuoteID        string        `json:"quote_id,omitempty"`
		Park           bool          `json:"park,omitempty"`
		TimeoutSeconds int           `json:"timeout_seconds,omitempty"`
		Enabled        bool          `json:"enabled,omitempty"`
		Error          string        `json:"error,omitempty"`
	}
	if decodeNativeJSON(w, r, &req) != nil {
		return
	}
	switch action {
	case "inspect":
		b, err := runtime.engine.Inspect(auth)
		nativeResult(w, b, err)
	case "confirm":
		b, err := runtime.engine.ConfirmSession(auth, req.SessionID, req.TranscriptPath)
		nativeResult(w, b, err)
	case "report":
		p, err := runtime.engine.Report(auth, req.ReportSeq, req.Text)
		nativeResult(w, p, err)
	case "publication":
		p, accepted, err := runtime.engine.Publication(auth, req.ReportSeq)
		nativeResult(w, map[string]any{"accepted": accepted, "publication": p}, err)
	case "send":
		m, err := runtime.engine.Send(auth, relay.SendRequest{ID: req.ID, Text: req.Text, To: req.To, AttachmentIDs: req.AttachmentIDs, QuoteID: req.QuoteID})
		nativeResult(w, m, err)
	case "wait":
		seconds := req.TimeoutSeconds
		if seconds <= 0 {
			seconds = int(relay.DefaultPark / time.Second)
		}
		if seconds > int(relay.MaxPark/time.Second) {
			writeManagementError(w, 400, "wait timeout exceeds 30 seconds")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), time.Duration(seconds)*time.Second)
		defer cancel()
		if req.Park && runtime.room.Agents[auth.Slot].Runtime == model.RuntimeGrok {
			// Grok clips Stop feedback. Readiness must not claim/ack a body
			// that the native harness could truncate before the model reads it.
			ready, err := runtime.engine.WaitForPending(ctx, auth)
			if errors.Is(err, context.DeadlineExceeded) {
				err = nil
			}
			nativeResult(w, map[string]any{"claim": nil, "foreground_required": ready}, err)
			return
		}
		claim, err := runtime.engine.Claim(ctx, auth, req.Park)
		if errors.Is(err, context.DeadlineExceeded) {
			err = nil
		}
		nativeResult(w, map[string]any{"claim": claim}, err)
	case "ack":
		nativeResult(w, map[string]bool{"handed_off": true}, runtime.engine.Ack(auth, req.ID, req.Receipt))
	case "status":
		snapshot, err := runtime.engine.AuthSnapshot(auth)
		nativeResult(w, snapshot, err)
	case "summary":
		summary, err := runtime.engine.AuthSummary(auth)
		nativeResult(w, summary, err)
	case "peer":
		peer, err := runtime.engine.Peer(auth)
		if err == nil {
			peer.Runtime = runtime.room.Agents[peer.Slot].Runtime.CanonicalForSlot(peer.Slot)
		}
		nativeResult(w, peer, err)
	case "failure":
		nativeResult(w, map[string]bool{"recorded": true}, runtime.engine.Failure(auth, req.Error))
	case "park":
		nativeResult(w, map[string]bool{"enabled": req.Enabled}, runtime.engine.ParkAs(auth, req.Enabled))
	case "unbind":
		nativeResult(w, map[string]bool{"unbound": true}, runtime.engine.UnbindAs(auth))
	default:
		writeManagementError(w, 404, "unknown relay operation")
	}
}

// Native response bodies are bounded at 256 KiB before JSON escaping. Reserve
// enough wire space for control-character escaping without weakening the small
// Management configuration request boundary or reflecting untrusted JSON.
func decodeNativeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeManagementError(w, 400, "invalid or oversized native relay JSON")
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		writeManagementError(w, 400, "invalid native relay JSON suffix")
		return err
	}
	return nil
}
