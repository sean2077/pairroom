package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRestoreValidatesGzipTrailerBeforeReplacingTarget(t *testing.T) {
	dataDir, _ := makeValidDataDir(t)
	backup := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, err := Backup(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"checksum", "truncated", "trailing-garbage"} {
		t.Run(mode, func(t *testing.T) {
			data := append([]byte(nil), original...)
			switch mode {
			case "checksum":
				data[len(data)-8] ^= 0xff
			case "truncated":
				data = data[:len(data)-8]
			case "trailing-garbage":
				data = append(data, []byte("unexpected trailing data")...)
			}
			bad := filepath.Join(t.TempDir(), "bad.tar.gz")
			if err := os.WriteFile(bad, data, 0o600); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			keep := filepath.Join(target, "keep")
			if err := os.WriteFile(keep, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Restore(bad, target, true); err == nil {
				t.Fatal("corrupt gzip container accepted")
			}
			if data, err := os.ReadFile(keep); err != nil || string(data) != "original" {
				t.Fatalf("failed validation replaced the target: %q, %v", data, err)
			}
		})
	}
}

func TestRestoreRejectsDuplicateEmptyManifest(t *testing.T) {
	dataDir, _ := makeValidDataDir(t)
	backup := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, err := Backup(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	gzIn, err := gzip.NewReader(bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	defer gzIn.Close()
	var output bytes.Buffer
	gzOut := gzip.NewWriter(&output)
	tw := tar.NewWriter(gzOut)
	if err := writeTarBytes(tw, "manifest.json", []byte(`{}`), time.Now()); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gzIn)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(tw, tr); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzOut.Close(); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "duplicate.tar.gz")
	if err := os.WriteFile(bad, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(bad, filepath.Join(t.TempDir(), "target"), false); err == nil || !strings.Contains(err.Error(), "duplicate manifest") {
		t.Fatalf("duplicate empty manifest error = %v", err)
	}
}

func TestArchiveOutputsCannotReplaceRoomData(t *testing.T) {
	for _, operation := range []string{"backup", "diagnostics"} {
		t.Run(operation, func(t *testing.T) {
			dataDir, _ := makeValidDataDir(t)
			target := filepath.Join(dataDir, "events.jsonl")
			before, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "backup" {
				_, err = Backup(dataDir, target)
			} else {
				err = Diagnostics(dataDir, target, "test-os", "test-arch")
			}
			if err == nil {
				t.Error("archive output replaced its source Room data")
			}
			after, readErr := os.ReadFile(target)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("Room history changed: %v", readErr)
			}
		})
	}
}

func TestRestoreBoundsManifestAndTrailingPadding(t *testing.T) {
	var oversized bytes.Buffer
	gz := gzip.NewWriter(&oversized)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Size: maxManifestBytes + 1, Mode: 0o600, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	// Deliberately omit the body: validation must reject the advertised size
	// before attempting to allocate/read it.
	_ = tw.Close()
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "oversized.tar.gz")
	if err := os.WriteFile(input, oversized.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(input, filepath.Join(t.TempDir(), "target"), false); err == nil || !strings.Contains(err.Error(), "manifest exceeds size limit") {
		t.Fatalf("oversized manifest error = %v", err)
	}

	dataDir, _ := makeValidDataDir(t)
	backup := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, err := Backup(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	compressed, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	tarData, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		padding []byte
		valid   bool
	}{
		{"zero-padding", make([]byte, 512), true},
		{"hidden-data", []byte("hidden archive"), false},
		{"padding-limit", make([]byte, maxTrailingPadding+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buffer bytes.Buffer
			writer := gzip.NewWriter(&buffer)
			if _, err := writer.Write(tarData); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write(tc.padding); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "padded.tar.gz")
			if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Restore(path, filepath.Join(t.TempDir(), "target"), false)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}

func TestArchiveOutputResolvesAncestorsAndAllowsExternalDirectories(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	for _, name := range []string{"events.jsonl", "attachments/image.png", "new/directory/archive.tar.gz"} {
		if _, err := archiveOutputPath(root, filepath.Join(root, filepath.FromSlash(name))); err == nil {
			t.Errorf("accepted output inside source: %s", name)
		}
	}
	output := filepath.Join(outside, "new", "archive.tar.gz")
	if actual, err := archiveOutputPath(root, output); err != nil || actual != output {
		t.Fatalf("new external output rejected: %s, %v", actual, err)
	}
	if _, err := os.Stat(filepath.Dir(output)); !os.IsNotExist(err) {
		t.Fatalf("validation created a directory: %v", err)
	}
	link := filepath.Join(outside, "alias")
	if err := os.Symlink(root, link); err != nil {
		t.Logf("symlink-specific assertion unavailable: %v", err)
		return
	}
	if _, err := archiveOutputPath(root, filepath.Join(link, "new", "archive.tar.gz")); err == nil {
		t.Fatal("symlinked parent bypassed output protection")
	}
}
