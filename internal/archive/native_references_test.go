package archive

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

// Native Rooms reference attachments from native.message.updated facts and
// from messages embedded in routed Stop publications, not message.created.
func TestVerifyCountsNativeMessageAttachmentReferences(t *testing.T) {
	dataDir := t.TempDir()
	media, err := attachment.Open(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	sent, err := media.SaveImage("sent.png", bytes.NewReader(encoded.Bytes()), "native-relay")
	if err != nil {
		t.Fatal(err)
	}
	published, err := media.SaveImage("published.png", bytes.NewReader(encoded.Bytes()), "native-relay")
	if err != nil {
		t.Fatal(err)
	}
	unsent, err := media.SaveImage("unsent.png", bytes.NewReader(encoded.Bytes()), "native-relay")
	if err != nil {
		t.Fatal(err)
	}
	log, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, fact := range []struct {
		kind string
		data any
	}{
		{"room.created", model.RoomMeta{ID: "room-native", Name: "Native", CreatedAt: now}},
		{"native.message.updated", map[string]any{"id": "relay-1", "from": "user", "to": "slot1", "state": "queued", "attachments": []model.Attachment{sent}}},
		{"native.publication", map[string]any{"bind_id": "bind", "generation": 1, "report_seq": 1, "message": map[string]any{"id": "relay-2", "from": "slot1", "to": "slot2", "state": "queued", "attachments": []model.Attachment{published}}}},
	} {
		event, err := model.NewEvent("room-native", fact.kind, model.ActorSystem, fact.data)
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(&event); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	report := Verify(dataDir)
	if !report.OK || report.ReferencedAttachments != 2 {
		t.Fatalf("report: %#v", report)
	}
	if len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], unsent.ID) {
		t.Fatalf("warnings=%v, want only the unsent upload", report.Warnings)
	}
}
