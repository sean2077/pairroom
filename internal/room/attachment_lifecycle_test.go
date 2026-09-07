package room

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

type guardedAttachmentStore struct {
	fakeAttachmentStore
	engine    *Engine
	once      sync.Once
	unguarded atomic.Bool
}

func (s *guardedAttachmentStore) Resolve(id string) (model.Attachment, string, error) {
	// Send must already own the same gate that protects removal. This directly
	// tests the race window, without depending on a goroutine winning a sleep.
	s.once.Do(func() {
		if s.engine.routingMu.TryLock() {
			s.unguarded.Store(true)
			s.engine.routingMu.Unlock()
		}
	})
	return s.fakeAttachmentStore.Resolve(id)
}

func TestAttachmentRemovalSharesSubmissionGate(t *testing.T) {
	image := model.Attachment{ID: "att-0123456789abcdef01234567", Kind: "image", MediaType: "image/png", Size: 68}
	media := &guardedAttachmentStore{fakeAttachmentStore: fakeAttachmentStore{metadata: map[string]model.Attachment{image.ID: image}}}
	engine, _ := newAttachmentEngine(t, media)
	media.engine = engine
	// The first Resolve is canonicalization; later native-path reads are not
	// expected to hold the routing gate.
	_, err := engine.Send(context.Background(), SendRequest{Text: "image", To: []model.ActorID{model.ActorClaude}, Attachments: []model.Attachment{image}, Intent: model.IntentQueue})
	if err != nil {
		t.Fatal(err)
	}
	if media.unguarded.Load() {
		t.Fatal("attachment resolved outside the removal/submission gate")
	}
	if err := engine.RemoveAttachment(image.ID); !errors.Is(err, ErrAttachmentReferenced) {
		t.Fatalf("referenced image removed: %v", err)
	}
	if _, ok := media.metadata[image.ID]; !ok {
		t.Fatal("referenced bytes were removed")
	}
}

func TestRemovedAttachmentCannotEnterTranscript(t *testing.T) {
	image := model.Attachment{ID: "att-0123456789abcdef01234567", Kind: "image", MediaType: "image/png", Size: 68}
	media := &fakeAttachmentStore{metadata: map[string]model.Attachment{image.ID: image}}
	engine, _ := newAttachmentEngine(t, media)
	if err := engine.RemoveAttachment(image.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Send(context.Background(), SendRequest{To: []model.ActorID{model.ActorClaude}, Attachments: []model.Attachment{image}}); err == nil {
		t.Fatal("deleted attachment accepted")
	}
	if engine.AttachmentReferenced(image.ID) {
		t.Fatal("failed submission created a reference")
	}
}
