package lanclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

func incomingFile(t *testing.T) (relay.Message, string, string) {
	t.Helper()
	dir := localDir(t)
	media, err := attachment.Open(dir, localDir(t))
	if err != nil {
		t.Fatal(err)
	}
	const content = "#!/bin/sh\nprintf 'reproduce the observed bug\\n'\n"
	metadata, err := media.SaveEvidence("repro.sh", strings.NewReader(content), "lan")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	return relay.Message{ID: "incoming-one", From: model.ActorSlot1, To: model.ActorSlot2, Text: "Inspect this repro before running it.", Attachments: []model.Attachment{metadata}, TargetGeneration: 7, State: "queued", Source: "explicit", CreatedAt: now, UpdatedAt: now}, content, dir
}

func respondClaim(t *testing.T, w http.ResponseWriter, data []byte, m relay.Message, receipt string) {
	t.Helper()
	var req lanshare.ClaimRequest
	if err := json.Unmarshal(data, &req); err != nil || req.ID != m.ID || req.Digest != relay.Digest("accepted-manifest") || req.Generation != 7 {
		t.Errorf("claim did not identify exact prefetched head: %+v %v", req, err)
	}
	m.State, m.ClaimedAt = "delivering", time.Now().UTC()
	reply(w, lanshare.ClaimResponse{Claim: &lanshare.Claim{ID: m.ID, Receipt: receipt, Message: m}})
}

func TestPrefetchFinishesBeforeClaimAndRendersVerifiedPrivateLocalPath(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, options := f.join(t, s)
	message, evidence, hostDir := incomingFile(t)
	downloadStarted, releaseDownload := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseDownload) })
	t.Cleanup(release)
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		switch action {
		case "head":
			reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: message, Digest: relay.Digest("accepted-manifest")}})
		case "download":
			close(downloadStarted)
			<-releaseDownload
			_, _ = io.WriteString(w, evidence)
		case "claim":
			respondClaim(t, w, data, message, "original-receipt")
		case "confirm":
			reply(w, relay.Binding{Slot: auth.Slot, BindID: "remote-binding", Generation: 7, Active: true})
		default:
			t.Errorf("unexpected foreground operation %s", action)
			w.WriteHeader(404)
		}
	})
	var collected collectResult
	finished := make(chan error, 1)
	go func() { finished <- c.Relay(ctx, auth, "wait", map[string]int{"timeout_seconds": 30}, &collected) }()
	select {
	case <-downloadStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("no evidence prefetch")
	}
	if got := strings.Join(f.actions(), ","); got != "join,head,download" {
		t.Fatalf("claim or ACK preceded complete evidence: %s", got)
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
	if err := clone.Relay(ctx, auth, "wait", nil, new(collectResult)); !errors.Is(err, ErrCollectorBusy) {
		t.Fatalf("independent client acquired concurrent collector: %v", err)
	}
	transcript := filepath.Join(options.Workspace, "private-transcript.jsonl")
	if err := clone.Relay(ctx, auth, "confirm", map[string]string{"session_id": auth.SessionID, "transcript_path": transcript}, nil); err != nil {
		t.Fatalf("slow transfer held local state lock: %v", err)
	}
	release()
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if collected.Claim == nil || collected.Claim.ID != message.ID || collected.Claim.Receipt != "original-receipt" {
		t.Fatalf("original claim receipt missing: %+v", collected)
	}
	media, err := attachment.Open(filepath.Join(c.dir, "evidence"), options.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	metadata, path, err := media.Resolve(message.Attachments[0].ID)
	if err != nil || metadata != message.Attachments[0] || metadata.SHA256 != relay.Digest(evidence) {
		t.Fatalf("cache manifest or hash changed: %+v %v", metadata, err)
	}
	actual, err := os.ReadFile(path)
	if err != nil || string(actual) != evidence || !strings.Contains(collected.Claim.Envelope, "; path: "+strconv.Quote(path)+"\n") || !strings.HasPrefix(path, c.dir+string(filepath.Separator)) || filepath.Ext(path) != ".data" {
		t.Fatal("envelope did not render verified bytes at a private local inert path")
	}
	quotedHost := strconv.Quote(hostDir)
	if strings.Contains(collected.Claim.Envelope, hostDir) || strings.Contains(collected.Claim.Envelope, quotedHost[1:len(quotedHost)-1]) {
		t.Fatal("remote filesystem path reached local envelope")
	}
	if err := privatefile.CheckDirectory(filepath.Join(c.dir, "evidence")); err != nil {
		t.Fatal(err)
	}
	r, err := c.read(ctx)
	if err != nil || r.TranscriptPath != transcript || len(r.Deliveries) != 1 || r.Deliveries[0].State != "claimed" {
		t.Fatalf("claim overwrite lost concurrent local confirmation: %+v %v", r.Deliveries, err)
	}
	if got := strings.Join(f.actions(), ","); got != "join,head,download,confirm,claim" {
		t.Fatalf("claim response acknowledged before stdout: %s", got)
	}
}

func TestInvalidEvidenceCannotConsumeAndGrokReadinessDoesNotDownload(t *testing.T) {
	for _, scenario := range []string{"corrupt-bytes", "invalid-manifest", "grok-park"} {
		t.Run(scenario, func(t *testing.T) {
			s, f := newStore(t), newRemote(t)
			c, auth, _ := f.join(t, s)
			m, _, _ := incomingFile(t)
			if scenario == "invalid-manifest" {
				m.Attachments[0].Size = -1
			}
			var claims, downloads atomic.Int32
			f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, _ []byte) {
				switch action {
				case "head":
					reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: m, Digest: relay.Digest("accepted-manifest")}})
				case "download":
					downloads.Add(1)
					_, _ = io.WriteString(w, "corrupt bytes")
				case "claim":
					claims.Add(1)
					reply(w, lanshare.ClaimResponse{})
				default:
					t.Errorf("unexpected action %s", action)
				}
			})
			var result collectResult
			err := c.Relay(context.Background(), auth, "wait", map[string]bool{"park": scenario == "grok-park"}, &result)
			if scenario == "grok-park" {
				if err != nil || result.Claim != nil || !result.ForegroundRequired || downloads.Load() != 0 {
					t.Fatalf("Grok readiness consumed evidence or text: %+v %v", result, err)
				}
			} else if err == nil {
				t.Fatal("unverified evidence was accepted")
			}
			if claims.Load() != 0 {
				t.Fatal("invalid/unneeded evidence started a delivery lease")
			}
			r, _ := c.read(context.Background())
			if len(r.Deliveries) != 0 {
				t.Fatal("failed evidence produced a recoverable receipt")
			}
		})
	}
}

func TestChangedConditionalHeadIsPreparedAgainBeforeClaim(t *testing.T) {
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	first, bytes1, _ := incomingFile(t)
	second, bytes2, _ := incomingFile(t)
	second.ID = "incoming-two"
	var heads, claims int
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		switch action {
		case "head":
			heads++
			m := first
			if heads > 1 {
				m = second
			}
			reply(w, lanshare.HeadResponse{Head: &lanshare.Head{Message: m, Digest: relay.Digest("accepted-manifest")}})
		case "download":
			var request struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(data, &request)
			if request.ID == first.Attachments[0].ID {
				_, _ = io.WriteString(w, bytes1)
			} else if request.ID == second.Attachments[0].ID {
				_, _ = io.WriteString(w, bytes2)
			} else {
				t.Error("downloaded an unknown manifest")
			}
		case "claim":
			claims++
			if claims == 1 {
				reply(w, lanshare.ClaimResponse{})
				return
			}
			respondClaim(t, w, data, second, "successor-receipt")
		default:
			t.Errorf("unexpected action %s", action)
		}
	})
	var result collectResult
	if err := c.Relay(context.Background(), auth, "wait", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result.Claim == nil || result.Claim.ID != second.ID || result.Claim.Receipt != "successor-receipt" {
		t.Fatalf("conditional head changed receipt identity: %+v", result)
	}
	if got := strings.Join(f.actions(), ","); got != "join,head,download,claim,head,download,claim" {
		t.Fatalf("successor was not independently prefetched: %s", got)
	}
}

func TestUploadAndOwnerCacheRequireExactBytesAndFreshRoomAuthorization(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, _ := f.join(t, s)
	hostMedia, err := attachment.Open(localDir(t), localDir(t))
	if err != nil {
		t.Fatal(err)
	}
	const evidence = "feature X failed at line 5\n"
	var uploaded model.Attachment
	var downloads, metadataReads atomic.Int32
	var denied atomic.Bool
	f.setHandler(func(w http.ResponseWriter, r *http.Request, action string, data []byte) {
		switch action {
		case "upload":
			r.Body = io.NopCloser(strings.NewReader(string(data)))
			parts, err := r.MultipartReader()
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			part, err := parts.NextPart()
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if part.FileName() != "log.txt" || r.Header.Get("X-PairRoom-Attachment-Kind") != "file" {
				t.Error("upload exposed a source path or lost evidence kind")
			}
			uploaded, err = hostMedia.SaveEvidence(part.FileName(), part, "lan")
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			_ = part.Close()
			if _, err := parts.NextPart(); err != io.EOF {
				t.Error("uploaded more than one part")
			}
			reply(w, uploaded)
		case "attachment":
			metadataReads.Add(1)
			if denied.Load() {
				w.WriteHeader(403)
				return
			}
			reply(w, uploaded)
		case "download":
			downloads.Add(1)
			_, _ = io.WriteString(w, evidence)
		case "user-send":
			var req relay.SendRequest
			_ = json.Unmarshal(data, &req)
			if req.ID != "owner-send" || len(req.AttachmentIDs) != 1 || req.AttachmentIDs[0] != uploaded.ID {
				t.Error("owner send lost explicit evidence identity")
			}
			reply(w, relay.Message{ID: "host-user-message", From: model.ActorUser, Author: "lan:fixture", Text: req.Text})
		case "user-receipt":
			reply(w, map[string]any{"accepted": true, "message": relay.Message{ID: "host-user-message", From: model.ActorUser}})
		default:
			t.Errorf("unexpected owner action %s", action)
			w.WriteHeader(404)
		}
	})
	metadata, err := c.Upload(ctx, auth, "file", `C:\private\source\log.txt`, strings.NewReader(evidence))
	if err != nil || metadata.SHA256 != relay.Digest(evidence) || metadata.Size != int64(len(evidence)) {
		t.Fatalf("uploaded bytes receipt mismatch: %+v %v", metadata, err)
	}
	var message relay.Message
	if err := c.Owner(ctx, "send", relay.SendRequest{ID: "owner-send", Text: "please inspect", To: auth.Slot, AttachmentIDs: []string{metadata.ID}}, &message); err != nil || message.ID != "host-user-message" {
		t.Fatalf("owner send: %+v %v", message, err)
	}
	var receipt struct {
		Accepted bool          `json:"accepted"`
		Message  relay.Message `json:"message"`
	}
	if err := c.Owner(ctx, "receipt", map[string]string{"id": "owner-send"}, &receipt); err != nil || !receipt.Accepted || receipt.Message.ID != message.ID {
		t.Fatalf("original owner receipt: %+v %v", receipt, err)
	}
	for range 2 {
		got, path, err := c.Download(ctx, metadata.ID)
		if err != nil || got != metadata {
			t.Fatalf("owner download: %+v %v", got, err)
		}
		actual, err := os.ReadFile(path)
		if err != nil || string(actual) != evidence {
			t.Fatal("cached owner download differs from actual bytes")
		}
	}
	if downloads.Load() != 1 || metadataReads.Load() != 2 {
		t.Fatal("cache hit bypassed fresh Room ACL or re-downloaded unchanged bytes")
	}
	denied.Store(true)
	if _, _, err := c.Download(ctx, metadata.ID); !errors.Is(err, relay.ErrAuth) {
		t.Fatalf("revoked cache leaked evidence: %v", err)
	}
	before := len(f.actions())
	if err := c.Owner(ctx, "process-start", nil, nil); err == nil {
		t.Fatal("owner process action forwarded")
	}
	if err := c.Owner(ctx, "send", map[string]string{"endpoint": "http://peer-management"}, nil); err == nil {
		t.Fatal("arbitrary owner payload forwarded")
	}
	if len(f.actions()) != before {
		t.Fatal("unsupported owner action reached remote")
	}
}

// A slow remote download must not hold the evidence-directory lock: another
// caller's own download completes while the first one is still in flight.
func TestSlowEvidenceDownloadDoesNotBlockAnotherCaller(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, _, _ := f.join(t, s)
	message, content, _ := incomingFile(t)
	first := message.Attachments[0]
	second := first
	second.ID = "att-" + strings.Repeat("2", 24)
	started, release := make(chan struct{}), make(chan struct{})
	f.setHandler(func(w http.ResponseWriter, _ *http.Request, action string, data []byte) {
		var request map[string]string
		_ = json.Unmarshal(data, &request)
		switch action {
		case "attachment":
			if request["id"] == second.ID {
				reply(w, second)
				return
			}
			reply(w, first)
		case "download":
			if request["id"] == first.ID {
				close(started)
				<-release
			}
			_, _ = io.WriteString(w, content)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := c.Download(ctx, first.ID)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first download never started")
	}
	secondCtx, cancelSecond := context.WithTimeout(ctx, 5*time.Second)
	defer cancelSecond()
	if _, path, err := c.Download(secondCtx, second.ID); err != nil {
		t.Fatalf("second download blocked behind the in-flight one: %v", err)
	} else if _, err := os.Stat(path); err != nil {
		t.Fatalf("second download did not store verified evidence: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first download: %v", err)
	}
}
