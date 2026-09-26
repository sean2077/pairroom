package room

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
)

func saveEmbeddedImage(t *testing.T, media *attachment.Store) model.Attachment {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := media.SaveImage("composer.png", bytes.NewReader(data), "user-upload")
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestEmbeddedReclaimRemovesOnlyOldUnreferencedUploads(t *testing.T) {
	media, err := attachment.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := newAttachmentEngine(t, media)
	sent := saveEmbeddedImage(t, media)
	message, err := engine.Send(context.Background(), SendRequest{Text: "sent", To: []model.ActorID{model.ActorSlot1}, Attachments: []model.Attachment{{ID: sent.ID}}, Intent: model.IntentQueue})
	if err != nil {
		t.Fatal(err)
	}
	// A retry copies the original's attachments into a new message; cancel
	// the original first so it is retryable.
	if err := engine.CancelMessage(context.Background(), message.ID, model.ActorSlot1); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Retry(context.Background(), message.ID, RetryRequest{}); err != nil {
		t.Fatal(err)
	}
	abandoned := saveEmbeddedImage(t, media)

	if removed, err := engine.ReclaimAttachments(); err != nil || removed != 0 {
		t.Fatalf("young upload reclaimed: removed=%d err=%v", removed, err)
	}
	removed, err := engine.reclaimAttachmentsBefore(time.Now().Add(time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("reclaim: removed=%d err=%v", removed, err)
	}
	if _, _, err := media.Resolve(abandoned.ID); !errors.Is(err, attachment.ErrUnknown) {
		t.Fatalf("abandoned upload survived: %v", err)
	}
	if _, _, err := media.Resolve(sent.ID); err != nil {
		t.Fatalf("transcript attachment removed: %v", err)
	}
	// A composer that still holds the reclaimed ID fails before persistence.
	before := len(engine.Snapshot().Messages)
	if _, err := engine.Send(context.Background(), SendRequest{Text: "late", To: []model.ActorID{model.ActorSlot1}, Attachments: []model.Attachment{{ID: abandoned.ID}}}); err == nil {
		t.Fatal("reclaimed attachment entered the transcript")
	}
	if len(engine.Snapshot().Messages) != before {
		t.Fatal("failed send created a message")
	}
}

func TestEmbeddedReclaimRechecksUnderRoutingGate(t *testing.T) {
	media, err := attachment.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	engine, _ := newAttachmentEngine(t, media)
	raced := saveEmbeddedImage(t, media)
	candidates, err := media.ReclaimCandidates(time.Now().Add(time.Hour))
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%v err=%v", candidates, err)
	}
	// The send commits after the pass listed its candidates.
	if _, err := engine.Send(context.Background(), SendRequest{Text: "late", To: []model.ActorID{model.ActorSlot1}, Attachments: []model.Attachment{{ID: raced.ID}}, Intent: model.IntentQueue}); err != nil {
		t.Fatal(err)
	}
	if removed, err := engine.discardUnreferenced(media, candidates); err != nil || removed != 0 {
		t.Fatalf("referenced upload removed: removed=%d err=%v", removed, err)
	}
	if _, _, err := media.Resolve(raced.ID); err != nil {
		t.Fatalf("referenced upload lost: %v", err)
	}
}

func TestEmbeddedReclaimSkipsStoresWithoutListing(t *testing.T) {
	image := model.Attachment{ID: "att-0123456789abcdef01234567", Kind: "image", MediaType: "image/png", Size: 68}
	media := &fakeAttachmentStore{metadata: map[string]model.Attachment{image.ID: image}}
	engine, _ := newAttachmentEngine(t, media)
	if removed, err := engine.reclaimAttachmentsBefore(time.Now().Add(time.Hour)); err != nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, ok := media.metadata[image.ID]; !ok {
		t.Fatal("store without listing lost an attachment")
	}
}
