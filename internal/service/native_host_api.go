package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
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
	slot := model.ActorID(r.PathValue("slot"))
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
	nativeResult(w, map[string]any{"binding": binding, "bootstrap": bootstrap, "collaboration": protocol.CollaborationInstructions(slot, runtime.room.Collaboration), "workspace": runtime.project.Root, "runtime": runtime.room.Agents[slot].Runtime, "notice": "Provider/model/effort/permissions are display-only. Native work is not stopped by replace. Association is pending until your approved Stop hook returns the nonce."}, nil)
}
func (s *ManagementServer) unbindNative(w http.ResponseWriter, r *http.Request) {
	runtime, err := s.nativeRuntime(r.Context(), r.PathValue("room"))
	if err != nil {
		s.writeError(w, err)
		return
	}
	release := runtime.acquire()
	defer release()
	nativeResult(w, map[string]bool{"unbound": true}, runtime.engine.Unbind(model.ActorID(r.PathValue("slot"))))
}

func (s *ManagementServer) nativeRelay(w http.ResponseWriter, r *http.Request) {
	// This route never treats a browser cookie or a management token as relay
	// identity. The secret stays in the header and is never reflected in errors.
	if r.URL.Query().Has("token") {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	auth, err := parseRelayAuth(r, model.ActorID(r.PathValue("slot")))
	if err != nil {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	durable, ok := s.registry.Room(r.PathValue("room"))
	if !ok || durable.HostMode != model.HostNative || durable.Archived() {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	// Reject unauthorized activation before acquiring runtime capacity. Recheck
	// inside Engine operations after activation to close replace/revoke races.
	events, err := readEventsReadOnly(filepath.Join(durable.DataDir, "events.jsonl"))
	if err != nil {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	var authenticated bool
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != relay.EventBinding {
			continue
		}
		b, err := relay.BindingFromEvent(events[i])
		if err != nil {
			break
		}
		if b.Slot != auth.Slot {
			continue
		}
		authenticated = relay.AuthenticateBindingEvent(events[i], auth) == nil
		break
	}
	if !authenticated {
		nativeResult(w, nil, relay.ErrAuth)
		return
	}
	runtime, err := s.nativeRuntime(r.Context(), durable.ID)
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
		Nonce          string        `json:"nonce,omitempty"`
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
	case "associate":
		b, err := runtime.engine.Associate(auth, req.Nonce, req.SessionID, req.TranscriptPath)
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
	case "peer":
		peer, err := runtime.engine.Peer(auth)
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
