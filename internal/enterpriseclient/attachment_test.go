package enterpriseclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
)

type wireObject struct {
	spec     attachmentSpec
	metadata model.Attachment
	data     []byte
	verified bool
}

func TestEnterpriseResourceRefusalRetainsDefiniteWireClass(t *testing.T) {
	err := wireFailure(http.StatusConflict, []byte(`{"outcome":"refused","class":"resource_limit"}`))
	var failure *Error
	if !errors.As(err, &failure) || failure.Outcome != "refused" || failure.Class != "resource_limit" {
		t.Fatalf("authoritative resource refusal became an unknown class: %v", err)
	}
}

// Controlled object receipts test client failure boundaries; these maps are
// not evidence of SQL lifecycle policy or production ObjectStore durability.
func (f *nativeWireFixture) serveObjects(w http.ResponseWriter, r *http.Request, data []byte, bound *model.EnterpriseBinding) bool {
	base := devicePath + "/rooms/room-native/attachments"
	if !strings.HasPrefix(r.URL.Path, base) {
		return false
	}
	if f.denyObjectMethod == r.Method {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization"})
		return true
	}
	if f.objects == nil {
		f.objects = map[string]*wireObject{}
		f.reservations = map[string]string{}
		f.verifications = map[string]attachmentReceipt{}
		f.objectCalls = map[string]int{}
	}
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	if r.URL.Path == base || r.URL.Path == base+"/resolve-reservation" {
		var req struct {
			ClientID string         `json:"client_id"`
			Spec     attachmentSpec `json:"spec"`
		}
		_ = json.Unmarshal(data, &req)
		resolve := strings.HasSuffix(r.URL.Path, "resolve-reservation")
		id := f.reservations[req.ClientID]
		if id == "" && resolve {
			write(attachmentReply{Outcome: "not_committed"})
			return true
		}
		if id == "" {
			id = fmt.Sprintf("att-%024x", len(f.objects)+1)
			f.reservations[req.ClientID] = id
			f.objects[id] = &wireObject{spec: req.Spec, metadata: model.Attachment{ID: id, Name: req.Spec.Name, Kind: req.Spec.Kind, Size: req.Spec.Size, SHA256: req.Spec.SHA256, Source: "upload", CreatedAt: time.Now().UTC()}}
			f.objectCalls["reserve"]++
		}
		if f.objects[id].spec != req.Spec {
			w.WriteHeader(409)
			write(attachmentReply{Outcome: "conflict", Class: "payload_conflict"})
			return true
		}
		if f.dropReserve && !resolve {
			f.dropReserve = false
			closeResponse(w)
			return true
		}
		write(attachmentReply{Outcome: "replayed", Receipt: &attachmentReceipt{Version: 1, Kind: "reserve", RoomID: "room-native", ObjectID: id, State: "reserved", Metadata: f.objects[id].metadata, ExpiresAt: time.Now().Add(30 * time.Minute)}})
		return true
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, base+"/"), "/")
	if len(parts) != 2 || f.objects[parts[0]] == nil {
		w.WriteHeader(404)
		return true
	}
	id, action := parts[0], parts[1]
	object := f.objects[id]
	if action == "content" && r.Method == http.MethodPut {
		f.objectCalls["put"]++
		if r.ContentLength != int64(len(data)) || r.ContentLength > attachment.MaxImageBytes || r.Header.Get("Content-Type") != "application/octet-stream" {
			f.t.Error("raw upload did not use fixed bounded length")
		}
		meta, err := attachment.ValidateObjectBytes(object.spec.Name, object.spec.Kind, data)
		if err != nil || !matchesObjectSpec(meta, object.spec) {
			w.WriteHeader(400)
			return true
		}
		if len(object.data) > 0 && !bytes.Equal(object.data, data) {
			f.t.Error("immutable object changed")
		}
		if !f.omitPut {
			object.data = append([]byte(nil), data...)
			meta.ID, meta.Source, meta.CreatedAt = id, object.metadata.Source, object.metadata.CreatedAt
			object.metadata = meta
		}
		if f.dropPut {
			f.dropPut = false
			closeResponse(w)
			return true
		}
		write(map[string]any{"status": "uploaded", "object_id": id, "size": int64(len(data)), "sha256": meta.SHA256})
		return true
	}
	if action == "verify" || action == "resolve-verification" {
		var req struct {
			ClientID string `json:"client_id"`
		}
		_ = json.Unmarshal(data, &req)
		f.objectCalls[action]++
		if original, ok := f.verifications[req.ClientID]; ok {
			write(attachmentReply{Outcome: "replayed", Receipt: &original})
			return true
		}
		if action == "resolve-verification" {
			write(attachmentReply{Outcome: "not_committed"})
			return true
		}
		if len(object.data) == 0 {
			w.WriteHeader(409)
			write(attachmentReply{Outcome: "not_committed", Class: "object_missing"})
			return true
		}
		object.verified = true
		receipt := attachmentReceipt{Version: 1, Kind: "verify", RoomID: "room-native", ObjectID: id, State: "verified", Metadata: object.metadata, ExpiresAt: time.Now().Add(30 * time.Minute)}
		f.verifications[req.ClientID] = receipt
		if f.dropVerify {
			f.dropVerify = false
			closeResponse(w)
			return true
		}
		write(attachmentReply{Outcome: "committed", Receipt: &receipt})
		return true
	}
	if action == "content" && r.Method == http.MethodGet {
		f.objectCalls["download"]++
		if f.downloadAction != nil {
			fn := f.downloadAction
			f.downloadAction = nil
			fn(f)
		}
		if f.corruptDownload {
			_, _ = w.Write(bytes.Repeat([]byte{'!'}, len(object.data)))
			return true
		}
		_, _ = w.Write(object.data)
		return true
	}
	w.WriteHeader(400)
	return true
}

func TestEnterpriseUploadOriginalReservationAndBytesSurviveLostResponses(t *testing.T) {
	for _, lost := range []string{"reserve", "put", "verify"} {
		t.Run(lost, func(t *testing.T) {
			f := newNativeWireFixture(t)
			e := f.endpoint(t, "upload", model.ActorSlot1, model.RuntimeClaude)
			f.approve(t, e)
			f.mu.Lock()
			f.dropReserve = lost == "reserve"
			f.dropPut = lost == "put"
			f.dropVerify = lost == "verify"
			f.mu.Unlock()
			ctx := context.Background()
			data := []byte("original explicit evidence\n")
			if _, err := e.binding.Upload(ctx, e.auth, "upload-original", "file", "evidence.txt", bytes.NewReader(data), false); err == nil {
				t.Fatal("lost response treated as confirmed")
			}
			client, err := New(e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			binding, err := client.OpenBinding(e.opts.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := binding.Upload(ctx, e.auth, "upload-original", "file", "evidence.txt", bytes.NewReader(data), false)
			if err != nil {
				t.Fatal(err)
			}
			if meta.Kind != "file" || meta.Size != int64(len(data)) {
				t.Fatal("verified metadata differs")
			}
			f.mu.Lock()
			puts, reserves := f.objectCalls["put"], f.objectCalls["reserve"]
			f.mu.Unlock()
			if puts != 1 || reserves != 1 {
				t.Fatalf("recovery resent effect or allocated new identity: puts=%d reserves=%d", puts, reserves)
			}
			if _, err := binding.Upload(ctx, e.auth, "upload-original", "file", "evidence.txt", strings.NewReader("changed"), true); err == nil {
				t.Fatal("original upload key accepted changed bytes")
			}
		})
	}
}

func TestEnterpriseAttachmentAuthorizationRefusalKeepsOriginalJournal(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		t.Run(method, func(t *testing.T) {
			f := newNativeWireFixture(t)
			e := f.endpoint(t, "deny", model.ActorSlot1, model.RuntimeClaude)
			f.approve(t, e)
			f.mu.Lock()
			f.denyObjectMethod = method
			f.mu.Unlock()
			_, err := e.binding.Upload(context.Background(), e.auth, "original-upload", "file", "evidence.txt", strings.NewReader("exact evidence"), false)
			var failure *Error
			if !errors.As(err, &failure) || failure.Outcome != "refused" || failure.Class != "authorization" {
				t.Fatalf("definite HTTP refusal was misclassified: %v", err)
			}
			path := filepath.Join(e.binding.dir, "uploads", relay.Digest("original-upload")+".content")
			data, err := readUploadBytes(path)
			if err != nil || string(data) != "exact evidence" {
				t.Fatal("refusal discarded original private recovery candidate")
			}
		})
	}
}

func TestEnterpriseUploadMissingBytesRequireExplicitRetryOfSameObject(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "missing", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	f.mu.Lock()
	f.dropPut, f.omitPut = true, true
	f.mu.Unlock()
	ctx := context.Background()
	upload := func(retry bool) (model.Attachment, error) {
		return e.binding.Upload(ctx, e.auth, "upload-key", "file", "file.txt", strings.NewReader("fixed evidence"), retry)
	}
	if _, err := upload(false); err == nil {
		t.Fatal("lost upload response accepted")
	}
	if _, err := upload(false); !errors.Is(err, ErrUploadUnresolved) {
		t.Fatalf("missing object did not require explicit retry: %v", err)
	}
	f.mu.Lock()
	if f.objectCalls["put"] != 1 {
		t.Fatal("verification retried PUT automatically")
	}
	f.omitPut = false
	f.mu.Unlock()
	if _, err := upload(true); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objectCalls["put"] != 2 || f.objectCalls["reserve"] != 1 || len(f.objects) != 1 {
		t.Fatal("explicit retry changed reservation or duplicated effects")
	}
}

func TestEnterpriseUploadSameSendKeyAcrossBindingsKeepsIndependentOperationIDs(t *testing.T) {
	f := newNativeWireFixture(t)
	a := f.endpoint(t, "first", model.ActorSlot1, model.RuntimeClaude)
	b := f.endpoint(t, "second", model.ActorSlot2, model.RuntimeClaude)
	f.approve(t, a)
	f.approve(t, b)
	first, err := a.binding.Upload(context.Background(), a.auth, "same-local-send-key", "file", "evidence.txt", strings.NewReader("same bytes"), false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.binding.Upload(context.Background(), b.auth, "same-local-send-key", "file", "evidence.txt", strings.NewReader("same bytes"), false)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatal("two Binding-local sends collided in the User operation namespace")
	}
}

func TestEnterpriseRawObjectsKeepSeparateBudgetsAndVerifiedReceiptsCompact(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "raw", model.ActorSlot1, model.RuntimeClaude)
	f.approve(t, e)
	data := bytes.Repeat([]byte{'x'}, (2<<20)+1)
	ctx := context.Background()
	if _, _, err := e.parent.request(ctx, http.MethodPost, devicePath+"/rooms/room-native/attachments", string(data), "", 64<<10); !errors.Is(err, ErrConfiguration) {
		t.Fatal("raw object extension widened JSON budget")
	}
	meta, err := e.binding.Upload(ctx, e.auth, "larger-than-json", "file", "large.txt", bytes.NewReader(data), false)
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.binding.read()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.parent.objectRequest(ctx, http.MethodPut, e.binding.objectPath(r, meta.ID)+"/content", bytes.Repeat([]byte{'x'}, (5<<20)+1), r.Grant.Token, 64<<10); !errors.Is(err, ErrConfiguration) {
		t.Fatal("oversized object accepted")
	}
	if _, _, err := e.parent.objectRequest(ctx, http.MethodGet, e.binding.objectPath(r, meta.ID)+"/content?other=room", nil, r.Grant.Token, 64<<10); !errors.Is(err, ErrConfiguration) {
		t.Fatal("object selector accepted query routing")
	}
	path := filepath.Join(e.binding.dir, "uploads", relay.Digest("larger-than-json")+".json")
	info, err := os.Stat(path)
	if err != nil || info.Size() > 4096 {
		t.Fatal("verified receipt retained large raw candidate")
	}
	if _, err := e.binding.Upload(ctx, e.auth, "larger-than-json", "file", "large.txt", bytes.NewReader(data), false); err != nil {
		t.Fatal("compact original receipt could not replay", err)
	}
}

func TestEnterpriseLargeUploadPrivateBytesSurviveClientRestart(t *testing.T) {
	for _, size := range []int{3 << 20, 5 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f := newNativeWireFixture(t)
			e := f.endpoint(t, "large-restart", model.ActorSlot1, model.RuntimeClaude)
			f.approve(t, e)
			data := bytes.Repeat([]byte{'a'}, size)
			f.mu.Lock()
			f.dropPut = true
			f.mu.Unlock()
			if _, err := e.binding.Upload(context.Background(), e.auth, "large-original", "file", "large.txt", bytes.NewReader(data), false); err == nil {
				t.Fatal("lost large PUT response accepted")
			}
			path := filepath.Join(e.binding.dir, "uploads", relay.Digest("large-original")+".content")
			retained, err := readUploadBytes(path)
			if err != nil || !bytes.Equal(retained, data) {
				t.Fatal("uncertain raw bytes were not retained in owner-private storage")
			}
			client, err := New(e.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			binding, err := client.OpenBinding(e.opts.RecordID)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := binding.Upload(context.Background(), e.auth, "large-original", "file", "large.txt", bytes.NewReader(data), false)
			if err != nil || meta.Size != int64(size) {
				t.Fatalf("large recovery: %v", err)
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("verified raw candidate was not compacted")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.objectCalls["put"] != 1 || f.objectCalls["reserve"] != 1 {
				t.Fatal("restart repeated large upload effect")
			}
		})
	}
}

func attachmentEndpoints(t *testing.T) (*nativeWireFixture, *nativeEndpoint, *nativeEndpoint, model.Attachment) {
	t.Helper()
	f := newNativeWireFixture(t)
	a := f.endpoint(t, "sender", model.ActorSlot1, model.RuntimeClaude)
	b := f.endpoint(t, "reader", model.ActorSlot2, model.RuntimeClaude)
	f.approve(t, a)
	f.approve(t, b)
	meta, err := a.binding.Upload(context.Background(), a.auth, "upload-one", "file", "review.txt", strings.NewReader("verified content\n"), false)
	if err != nil {
		t.Fatal(err)
	}
	var sent relay.Message
	if err := a.binding.Relay(context.Background(), a.auth, "send", map[string]any{"id": "send-with-evidence", "text": "Inspect this evidence.", "attachment_ids": []string{meta.ID}, "expected_peer_generation": uint64(1)}, &sent); err != nil {
		t.Fatal(err)
	}
	return f, a, b, meta
}

func TestEnterpriseHumanQuotedEvidenceUsesSharedPlannerAndCollector(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-empty-author-%v", legacy), func(t *testing.T) {
			f, _, b, meta := attachmentEndpoints(t)
			anchor := review.Anchor{Schema: 1, Workspace: "endpoint review observation", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), DirtySHA256: strings.Repeat("c", 64)}
			f.mu.Lock()
			view, err := relay.DescribeEnterpriseSnapshot(f.snapshot(), b.auth.Slot, b.auth.BindID, b.auth.Generation, time.Now().UTC())
			if err != nil || view.Next == nil {
				t.Fatal("missing source message")
			}
			source := view.Next.MessageID
			for i, intent := range []relay.EnterpriseOperationIntent{{Kind: relay.EnterpriseOperationCancel, MessageID: source}, {Kind: relay.EnterpriseOperationSend, To: b.auth.Slot, Text: "human reply with inherited evidence", QuoteID: source, Review: anchor}} {
				prepare, err := relay.PrepareEnterpriseOperation(f.snapshot(), relay.EnterpriseOperationCaller{Actor: model.ActorUser, UserID: "owner-fixture", ClientID: fmt.Sprintf("human-evidence-%d", i)}, intent, relay.EnterpriseOperationIDs{MessageID: "human-quoted"})
				if err != nil {
					t.Fatal(err)
				}
				plan, err := prepare(time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				if i == 1 && (plan.Result.Message.Author != relay.EnterpriseHumanAuthor("owner-fixture") || len(plan.Result.Message.Attachments) != 1 || plan.Result.Message.Attachments[0] != meta) {
					t.Fatal("shared human producer changed author or source evidence")
				}
				for _, fact := range plan.Facts {
					if legacy && i == 1 {
						var payload map[string]json.RawMessage
						_ = json.Unmarshal(fact.Data, &payload)
						delete(payload, "author")
						fact.Data, _ = json.Marshal(payload)
					}
					fact.ID, fact.Seq = model.NewID("event"), uint64(len(f.events)+1)
					f.events = append(f.events, fact)
				}
				f.revision++
			}
			f.mu.Unlock()
			var response struct {
				Claim *relay.Claim `json:"claim"`
			}
			if err := b.binding.Relay(t.Context(), b.auth, "wait", map[string]any{"timeout_seconds": 1}, &response); err != nil {
				t.Fatalf("collect shared human evidence: %v", err)
			}
			if response.Claim == nil || response.Claim.ID != "human-quoted" || !strings.Contains(response.Claim.Envelope, "from: @user") || !strings.Contains(response.Claim.Envelope, "Inspect this evidence.") || !strings.Contains(response.Claim.Envelope, anchor.Head) || !strings.Contains(response.Claim.Envelope, meta.Name) {
				t.Fatalf("human envelope lost identity, quote, review or evidence: %+v", response.Claim)
			}
			journal, err := b.binding.readDelivery()
			if err != nil || journal.StructuredClaim == nil || len(journal.StructuredClaim.Message.Attachments) != 1 || journal.StructuredClaim.Message.Attachments[0] != meta {
				t.Fatal("collector lost frozen immutable evidence receipt")
			}
			if strings.Contains(response.Claim.Envelope, `author: "enterprise:owner-fixture"`) == legacy {
				t.Fatal("new human attribution missing or legacy author inferred")
			}
		})
	}
}

func TestEnterpriseNativeQuoteReviewRecoveryAndHistoryAreReadOnly(t *testing.T) {
	f, a, b, meta := attachmentEndpoints(t)
	anchor := review.Anchor{Schema: 1, Workspace: "endpoint observation", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), DirtySHA256: strings.Repeat("c", 64)}
	var source relay.HistoryPage
	if err := b.binding.Relay(t.Context(), b.auth, "history", relay.HistoryQuery{Limit: 1}, &source); err != nil || len(source.Messages) != 1 {
		t.Fatalf("source history: %v", err)
	}
	request := map[string]any{"id": "quoted-reply", "text": "full reviewed reply", "quote_id": source.Messages[0].ID, "review": anchor, "expected_peer_generation": uint64(1)}
	f.mu.Lock()
	f.dropKind = relay.EnterpriseOperationSend
	f.mu.Unlock()
	var sent relay.Message
	if err := b.binding.Relay(t.Context(), b.auth, "send", request, &sent); err == nil {
		t.Fatal("lost response reported acceptance")
	}
	client, err := New(b.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	reopened, err := client.OpenBinding(b.opts.RecordID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.Relay(t.Context(), b.auth, "send", request, &sent); err != nil || sent.Quote == nil || sent.Quote.FromHandle != "@agent1" || sent.Quote.Text != source.Messages[0].Text || sent.Review == nil || *sent.Review != anchor || len(sent.Attachments) != 1 || sent.Attachments[0] != meta {
		t.Fatalf("restarted original quote recovery: %v", err)
	}
	f.mu.Lock()
	posts, seq := f.posts, len(f.events)
	f.mu.Unlock()
	var page relay.HistoryPage
	if err := reopened.Relay(t.Context(), b.auth, "history", relay.HistoryQuery{ID: sent.ID}, &page); err != nil || len(page.Messages) != 1 || page.Messages[0].Review == nil || *page.Messages[0].Review != anchor {
		t.Fatalf("history evidence: %v", err)
	}
	if err := reopened.Relay(t.Context(), b.auth, "history", relay.HistoryQuery{Cursor: "pending:0"}, &page); err == nil {
		t.Fatal("foreign view cursor accepted")
	}
	f.mu.Lock()
	if f.posts != posts || len(f.events) != seq {
		t.Fatal("history/recovery repeated an effect")
	}
	f.mu.Unlock()
	var response struct {
		Claim *relay.Claim `json:"claim"`
	}
	if err := a.binding.Relay(t.Context(), a.auth, "wait", map[string]any{"timeout_seconds": 1}, &response); err != nil || response.Claim == nil || !strings.Contains(response.Claim.Envelope, anchor.Head) || !strings.Contains(response.Claim.Envelope, source.Messages[0].Text) {
		t.Fatalf("native quoted collector: %v", err)
	}
}

func TestEnterpriseAttachmentPrefetchBeforeExactClaimAndLocalEnvelope(t *testing.T) {
	f, _, b, meta := attachmentEndpoints(t)
	ctx := context.Background()
	f.mu.Lock()
	postsBefore := f.posts
	f.downloadAction = func(f *nativeWireFixture) {
		if f.posts != postsBefore {
			t.Error("claim started before all evidence downloads completed")
		}
	}
	f.mu.Unlock()
	var response struct {
		Claim *relay.Claim `json:"claim"`
	}
	if err := b.binding.Relay(ctx, b.auth, "wait", map[string]any{"timeout_seconds": 1}, &response); err != nil {
		t.Fatal(err)
	}
	if response.Claim == nil || !strings.Contains(response.Claim.Envelope, "Inspect this evidence.") || !strings.Contains(response.Claim.Envelope, "review.txt") || !strings.Contains(response.Claim.Envelope, strings.Trim(fmt.Sprintf("%q", b.binding.dir), "\"")) {
		t.Fatal("structured claim omitted local verified evidence")
	}
	r, err := b.binding.read()
	if err != nil {
		t.Fatal(err)
	}
	path, err := b.binding.cachedAttachment(ctx, r, meta)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "verified content\n" {
		t.Fatal("cache bytes differ")
	}
	journal, err := b.binding.readDelivery()
	if err != nil || journal.StructuredClaim == nil || journal.ExpectedMessageID != response.Claim.ID || journal.ExpectedTargetGeneration != 1 {
		t.Fatalf("durable original structured receipt missing: %v", err)
	}
	var ack any
	if err := b.binding.Relay(ctx, b.auth, "ack", map[string]string{"id": response.Claim.ID, "receipt": response.Claim.Receipt}, &ack); err != nil {
		t.Fatal(err)
	}
}

func TestEnterpriseCorruptDownloadDoesNotClaim(t *testing.T) {
	f, _, b, _ := attachmentEndpoints(t)
	f.mu.Lock()
	f.corruptDownload = true
	posts := f.posts
	f.mu.Unlock()
	var response struct {
		Claim *relay.Claim `json:"claim"`
	}
	if err := b.binding.Relay(context.Background(), b.auth, "wait", map[string]any{"timeout_seconds": 1}, &response); err == nil {
		t.Fatal("corrupt evidence accepted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posts != posts || response.Claim != nil {
		t.Fatal("failed prefetch consumed inbox")
	}
}

func TestEnterpriseChangedHeadCannotClaimUnprefetchedSuccessor(t *testing.T) {
	f, _, b, _ := attachmentEndpoints(t)
	f.mu.Lock()
	f.downloadAction = func(f *nativeWireFixture) {
		view, err := relay.DescribeEnterpriseSnapshot(f.snapshot(), b.auth.Slot, b.auth.BindID, b.auth.Generation, time.Now().UTC())
		if err != nil || view.Next == nil {
			t.Fatal("missing original head")
		}
		for i, intent := range []relay.EnterpriseOperationIntent{{Kind: relay.EnterpriseOperationCancel, MessageID: view.Next.MessageID}, {Kind: relay.EnterpriseOperationSend, To: b.auth.Slot, Text: "successor was never prefetched", ExpectedPeerGeneration: 1}} {
			prepare, err := relay.PrepareEnterpriseOperation(f.snapshot(), relay.EnterpriseOperationCaller{Actor: model.ActorUser, UserID: "owner-fixture", ClientID: fmt.Sprintf("change-%d", i)}, intent, relay.EnterpriseOperationIDs{MessageID: "successor", Receipt: "unused"})
			if err != nil {
				t.Fatal(err)
			}
			plan, err := prepare(time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range plan.Facts {
				event.ID = model.NewID("event")
				event.Seq = uint64(len(f.events) + 1)
				f.events = append(f.events, event)
			}
			f.revision++
		}
	}
	f.mu.Unlock()
	var response struct {
		Claim *relay.Claim `json:"claim"`
	}
	err := b.binding.Relay(context.Background(), b.auth, "wait", map[string]any{"timeout_seconds": 1}, &response)
	var failure *Error
	if !errors.As(err, &failure) || failure.Class != "prepared_head_changed" || response.Claim != nil {
		t.Fatalf("changed head was not refused: %v", err)
	}
	journal, err := b.binding.readDelivery()
	if err != nil || journal.Phase != "claim_absent" {
		t.Fatal("definite zero-effect head conflict became Unknown")
	}
}

func TestEnterpriseLostStructuredClaimReconciliationNeverRendersAgain(t *testing.T) {
	f, _, b, _ := attachmentEndpoints(t)
	f.mu.Lock()
	f.dropKind = relay.EnterpriseOperationClaim
	f.mu.Unlock()
	var response struct {
		Claim *relay.Claim `json:"claim"`
	}
	if err := b.binding.Relay(context.Background(), b.auth, "wait", map[string]any{"timeout_seconds": 1}, &response); err == nil {
		t.Fatal("lost claim response accepted")
	}
	ops, err := b.binding.ReconcileDelivery(context.Background(), b.auth, false)
	if err != nil || len(ops) != 1 || ops[0].MessageID == "" {
		t.Fatalf("reconcile: %v", err)
	}
	encoded, _ := json.Marshal(ops)
	if bytes.Contains(encoded, []byte("Inspect this evidence")) || bytes.Contains(encoded, []byte(b.binding.dir)) {
		t.Fatal("reconciliation returned old envelope or cache path")
	}
	files, err := os.ReadDir(filepath.Join(b.binding.dir, "uncertain-deliveries"))
	if err != nil || len(files) != 1 {
		t.Fatal("original structured receipt not retained privately")
	}
}
