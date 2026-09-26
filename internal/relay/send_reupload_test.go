package relay

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
)

// A same-ID CLI retry re-uploads each image under a fresh attachment ID. The
// original receipt is returned only for identical delivered content; any other
// attachment change stays a conflict and never publishes a second message.
func TestNativeSameIDRetryAcceptsReuploadedIdenticalImage(t *testing.T) {
	e, auth, dir := testEngine(t)
	media, err := attachment.Open(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Media = media
	a := auth[model.ActorSlot1]
	image, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	upload := func(name string, data []byte) string {
		t.Helper()
		saved, err := media.SaveImage(name, bytes.NewReader(data), "native-relay")
		if err != nil {
			t.Fatal(err)
		}
		return saved.ID
	}
	first := upload("diagram.png", image)
	original, err := e.Send(a, SendRequest{ID: "with-image", Text: "see image", AttachmentIDs: []string{first}})
	if err != nil {
		t.Fatal(err)
	}
	before := e.Sequence()
	again := upload("diagram.png", image)
	if again == first {
		t.Fatal("fixture must model a fresh upload identity")
	}
	got, err := e.Send(a, SendRequest{ID: "with-image", Text: "see image", AttachmentIDs: []string{again}})
	if err != nil || got.ID != original.ID || len(got.Attachments) != 1 || got.Attachments[0].ID != first {
		t.Fatalf("identical re-upload did not return the original receipt: %+v %v", got, err)
	}
	// A PNG with trailing bytes decodes to the same dimensions but is different content.
	changed := append(append([]byte(nil), image...), []byte("changed")...)
	for name, ids := range map[string][]string{
		"different bytes": {upload("diagram.png", changed)},
		"different name":  {upload("other.png", image)},
		"extra image":     {again, upload("diagram.png", image)},
		"missing image":   {},
		"unknown id":      {"att-000000000000000000000000"},
	} {
		req := SendRequest{ID: "with-image", Text: "see image", AttachmentIDs: ids}
		if _, err := e.Send(a, req); !errors.Is(err, ErrSendPayloadConflict) {
			t.Fatalf("%s: same ID accepted different attachments: %v", name, err)
		}
	}
	if e.Sequence() != before || len(e.Snapshot().Messages) != 1 {
		t.Fatal("same-ID retry appended a publication")
	}
}
