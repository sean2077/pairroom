package attachment

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReclaimCandidatesHonorsCutoffAndLeftoverContent(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.SaveImage("draft.png", bytes.NewReader(pngBytes(t)), "upload")
	if err != nil {
		t.Fatal(err)
	}
	// A content file whose manifest is gone (an interrupted Remove) and a
	// non-attachment file must not be confused with each other.
	leftover := "att-0123456789abcdef01234567"
	if err := os.WriteFile(filepath.Join(store.Root(), leftover+".png"), pngBytes(t), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Root(), "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	young, err := store.ReclaimCandidates(time.Now().Add(-ReclaimGrace))
	if err != nil || len(young) != 0 {
		t.Fatalf("young uploads offered for reclamation: %v %v", young, err)
	}
	old, err := store.ReclaimCandidates(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{saved.ID: true, leftover: true}
	if len(old) != len(want) || !want[old[0]] || !want[old[1]] {
		t.Fatalf("candidates=%v, want %v", old, want)
	}
}

func TestDiscardRemovesManifestAndContentOnly(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	gone, err := store.SaveImage("gone.png", bytes.NewReader(pngBytes(t)), "upload")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.SaveImage("kept.png", bytes.NewReader(pngBytes(t)), "upload")
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.Discard(gone.ID)
	if err != nil || !removed {
		t.Fatalf("discard: removed=%v err=%v", removed, err)
	}
	if _, _, err := store.Resolve(gone.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("discarded attachment still resolves: %v", err)
	}
	for _, name := range []string{gone.ID + ".json", gone.ID + ".png"} {
		if _, err := os.Lstat(filepath.Join(store.Root(), name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived discard: %v", name, err)
		}
	}
	if _, _, err := store.Resolve(kept.ID); err != nil {
		t.Fatalf("unrelated attachment damaged: %v", err)
	}
	again, err := store.Discard(gone.ID)
	if err != nil || again {
		t.Fatalf("second discard: removed=%v err=%v", again, err)
	}
	if _, err := store.Discard("../events"); err == nil {
		t.Fatal("invalid id accepted")
	}
}

func TestDiscardRefusesManifestNamingAnotherUpload(t *testing.T) {
	dataDir := t.TempDir()
	store, err := Open(dataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	victim, err := store.SaveImage("victim.png", bytes.NewReader(pngBytes(t)), "upload")
	if err != nil {
		t.Fatal(err)
	}
	forged := "att-0123456789abcdef01234567"
	data := []byte(`{"attachment":{"id":"` + forged + `","kind":"image","media_type":"image/png"},"filename":"` + victim.ID + `.png"}`)
	if err := os.WriteFile(filepath.Join(store.Root(), forged+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Discard(forged); err == nil {
		t.Fatal("manifest pointing at another upload was discarded")
	}
	if _, _, err := store.Resolve(victim.ID); err != nil {
		t.Fatalf("another upload's content was removed: %v", err)
	}
}
