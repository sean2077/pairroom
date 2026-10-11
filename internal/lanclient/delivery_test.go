package lanclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func textMessage() relay.Message {
	now := time.Now().UTC()
	return relay.Message{ID: "incoming-one", From: model.ActorSlot1, To: model.ActorSlot2, Text: "Please investigate the function's observed result.", TargetGeneration: 7, State: "queued", Source: "explicit", CreatedAt: now, UpdatedAt: now}
}

func collectOne(t *testing.T, c *Client, auth relay.Auth) relay.Claim {
	t.Helper()
	var result collectResult
	if err := c.Relay(context.Background(), auth, "wait", nil, &result); err != nil || result.Claim == nil {
		t.Fatalf("collect failed: %+v %v", result, err)
	}
	return *result.Claim
}

func TestOriginalAckRecoverySurvivesClientRestartWithoutClaimOrStdoutReplay(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	m := textMessage()
	var ackCount atomic.Int32
	var deny atomic.Bool
	var stdoutWritten atomic.Bool
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		switch action {
		case "head":
			reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: m, Digest: relay.Digest("accepted-manifest")}})
		case "claim":
			respondClaim(t, w, data, m, "exact-original-receipt")
		case "ack":
			if !stdoutWritten.Load() {
				t.Error("ACK preceded actual local stdout")
			}
			var request struct {
				ID      string `json:"id"`
				Receipt string `json:"receipt"`
			}
			if err := json.Unmarshal(data, &request); err != nil || request.ID != m.ID || request.Receipt != "exact-original-receipt" {
				t.Error("original ACK identity changed")
			}
			count := ackCount.Add(1)
			if deny.Load() {
				w.WriteHeader(403)
				return
			}
			if count == 1 {
				loseResponse(w)
				return
			}
			reply(w, map[string]bool{"handed_off": true})
		default:
			t.Errorf("receipt recovery contacted %s", action)
			w.WriteHeader(404)
		}
	})
	claim := collectOne(t, c, auth)
	original, err := c.read(ctx)
	if err != nil || len(original.Deliveries) != 1 || original.Deliveries[0].State != "claimed" {
		t.Fatal("claim was not persisted before returning envelope")
	}
	if err := c.Maintenance(ctx); err != nil {
		t.Fatal(err)
	}
	if ackCount.Load() != 0 {
		t.Fatal("claim response was treated as stdout after maintenance")
	}
	var stdout strings.Builder
	if _, err := stdout.WriteString(claim.Envelope); err != nil {
		t.Fatal(err)
	}
	stdoutWritten.Store(stdout.Len() > 0)
	err = c.Relay(ctx, auth, "ack", map[string]string{"id": claim.ID, "receipt": claim.Receipt}, nil)
	if !errors.Is(err, ErrTransportUnavailable) {
		t.Fatalf("lost ACK response classification: %v", err)
	}
	r, err := c.read(ctx)
	if err != nil || r.Deliveries[0].State != "stdout" {
		t.Fatal("lost ACK lost durable stdout receipt")
	}
	s.Close()
	restarted, err := OpenAt(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	c, err = restarted.Get(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeActions := len(f.actions())
	if err := c.Maintenance(ctx); err != nil {
		t.Fatal(err)
	}
	r, err = c.read(ctx)
	if err != nil || r.Deliveries[0].State != "acknowledged" || r.Identity != original.Identity || r.RequestID != original.RequestID {
		t.Fatal("original receipt recovery rotated identity or lost completion")
	}
	if got := strings.Join(f.actions()[beforeActions:], ","); got != "join-status,ack" {
		t.Fatalf("recovery claimed or replayed inbox text: %s", got)
	}
	if stdout.String() != claim.Envelope || ackCount.Load() != 2 {
		t.Fatal("recovery replayed stdout or changed acknowledgement count")
	}
	deniedBefore := len(f.actions())
	deny.Store(true)
	if err := c.Relay(ctx, auth, "ack", map[string]string{"id": claim.ID, "receipt": claim.Receipt}, nil); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("cached acknowledged receipt masked remote revocation: %v", err)
	}
	if len(f.actions()) != deniedBefore+1 || ackCount.Load() != 3 {
		t.Fatal("duplicate ACK skipped current remote membership authentication")
	}
	wrong := auth
	wrong.Generation++
	if err := c.Relay(ctx, wrong, "ack", map[string]string{"id": claim.ID, "receipt": claim.Receipt}, nil); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("old generation was admitted: %v", err)
	}
	if ackCount.Load() != 3 {
		t.Fatal("wrong-generation ACK reached host")
	}
}

func TestLostClaimResponseCannotAuthorizeAckOrRecoveryReplay(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	m := textMessage()
	var claims, acks atomic.Int32
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, _ []byte) {
		switch action {
		case "head":
			reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: m, Digest: relay.Digest("accepted-manifest")}})
		case "claim":
			claims.Add(1)
			loseResponse(w)
		case "ack":
			acks.Add(1)
			reply(w, map[string]bool{"handed_off": true})
		default:
			t.Errorf("unexpected recovery operation %s", action)
		}
	})
	var result collectResult
	if err := c.Relay(ctx, auth, "wait", nil, &result); !errors.Is(err, ErrTransportUnavailable) || result.Claim != nil {
		t.Fatalf("lost claim was returned as an envelope: %+v %v", result, err)
	}
	if err := c.Relay(ctx, auth, "ack", map[string]string{"id": m.ID, "receipt": "unobserved-original-receipt"}, nil); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("unknown receipt authorized stdout: %v", err)
	}
	restarted, err := OpenAt(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	c, err = restarted.Get(ctx, c.id)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.actions())
	if err := c.Maintenance(ctx); err != nil {
		t.Fatal(err)
	}
	if claims.Load() != 1 || acks.Load() != 0 || strings.Join(f.actions()[before:], ",") != "join-status" {
		t.Fatal("recovery invented claim/ACK/stdout for a lost claim response")
	}
	r, _ := c.read(ctx)
	if len(r.Deliveries) != 0 {
		t.Fatal("lost claim response fabricated a local receipt")
	}
}

func TestExplicitAckRefusalRetainsUnknownWithoutAutomaticRetry(t *testing.T) {
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	m := textMessage()
	var acks atomic.Int32
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		switch action {
		case "head":
			reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: m, Digest: relay.Digest("accepted-manifest")}})
		case "claim":
			respondClaim(t, w, data, m, "refused-original")
		case "ack":
			acks.Add(1)
			w.WriteHeader(409)
			reply(w, map[string]string{"error": "inspect an explicit retry"})
		default:
			t.Errorf("unexpected action %s", action)
		}
	})
	claim := collectOne(t, c, auth)
	err := c.Relay(context.Background(), auth, "ack", map[string]string{"id": claim.ID, "receipt": claim.Receipt}, nil)
	var remoteError *Error
	if !errors.As(err, &remoteError) || remoteError.Status != 409 {
		t.Fatalf("definite refusal lost HTTP classification: %v", err)
	}
	if err := c.Maintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	r, _ := c.read(context.Background())
	if acks.Load() != 1 || r.Deliveries[0].State != "unknown" {
		t.Fatal("explicit ACK refusal was retried")
	}
}

func TestConcurrentAckCompletionPreservesOtherProcessLocalUpdates(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, options := f.join(t, s)
	m := textMessage()
	ackStarted, releaseAck := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseAck) })
	t.Cleanup(release)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		switch action {
		case "head":
			reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: m, Digest: relay.Digest("accepted-manifest")}})
		case "claim":
			respondClaim(t, w, data, m, "concurrent-original")
		case "ack":
			close(ackStarted)
			<-releaseAck
			reply(w, map[string]bool{"handed_off": true})
		case "confirm":
			reply(w, relay.Binding{})
		case "park":
			reply(w, map[string]bool{"enabled": false})
		default:
			t.Errorf("unexpected action %s", action)
		}
	})
	claim := collectOne(t, c, auth)
	finished := make(chan error, 1)
	go func() {
		finished <- c.Relay(ctx, auth, "ack", map[string]string{"id": claim.ID, "receipt": claim.Receipt}, nil)
	}()
	select {
	case <-ackStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("ACK did not reach transport")
	}
	other, err := OpenAt(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	clone, err := other.Get(ctx, c.id)
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(options.Workspace, "private-transcript.jsonl")
	if err := clone.Relay(ctx, auth, "confirm", map[string]string{"session_id": auth.SessionID, "transcript_path": transcript}, nil); err != nil {
		t.Fatal(err)
	}
	if err := clone.Relay(ctx, auth, "park", map[string]bool{"enabled": false}, nil); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	r, err := c.read(ctx)
	if err != nil || r.TranscriptPath != transcript || r.ParkEnabled || r.LastActivity.IsZero() || r.Deliveries[0].State != "acknowledged" {
		t.Fatalf("ACK completion overwrote independent process state: %+v %v", r.Deliveries, err)
	}
}

func TestAdmissionObservationDoesNotRewriteUnchangedRecord(t *testing.T) {
	s, f := newStore(t), newRemote(t)
	c, _, _ := f.join(t, s)
	file := filepath.Join(c.dir, "client.json")
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Maintenance(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(file)
	if err != nil || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged status observation rewrote private client state")
	}
}

func TestResumeAndForegroundWaitRecoverOnlyOriginalStdoutAck(t *testing.T) {
	for _, action := range []string{"resume", "wait"} {
		t.Run(action, func(t *testing.T) {
			s, f := newStore(t), newRemote(t)
			c, auth, _ := f.join(t, s)
			m := textMessage()
			var heads, acks atomic.Int32
			f.setHandler(func(w http.ResponseWriter, _ *http.Request, op string, data []byte) {
				switch op {
				case "head":
					if heads.Add(1) > 1 {
						reply(w, lanshare.HeadResponse{})
						return
					}
					reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: m, Digest: relay.Digest("accepted-manifest")}})
				case "claim":
					respondClaim(t, w, data, m, "foreground-original")
				case "ack":
					var request struct {
						ID      string `json:"id"`
						Receipt string `json:"receipt"`
					}
					if json.Unmarshal(data, &request) != nil || request.ID != m.ID || request.Receipt != "foreground-original" {
						t.Error("recovered ACK changed original identity")
					}
					if acks.Add(1) == 1 {
						loseResponse(w)
						return
					}
					reply(w, map[string]bool{"handed_off": true})
				default:
					t.Errorf("unexpected operation %s", op)
				}
			})
			claim := collectOne(t, c, auth)
			if err := c.Relay(context.Background(), auth, "ack", map[string]string{"id": claim.ID, "receipt": claim.Receipt}, nil); !errors.Is(err, ErrTransportUnavailable) {
				t.Fatal(err)
			}
			before := len(f.actions())
			if action == "resume" {
				if result, err := c.Resume(context.Background()); err != nil || result.Status != "accepted" {
					t.Fatalf("accepted resume did not recover original ACK: %+v %v", result, err)
				}
				if got := strings.Join(f.actions()[before:], ","); got != "join-status,ack" {
					t.Fatalf("resume replayed body or claim: %s", got)
				}
			} else {
				var result collectResult
				if err := c.Relay(context.Background(), auth, "wait", nil, &result); err != nil || result.Claim != nil {
					t.Fatalf("foreground wait replayed prior claim: %+v %v", result, err)
				}
				if got := strings.Join(f.actions()[before:], ","); got != "ack,head" {
					t.Fatalf("collect began before original ACK reconciliation: %s", got)
				}
			}
			r, _ := c.read(context.Background())
			if acks.Load() != 2 || r.Deliveries[0].State != "acknowledged" {
				t.Fatal("original foreground receipt remains unrecovered")
			}
		})
	}
}

func TestRetainedOriginalCannotBePrintedAgainButExplicitRetryHasNewIdentity(t *testing.T) {
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	original := textMessage()
	var retry atomic.Bool
	var claims atomic.Int32
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		m, receipt := original, "original-unprinted"
		if retry.Load() {
			m.ID, m.RetryOf, receipt = "explicit-retry-message", original.ID, "explicit-retry-receipt"
		}
		switch action {
		case "head":
			reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: m, Digest: relay.Digest("accepted-manifest")}})
		case "claim":
			claims.Add(1)
			respondClaim(t, w, data, m, receipt)
		default:
			t.Errorf("unexpected operation %s", action)
		}
	})
	_ = collectOne(t, c, auth)
	var result collectResult
	if err := c.Relay(context.Background(), auth, "wait", nil, &result); err == nil || result.Claim != nil || claims.Load() != 1 {
		t.Fatalf("retained original envelope was claimed or returned twice: %+v %v", result, err)
	}
	retry.Store(true)
	claim := collectOne(t, c, auth)
	if claim.ID != "explicit-retry-message" || claim.Receipt != "explicit-retry-receipt" || claims.Load() != 2 {
		t.Fatalf("explicit new retry identity was blocked: %+v", claim)
	}
	r, _ := c.read(context.Background())
	if len(r.Deliveries) != 2 || r.Deliveries[0].ID != original.ID || r.Deliveries[0].State != "claimed" {
		t.Fatal("explicit retry rewrote the original unresolved receipt")
	}
}

// A long-lived membership accumulates definitively refused acknowledgements that
// can never settle. They must not wedge collection, and a full journal sheds
// them instead of refusing new work.
func TestTerminalDeliveryReceiptsDoNotWedgeCollection(t *testing.T) {
	full := record{Schema: 1}
	for i := 0; i < maxDeliveries; i++ {
		full.Deliveries = append(full.Deliveries, delivery{ID: fmt.Sprintf("relay-terminal-%d", i), Receipt: "receipt", Generation: 1, State: "unknown"})
	}
	if err := deliveryCapacity(full); err != nil {
		t.Fatalf("terminal receipts wedged collection: %v", err)
	}
	trimmed, err := trimDeliveries(full.Deliveries, "relay-next")
	if err != nil || len(trimmed) != 0 {
		t.Fatalf("full journal did not shed terminal receipts: %d %v", len(trimmed), err)
	}
	recoverable := record{Schema: 1}
	for i := 0; i < maxDeliveries; i++ {
		recoverable.Deliveries = append(recoverable.Deliveries, delivery{ID: fmt.Sprintf("relay-stdout-%d", i), Receipt: "receipt", Generation: 1, State: "stdout"})
	}
	if err := deliveryCapacity(recoverable); err == nil {
		t.Fatal("recoverable receipts exceeded their own bound")
	}
	kept, err := trimDeliveries(recoverable.Deliveries, "relay-next")
	if err != nil || len(kept) != maxDeliveries {
		t.Fatalf("recoverable receipts were shed: %d %v", len(kept), err)
	}
	if _, err := trimDeliveries(full.Deliveries, "relay-terminal-0"); err == nil {
		t.Fatal("an already retained receipt was accepted twice")
	}
}
