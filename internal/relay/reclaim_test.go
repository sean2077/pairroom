package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/archive"
	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/review"
	"github.com/sean2077/pairroom/internal/store"
)

// reclaimClock lets a test move the Engine past the grace period without
// sleeping. Uploads keep their real creation times.
type reclaimClock struct{ offset atomic.Int64 }

func (c *reclaimClock) now() time.Time {
	return time.Now().UTC().Add(time.Duration(c.offset.Load()))
}
func (c *reclaimClock) advance(d time.Duration) { c.offset.Store(int64(d)) }

func reclaimEngine(t *testing.T) (*Engine, map[model.ActorID]Auth, string, *attachment.Store, *reclaimClock) {
	t.Helper()
	e, auth, dir := testEngine(t)
	media, err := attachment.Open(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := &reclaimClock{}
	e.mu.Lock()
	e.cfg.Media = media
	e.cfg.Now = clock.now
	e.cfg.Lease = time.Hour
	e.mu.Unlock()
	return e, auth, dir, media, clock
}

func saveTestImage(t *testing.T, media *attachment.Store, name string) model.Attachment {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := media.SaveImage(name, bytes.NewReader(data), "native-relay")
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestReclaimKeepsEveryReferencedAttachmentAndRemovesOnlyOldUnreferenced(t *testing.T) {
	e, auth, dir, media, clock := reclaimEngine(t)
	slot1, slot2 := auth[model.ActorSlot1], auth[model.ActorSlot2]
	image := func(name string) string { return saveTestImage(t, media, name).ID }
	referenced := map[string]string{}

	// handed_off: an explicit peer send that slot2 collected.
	referenced["handed_off"] = image("handed.png")
	if _, err := e.Send(slot1, SendRequest{ID: "handed", Text: "handed", AttachmentIDs: []string{referenced["handed_off"]}}); err != nil {
		t.Fatal(err)
	}
	if claim, err := e.Claim(context.Background(), slot2, false); err != nil || claim == nil {
		t.Fatalf("claim: %+v %v", claim, err)
	} else if err := e.Ack(slot2, claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	// queued: a user message still waiting in slot1's inbox.
	referenced["queued"] = image("queued.png")
	if _, err := e.SendUser(SendRequest{ID: "queued", To: model.ActorSlot1, Text: "queued for slot1", AttachmentIDs: []string{referenced["queued"]}}); err != nil {
		t.Fatal(err)
	}
	// @user escalation is a "human" message with no inbox state.
	referenced["human"] = image("human.png")
	if _, err := e.Send(slot2, SendRequest{ID: "human", To: model.ActorUser, Text: "for the user", AttachmentIDs: []string{referenced["human"]}}); err != nil {
		t.Fatal(err)
	}
	// cancelled before collection
	referenced["cancelled"] = image("cancelled.png")
	cancelled, err := e.SendUser(SendRequest{ID: "cancelled", To: model.ActorSlot2, Text: "cancel me", AttachmentIDs: []string{referenced["cancelled"]}})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(cancelled.ID); err != nil {
		t.Fatal(err)
	}
	// unknown after an expired lease, then an explicit Retry that copies it.
	referenced["unknown"] = image("unknown.png")
	unknown, err := e.SendUser(SendRequest{ID: "unknown", To: model.ActorSlot2, Text: "lost delivery", AttachmentIDs: []string{referenced["unknown"]}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := e.Claim(context.Background(), slot2, false)
	if err != nil || claim == nil || claim.ID != unknown.ID {
		t.Fatalf("claim unknown: %+v %v", claim, err)
	}
	clock.advance(2 * time.Hour)
	if err := e.Reap(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Retry(unknown.ID); err != nil {
		t.Fatal(err)
	}
	// A quote merges the quoted message's images into the quoting message.
	referenced["quote"] = image("quote.png")
	if _, err := e.SendUser(SendRequest{ID: "quote", To: model.ActorSlot1, Text: "see both", QuoteID: claim.ID, AttachmentIDs: []string{referenced["quote"]}}); err != nil {
		t.Fatal(err)
	}
	// Review evidence travels with an attachment on the same message.
	referenced["review"] = image("review.png")
	if _, err := e.Send(slot1, SendRequest{ID: "review", Text: "reviewed", AttachmentIDs: []string{referenced["review"]}, Review: reviewAnchorForTest()}); err != nil {
		t.Fatal(err)
	}
	// Revoking a generation cancels its queued inbox work but keeps the facts.
	referenced["revoked"] = image("revoked.png")
	if _, err := e.SendUser(SendRequest{ID: "revoked", To: model.ActorSlot2, Text: "revoked", AttachmentIDs: []string{referenced["revoked"]}}); err != nil {
		t.Fatal(err)
	}
	if err := e.Unbind(model.ActorSlot2); err != nil {
		t.Fatal(err)
	}

	abandoned := image("abandoned.png")
	clock.advance(attachment.ReclaimGrace - time.Hour)
	if removed, err := e.ReclaimAttachments(); err != nil || removed != 0 {
		t.Fatalf("young upload reclaimed: removed=%d err=%v", removed, err)
	}
	if _, _, err := media.Resolve(abandoned); err != nil {
		t.Fatalf("young upload removed: %v", err)
	}

	// Replay proves the references are durable facts, not live-only state.
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Config{RoomID: "room", Store: log, Runtimes: e.cfg.Runtimes, Media: media, Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	clock.advance(attachment.ReclaimGrace + time.Hour)
	before := reopened.Snapshot().Sequence
	removed, err := reopened.ReclaimAttachments()
	if err != nil || removed != 1 {
		t.Fatalf("reclaim: removed=%d err=%v", removed, err)
	}
	if reopened.Snapshot().Sequence != before {
		t.Fatal("reclamation appended to the Event Log")
	}
	if _, _, err := media.Resolve(abandoned); !errors.Is(err, attachment.ErrUnknown) {
		t.Fatalf("old unreferenced upload survived: %v", err)
	}
	for kind, id := range referenced {
		if _, _, err := media.Resolve(id); err != nil {
			t.Fatalf("%s reference lost its attachment: %v", kind, err)
		}
	}
	if report := archive.Verify(dir); !report.OK || len(report.Warnings) != 0 {
		t.Fatalf("backup verification after reclamation: errors=%v warnings=%v", report.Errors, report.Warnings)
	}
}

func reviewAnchorForTest() *review.Anchor {
	return &review.Anchor{Schema: 1, Workspace: "/project", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), DirtySHA256: strings.Repeat("c", 64)}
}

func TestReclaimBoundsOnePass(t *testing.T) {
	e, _, _, media, clock := reclaimEngine(t)
	for i := 0; i < attachment.ReclaimBatch+3; i++ {
		saveTestImage(t, media, "draft.png")
	}
	clock.advance(attachment.ReclaimGrace + time.Hour)
	if removed, err := e.ReclaimAttachments(); err != nil || removed != attachment.ReclaimBatch {
		t.Fatalf("first pass removed=%d err=%v", removed, err)
	}
	if removed, err := e.ReclaimAttachments(); err != nil || removed != 3 {
		t.Fatalf("second pass removed=%d err=%v", removed, err)
	}
}

func TestReclaimedDraftRetryFailsWithoutPublishing(t *testing.T) {
	e, _, _, media, clock := reclaimEngine(t)
	draft := saveTestImage(t, media, "draft.png")
	clock.advance(attachment.ReclaimGrace + time.Hour)
	if removed, err := e.ReclaimAttachments(); err != nil || removed != 1 {
		t.Fatalf("reclaim: removed=%d err=%v", removed, err)
	}
	before := e.Snapshot()
	// The browser outbox resends its saved payload under the same client ID.
	for i := 0; i < 2; i++ {
		_, err := e.SendUser(SendRequest{ID: "stale-draft", To: model.ActorSlot1, Text: "late retry", AttachmentIDs: []string{draft.ID}})
		if !errors.Is(err, attachment.ErrUnknown) {
			t.Fatalf("stale draft retry error = %v", err)
		}
	}
	after := e.Snapshot()
	if after.Sequence != before.Sequence || len(after.Messages) != len(before.Messages) {
		t.Fatal("a draft with a reclaimed attachment published a message")
	}
}

func TestReclaimNeverRemovesAnAttachmentAConcurrentSendPublished(t *testing.T) {
	e, auth, _, media, clock := reclaimEngine(t)
	const rounds = 24
	ids := make([]string, rounds)
	for i := range ids {
		ids[i] = saveTestImage(t, media, "race.png").ID
	}
	clock.advance(attachment.ReclaimGrace + time.Hour)
	var published []string
	var mu sync.Mutex
	for i, id := range ids {
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			m, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "race-" + id, Text: "race", AttachmentIDs: []string{id}})
			if err == nil {
				mu.Lock()
				published = append(published, m.ID)
				mu.Unlock()
			} else if !errors.Is(err, attachment.ErrUnknown) {
				t.Errorf("round %d: unexpected send error %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.ReclaimAttachments(); err != nil {
				t.Errorf("round %d: reclaim: %v", i, err)
			}
		}()
		close(start)
		wg.Wait()
	}
	// Every published message still carries resolvable bytes, so each one
	// remains collectable with its accepted image.
	snapshot := e.Snapshot()
	byID := map[string]Message{}
	for _, m := range snapshot.Messages {
		byID[m.ID] = m
	}
	for _, id := range published {
		for _, a := range byID[id].Attachments {
			if _, _, err := media.Resolve(a.ID); err != nil {
				t.Fatalf("published message %s lost attachment: %v", id, err)
			}
		}
	}
	for range published {
		claim, err := e.Claim(context.Background(), auth[model.ActorSlot2], false)
		if err != nil || claim == nil {
			t.Fatalf("published message is not collectable: %+v %v", claim, err)
		}
		if err := e.Ack(auth[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReclaimRefusesAfterFatalStoreFailure(t *testing.T) {
	e, _, _, media, clock := reclaimEngine(t)
	draft := saveTestImage(t, media, "draft.png")
	clock.advance(attachment.ReclaimGrace + time.Hour)
	e.mu.Lock()
	e.fatal = errors.New("simulated append failure")
	e.mu.Unlock()
	if removed, err := e.ReclaimAttachments(); err == nil || removed != 0 {
		t.Fatalf("reclaim after fatal: removed=%d err=%v", removed, err)
	}
	if _, _, err := media.Resolve(draft.ID); err != nil {
		t.Fatalf("attachment removed without a trustworthy projection: %v", err)
	}
}

// The candidate list is read outside the admission lock. A send that commits
// its reference after listing must still win over the removal.
func TestReclaimRechecksReferencesAfterListing(t *testing.T) {
	e, auth, _, media, clock := reclaimEngine(t)
	raced := saveTestImage(t, media, "raced.png")
	clock.advance(attachment.ReclaimGrace + time.Hour)
	candidates, err := media.ReclaimCandidates(clock.now().Add(-attachment.ReclaimGrace))
	if err != nil || len(candidates) != 1 || candidates[0] != raced.ID {
		t.Fatalf("candidates=%v err=%v", candidates, err)
	}
	if _, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "late", Text: "late send", AttachmentIDs: []string{raced.ID}}); err != nil {
		t.Fatal(err)
	}
	if removed, err := e.discardUnreferenced(candidates); err != nil || removed != 0 {
		t.Fatalf("stale candidate removed: removed=%d err=%v", removed, err)
	}
	claim, err := e.Claim(context.Background(), auth[model.ActorSlot2], false)
	if err != nil || claim == nil {
		t.Fatalf("published message lost its attachment: %+v %v", claim, err)
	}
}
