package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func makeValidDataDir(t *testing.T) (string, model.Attachment) {
	t.Helper()
	dataDir := t.TempDir()
	repo := t.TempDir()
	media, err := attachment.Open(dataDir, repo)
	if err != nil {
		t.Fatal(err)
	}
	imageValue := image.NewRGBA(image.Rect(0, 0, 2, 2))
	imageValue.Set(0, 0, color.RGBA{R: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageValue); err != nil {
		t.Fatal(err)
	}
	attached, err := media.SaveImage("sample.png", bytes.NewReader(encoded.Bytes()), "test")
	if err != nil {
		t.Fatal(err)
	}

	eventStore, err := store.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	created, err := model.NewEvent("room-test", "room.created", model.ActorSystem, model.RoomMeta{ID: "room-test", Name: "Test", Repo: repo, CreatedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if err := eventStore.Append(&created); err != nil {
		t.Fatal(err)
	}
	message := model.Message{ID: "msg-test", Seq: 2, From: model.ActorUser, To: []model.ActorID{model.ActorClaude}, Text: "SECRET_TRANSCRIPT_TEXT", ThreadID: "msg-test", CreatedAt: time.Now().UTC(), Attachments: []model.Attachment{attached}}
	messageEvent, err := model.NewEvent("room-test", "message.created", model.ActorUser, message)
	if err != nil {
		t.Fatal(err)
	}
	if err := eventStore.Append(&messageEvent); err != nil {
		t.Fatal(err)
	}
	if err := eventStore.Close(); err != nil {
		t.Fatal(err)
	}
	return dataDir, attached
}

func TestVerifyBackupRestoreRoundTrip(t *testing.T) {
	dataDir, attached := makeValidDataDir(t)
	report := Verify(dataDir)
	if !report.OK || report.EventCount != 2 || report.AttachmentCount != 1 || report.ReferencedAttachments != 1 {
		t.Fatalf("unexpected verify report: %#v", report)
	}
	backup := filepath.Join(t.TempDir(), "room.tar.gz")
	manifest, err := Backup(dataDir, backup)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != backupFormat || len(manifest.Files) != 4 {
		t.Fatalf("unexpected backup manifest: %#v", manifest)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	restoredReport, err := Restore(backup, restored, false)
	if err != nil {
		t.Fatal(err)
	}
	if !restoredReport.OK || restoredReport.DataDir != restored {
		t.Fatalf("unexpected restored report: %#v", restoredReport)
	}
	content, err := os.ReadFile(filepath.Join(restored, "attachments", attached.ID+".png"))
	if err != nil || len(content) == 0 {
		t.Fatalf("restored attachment missing: %v", err)
	}
	if Verify(restored).OK != true {
		t.Fatalf("restored data failed second verification")
	}
}

func TestVerifyDetectsAttachmentTampering(t *testing.T) {
	dataDir, attached := makeValidDataDir(t)
	path := filepath.Join(dataDir, "attachments", attached.ID+".png")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("tampered"))
	_ = file.Close()
	report := Verify(dataDir)
	if report.OK || !containsReportError(report, "size") {
		t.Fatalf("tampering was not detected: %#v", report)
	}
	if _, err := Backup(dataDir, filepath.Join(t.TempDir(), "bad.tar.gz")); err == nil {
		t.Fatal("backup accepted corrupted data")
	}
}

func TestRestoreRejectsTraversalAndNonEmptyTarget(t *testing.T) {
	malicious := filepath.Join(t.TempDir(), "traversal.tar.gz")
	file, err := os.Create(malicious)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	manifest := BackupManifest{Format: backupFormat, FormatVersion: backupFormatVersion, PairRoomVersion: "test", CreatedAt: time.Now().UTC()}
	data, _ := json.Marshal(manifest)
	if err := writeTarBytes(tw, "manifest.json", data, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o600, Size: 1}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte("x"))
	_ = tw.Close()
	_ = gz.Close()
	_ = file.Close()
	if _, err := Restore(malicious, filepath.Join(t.TempDir(), "target"), false); err == nil || !strings.Contains(err.Error(), "unsafe archive path") {
		t.Fatalf("traversal restore error = %v", err)
	}

	dataDir, _ := makeValidDataDir(t)
	backup := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, err := Backup(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "occupied")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(backup, target, false); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("nonempty restore error = %v", err)
	}
	if _, err := Restore(backup, target, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "keep")); !os.IsNotExist(err) {
		t.Fatalf("forced restore kept old file: %v", err)
	}
}

func TestDiagnosticsRedactsTranscriptAndAttachments(t *testing.T) {
	dataDir, _ := makeValidDataDir(t)
	output := filepath.Join(t.TempDir(), "diagnostics.tar.gz")
	if err := Diagnostics(dataDir, output, "test-os", "test-arch"); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var all bytes.Buffer
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		all.WriteString(header.Name)
		_, _ = io.Copy(&all, tr)
	}
	text := all.String()
	if strings.Contains(text, "SECRET_TRANSCRIPT_TEXT") || strings.Contains(text, ".png") {
		t.Fatalf("diagnostics leaked sensitive room data: %q", text)
	}
	if !strings.Contains(text, "test-os") || !strings.Contains(text, "message.created") {
		t.Fatalf("diagnostics omitted structural evidence: %q", text)
	}
}

func TestRestoreManifestHashMismatch(t *testing.T) {
	dataDir, _ := makeValidDataDir(t)
	backup := filepath.Join(t.TempDir(), "valid.tar.gz")
	manifest, err := Backup(dataDir, backup)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Files[0].SHA256 = strings.Repeat("0", 64)
	bad := filepath.Join(t.TempDir(), "bad.tar.gz")
	if err := rewriteManifest(backup, bad, manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(bad, filepath.Join(t.TempDir(), "restore"), false); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("hash mismatch restore error = %v", err)
	}
}

func rewriteManifest(input, output string, manifest BackupManifest) error {
	in, err := os.Open(input)
	if err != nil {
		return err
	}
	defer in.Close()
	gzIn, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer gzIn.Close()
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	defer out.Close()
	gzOut := gzip.NewWriter(out)
	twOut := tar.NewWriter(gzOut)
	tr := tar.NewReader(gzIn)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		if header.Name == "manifest.json" {
			data, _ = json.Marshal(manifest)
			header.Size = int64(len(data))
		}
		if err := twOut.WriteHeader(header); err != nil {
			return err
		}
		if _, err := twOut.Write(data); err != nil {
			return err
		}
	}
	if err := twOut.Close(); err != nil {
		return err
	}
	if err := gzOut.Close(); err != nil {
		return err
	}
	return out.Sync()
}

func containsReportError(report VerifyReport, fragment string) bool {
	for _, value := range report.Errors {
		if strings.Contains(value, fragment) {
			return true
		}
	}
	return false
}
