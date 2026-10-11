package lanclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestConfirmationPrivacyAndExactPublicationIdentityMapping(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, options := f.join(t, s)
	var badPublication atomic.Bool
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		switch action {
		case "confirm":
			if string(data) != "null" {
				t.Errorf("local confirmation metadata was forwarded: %s", data)
			}
			reply(w, relay.Binding{Slot: auth.Slot, BindID: "remote-binding", Generation: 7})
		case "report", "publication":
			value := relay.Publication{BindID: "remote-binding", Generation: 7, ReportSeq: 12}
			if badPublication.Load() {
				value.BindID = "unrelated-remote-binding"
			}
			if action == "report" {
				reply(w, value)
			} else {
				reply(w, map[string]any{"accepted": true, "publication": value})
			}
		case "status":
			reply(w, relay.Snapshot{RoomID: "shared-room", Bindings: map[model.ActorID]relay.Binding{model.ActorSlot1: {SessionID: "remote-session", TranscriptPath: "/remote/transcript"}}})
		case "peer":
			reply(w, relay.Binding{Runtime: model.RuntimeClaude, SessionID: "remote-session", TranscriptPath: "/remote/transcript"})
		default:
			t.Errorf("unexpected operation %s", action)
			w.WriteHeader(404)
		}
	})
	var local relay.Binding
	if err := c.Relay(ctx, auth, "confirm", map[string]string{"session_id": auth.SessionID, "transcript_path": filepath.Join(options.Workspace, "private-transcript.jsonl")}, &local); err != nil || local.BindID != auth.BindID || local.SessionID != auth.SessionID || local.Generation != auth.Generation {
		t.Fatalf("local confirmation lost original native association: %+v %v", local, err)
	}
	var publication relay.Publication
	if err := c.Relay(ctx, auth, "report", map[string]any{"report_seq": 12, "text": "@claude full reply"}, &publication); err != nil || publication.BindID != auth.BindID || publication.Generation != auth.Generation || publication.ReportSeq != 12 {
		t.Fatalf("publication receipt lost local WAL identity: %+v %v", publication, err)
	}
	var receipt publicationReceipt
	if err := c.Relay(ctx, auth, "publication", map[string]int{"report_seq": 12}, &receipt); err != nil || !receipt.Accepted || receipt.Publication.BindID != auth.BindID {
		t.Fatalf("lookup receipt lost local WAL identity: %+v %v", receipt, err)
	}
	for _, action := range []string{"status", "peer"} {
		var result map[string]any
		if err := c.Relay(ctx, auth, action, nil, &result); err != nil {
			t.Fatal(err)
		}
		value, _ := json.Marshal(result)
		if strings.Contains(string(value), "remote-session") || strings.Contains(string(value), "/remote/transcript") {
			t.Fatal("peer native identity entered local relay context")
		}
	}
	badPublication.Store(true)
	for _, action := range []string{"report", "publication"} {
		if err := c.Relay(ctx, auth, action, map[string]int{"report_seq": 12}, nil); err == nil {
			t.Fatalf("%s accepted a receipt for a different remote binding", action)
		}
	}
	before := len(f.actions())
	if err := c.Relay(ctx, auth, "confirm", map[string]string{"session_id": "different-session"}, nil); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("confirmation changed local session: %v", err)
	}
	if err := c.Relay(ctx, auth, "invite", nil, nil); err == nil {
		t.Fatal("guest acquired host invitation authority")
	}
	if len(f.actions()) != before {
		t.Fatal("forbidden local operation reached host")
	}
}

func TestErrorClassificationNeverReflectsRemoteTextOrTreatsHTTPAsNoResponse(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, _ string, _ []byte) {
		w.WriteHeader(500)
		reply(w, map[string]string{"error": "peer-injected secret path and command", "code": "arbitrary-peer-code"})
	})
	err := c.Relay(ctx, auth, "doctor", nil, nil)
	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 500 || failure.Code != "" || strings.Contains(err.Error(), "peer-injected") || errors.Is(err, ErrTransportUnavailable) {
		t.Fatalf("remote error text/category escaped boundary: %v", err)
	}
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, _ string, _ []byte) {
		_, _ = io.WriteString(w, "malformed successful JSON")
	})
	if err := c.Relay(ctx, auth, "doctor", nil, nil); !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrTransportUnavailable) {
		t.Fatalf("HTTP parse failure became retryable no-response: %v", err)
	}
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, _ string, _ []byte) {
		w.WriteHeader(409)
		reply(w, map[string]string{"error": "untrusted details", "code": relay.SendPayloadConflictCode})
	})
	if err := c.Relay(ctx, auth, "send", relay.SendRequest{ID: "same-send", Text: "changed"}, nil); !errors.Is(err, relay.ErrSendPayloadConflict) {
		t.Fatalf("stable payload conflict identity was lost: %v", err)
	}
	f.server.Close()
	if err := c.Relay(ctx, auth, "doctor", nil, nil); !errors.Is(err, ErrTransportUnavailable) || strings.Contains(err.Error(), f.invite.Endpoint) {
		t.Fatalf("offline doctor exposed URL or lost no-response category: %v", err)
	}
}

func TestPendingExpiryRenewsOnlyRequestAndGenerationChangeFailsClosed(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	f.mu.Lock()
	f.status = "pending"
	f.mu.Unlock()
	options := f.options(t)
	c, result, err := s.Join(ctx, options)
	if err != nil || result.Status != "pending" || result.Binding != nil || result.Bootstrap != "" {
		t.Fatalf("pending request invented admitted native binding: %+v %v", result, err)
	}
	original, _ := c.read(ctx)
	f.mu.Lock()
	f.status = "expired"
	f.mu.Unlock()
	if result, err := c.Resume(ctx); err != nil || result.Status != "expired" {
		t.Fatalf("expiry observation failed: %+v %v", result, err)
	}
	f.mu.Lock()
	f.status, f.requestID, f.key = "pending", "", ""
	f.mu.Unlock()
	invite := f.invite
	invite.InviteID = "second-invitation"
	options.Invite = lanshare.EncodeInvite(invite)
	_, result, err = s.Join(ctx, options)
	if err != nil || result.Status != "pending" {
		t.Fatalf("explicit new invite could not renew expired request: %+v %v", result, err)
	}
	renewed, _ := c.read(ctx)
	if renewed.Identity != original.Identity || renewed.RequestID == original.RequestID || renewed.Invite.InviteID != invite.InviteID {
		t.Fatal("expired admission rotated Room key or reused retired request")
	}
	f.mu.Lock()
	f.status = "accepted"
	f.mu.Unlock()
	if result, err := c.Resume(ctx); err != nil || result.Status != "accepted" {
		t.Fatalf("renewed acceptance failed: %+v %v", result, err)
	}
	f.mu.Lock()
	f.room.Generation++
	f.mu.Unlock()
	if _, err := c.Resume(ctx); err == nil {
		t.Fatal("same key silently inherited a new membership generation")
	}
	r, _ := c.read(ctx)
	if r.Room.Generation != 7 || r.BindID != options.BindID || r.Identity != original.Identity {
		t.Fatal("failed membership validation rewrote original local scope")
	}
}
