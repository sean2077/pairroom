package attachment

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestEvidenceRoundTripAcrossIndependentStores(t *testing.T) {
	host, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("#!/bin/sh\nprintf 'repro: 中文\\n'\n")
	meta, err := host.SaveEvidence("../../repro.sh", bytes.NewReader(body), "native-relay")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "repro.sh" || meta.Kind != "file" || meta.MediaType != "text/plain" || meta.Width != 0 || meta.Size != int64(len(body)) {
		t.Fatalf("unexpected evidence metadata: %+v", meta)
	}
	_, hostPath, err := host.Resolve(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(meta)
	if err != nil || bytes.Contains(encoded, []byte(host.Root())) {
		t.Fatalf("metadata leaked a host path: %s, %v", encoded, err)
	}
	guest, err := Open(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, file, err := host.OpenFile(meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := guest.ImportVerified(meta, file); err != nil {
		t.Fatal(err)
	}
	_, guestPath, err := guest.Resolve(meta.ID)
	if err != nil || guestPath == hostPath || filepath.Ext(guestPath) != ".data" {
		t.Fatalf("guest cache path = %q, %v", guestPath, err)
	}
	got, err := os.ReadFile(guestPath)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("download changed bytes: %q, %v", got, err)
	}
	if info, err := os.Stat(guestPath); err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatalf("evidence must be private and non-executable: %v, %v", info, err)
	}
	if _, err := guest.ImportVerified(meta, bytes.NewReader(body)); err != nil {
		t.Fatalf("idempotent download: %v", err)
	}
	if _, err := guest.ResolveMany([]string{meta.ID, meta.ID}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(guestPath, bytes.Repeat([]byte("x"), len(body)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := guest.Resolve(meta.ID); err == nil {
		t.Fatal("same-size cache tampering must fail closed")
	}
}

func TestEvidenceRejectsUnsupportedPayloadsWithoutPublishing(t *testing.T) {
	for name, body := range map[string][]byte{
		"empty": {}, "binary": {0xff, 0xfe}, "nul": []byte("a\x00b"),
		"oversized": bytes.Repeat([]byte("a"), int(MaxEvidenceBytes+1)),
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveEvidence("repro.txt", bytes.NewReader(body), "upload"); err == nil {
				t.Fatal("invalid evidence was accepted")
			}
			entries, err := os.ReadDir(s.Root())
			if err != nil || len(entries) != 0 {
				t.Fatalf("rejected evidence left published files: %v, %v", entries, err)
			}
		})
	}
}

func TestAttachmentImportRejectsMismatchedManifestAndTraversal(t *testing.T) {
	host, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := host.SaveEvidence("patch.diff", strings.NewReader("+fixed\n"), "upload")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guest.ImportVerified(meta, strings.NewReader("+wrong\n")); err == nil {
		t.Fatal("mismatched hash was accepted")
	}
	meta.ID = "../../escape"
	if _, err := guest.ImportVerified(meta, strings.NewReader("+fixed\n")); err == nil {
		t.Fatal("path traversal ID was accepted")
	}
	entries, err := os.ReadDir(guest.Root())
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected import published content: %v, %v", entries, err)
	}
}

func TestAttachmentImageImportRetainsVerification(t *testing.T) {
	host, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := host.SaveImage("screen.png", bytes.NewReader(pngBytes(t)), "upload")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guest.ImportVerified(meta, bytes.NewReader(pngBytes(t))); err != nil {
		t.Fatal(err)
	}
	meta.Width = 2
	if _, err := guest.ImportVerified(meta, bytes.NewReader(pngBytes(t))); err == nil {
		t.Fatal("forged image dimensions were accepted")
	}
}

func TestSharedAttachmentRoomQuotaAndReclamation(t *testing.T) {
	s, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := s.SaveEvidence("log.txt", strings.NewReader("evidence"), "upload")
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := s.Discard(meta.ID); err != nil || !removed {
		t.Fatalf("file evidence reclamation = %v, %v", removed, err)
	}
	filler, err := os.Create(filepath.Join(s.Root(), "quota.data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := filler.Truncate(MaxRoomSharedBytes); err != nil {
		t.Fatal(err)
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveEvidence("log.txt", strings.NewReader("evidence"), "upload"); err == nil {
		t.Fatal("Room evidence quota was ignored")
	}
	if _, err := s.SaveSharedImage("screen.png", bytes.NewReader(pngBytes(t)), "upload"); err == nil {
		t.Fatal("Room shared image quota was ignored")
	}
}

func TestAttachmentStoreRejectsSymlinkRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires platform-specific privileges")
	}
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "attachments")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, ""); err == nil {
		t.Fatal("symlink cache root was accepted")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink target was modified: %v, %v", entries, err)
	}
}

// A bounded reader may fail during download. No metadata or partial object may
// become observable to a later claim in that case.
func TestAttachmentImportInterruptedDownload(t *testing.T) {
	host, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := host.SaveEvidence("log.txt", strings.NewReader("full log\n"), "upload")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guest.ImportVerified(meta, io.LimitReader(strings.NewReader("full log\n"), 4)); err == nil {
		t.Fatal("partial file became claimable")
	}
	if _, err := guest.ImportVerified(meta, strings.NewReader("full log\n")); err != nil {
		t.Fatalf("complete retry failed: %v", err)
	}
}
