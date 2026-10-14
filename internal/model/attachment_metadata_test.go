package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAttachmentMetadataPreservesInstantAndEveryImmutableField(t *testing.T) {
	at := time.Date(2026, 10, 13, 10, 20, 30, 123456000, time.UTC)
	base := Attachment{ID: "object", Name: "evidence.txt", MediaType: "text/plain", Kind: "file", Size: 123, SHA256: "digest", Width: 1, Height: 2, Source: "upload", CreatedAt: at}
	for _, zone := range []*time.Location{time.FixedZone("UTC SQL representation", 0), time.FixedZone("Singapore", 8*60*60)} {
		other := base
		other.CreatedAt = at.In(zone)
		if base == other || !base.EqualMetadata(other) || !other.EqualMetadata(base) {
			t.Fatal("equal persisted instants depended on time.Location identity")
		}
		encoded, err := json.Marshal(other)
		if err != nil {
			t.Fatal(err)
		}
		var decoded Attachment
		if err := json.Unmarshal(encoded, &decoded); err != nil || !base.EqualMetadata(decoded) {
			t.Fatal("JSON roundtrip changed immutable metadata")
		}
	}
	for name, mutate := range map[string]func(*Attachment){
		"id":         func(a *Attachment) { a.ID += "changed" },
		"name":       func(a *Attachment) { a.Name += "changed" },
		"media_type": func(a *Attachment) { a.MediaType += "changed" },
		"kind":       func(a *Attachment) { a.Kind += "changed" },
		"size":       func(a *Attachment) { a.Size++ },
		"digest":     func(a *Attachment) { a.SHA256 += "changed" },
		"width":      func(a *Attachment) { a.Width++ },
		"height":     func(a *Attachment) { a.Height++ },
		"source":     func(a *Attachment) { a.Source += "changed" },
		"instant":    func(a *Attachment) { a.CreatedAt = a.CreatedAt.Add(time.Nanosecond) },
	} {
		t.Run(name, func(t *testing.T) {
			other := base
			mutate(&other)
			if base.EqualMetadata(other) || other.EqualMetadata(base) {
				t.Fatal("changed immutable metadata compared equal")
			}
		})
	}
}
