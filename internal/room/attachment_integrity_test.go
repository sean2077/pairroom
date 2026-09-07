package room

import (
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestAgentAttachmentsPreserveAcceptedContentIdentity(t *testing.T) {
	accepted := model.Attachment{ID: "att-0123456789abcdef01234567", Kind: "image", MediaType: "image/png", SHA256: strings.Repeat("a", 64), Size: 68}
	media := &fakeAttachmentStore{metadata: map[string]model.Attachment{accepted.ID: accepted}, paths: map[string]string{accepted.ID: "/private/image.png"}}
	engine := &Engine{cfg: Config{Attachments: media}}
	if _, err := engine.agentAttachments([]model.Attachment{accepted}); err != nil {
		t.Fatalf("unchanged accepted image rejected: %v", err)
	}
	// Resolve can successfully validate a replacement file against a replacement
	// manifest. That does not make it the image already accepted in the Event Log.
	replacement := accepted
	replacement.SHA256 = strings.Repeat("b", 64)
	media.metadata[accepted.ID] = replacement
	if _, err := engine.agentAttachments([]model.Attachment{accepted}); err == nil {
		t.Fatal("native input silently substituted different bytes for the accepted image")
	}
	// Historical metadata without a digest remains readable; the attachment
	// store and adapter still validate the current stored content at their bounds.
	legacy := accepted
	legacy.SHA256 = ""
	if _, err := engine.agentAttachments([]model.Attachment{legacy}); err != nil {
		t.Fatalf("legacy digest-free attachment rejected: %v", err)
	}
}
