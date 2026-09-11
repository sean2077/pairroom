package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
)

func TestNativeAttachmentIdentityAndQuotedImages(t *testing.T) {
	e, auth, dir := testEngine(t)
	media, err := attachment.Open(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.cfg.Media = media
	image, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	saved, err := media.SaveImage("diagram.png", bytes.NewReader(image), "upload")
	if err != nil {
		t.Fatal(err)
	}
	original, err := e.Send(auth[model.ActorClaude], SendRequest{ID: "image-source", Text: "image source", AttachmentIDs: []string{saved.ID}})
	if err != nil {
		t.Fatal(err)
	}
	quoted, err := e.SendUser(SendRequest{ID: "quote-image", To: model.ActorClaude, Text: "inspect it", QuoteID: original.ID, AttachmentIDs: []string{saved.ID}})
	if err != nil || len(quoted.Attachments) != 1 || quoted.Quote.Text != original.Text {
		t.Fatalf("quote: %+v %v", quoted, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	claim, err := e.Claim(ctx, auth[model.ActorClaude], false)
	if err != nil || !strings.Contains(claim.Envelope, "diagram.png") || !strings.Contains(claim.Envelope, "quoted_message:") {
		t.Fatalf("lost quoted media: %+v %v", claim, err)
	}
	if err := e.Ack(auth[model.ActorClaude], claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	// Replacing BOTH bytes and their manifest can pass Store.Resolve but must
	// not replace the content pinned in an already accepted native message.
	replacement := append(append([]byte(nil), image...), []byte("changed bytes")...)
	next, err := media.SaveImage("other.png", bytes.NewReader(replacement), "upload")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "attachments", saved.ID+".json")
	data, _ := os.ReadFile(path)
	var manifest struct {
		Attachment model.Attachment `json:"attachment"`
		Filename   string           `json:"filename"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Attachment.SHA256 = next.SHA256
	manifest.Attachment.Size = next.Size
	if err := os.WriteFile(filepath.Join(dir, "attachments", manifest.Filename), replacement, 0600); err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(manifest)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := media.Resolve(saved.ID); err != nil {
		t.Fatalf("fixture must pass manifest verification: %v", err)
	}
	before := e.Snapshot().Sequence
	if claim, err := e.Claim(ctx, auth[model.ActorCodex], false); err == nil || claim != nil {
		t.Fatal("accepted image was substituted")
	}
	if e.Snapshot().Sequence != before || e.Snapshot().Messages[0].State != "queued" {
		t.Fatal("bad media fabricated a delivery")
	}
	if _, err := e.SendUser(SendRequest{ID: "bad-quote", To: model.ActorClaude, Text: "changed", QuoteID: original.ID}); err == nil {
		t.Fatal("quote substituted accepted image")
	}
}
