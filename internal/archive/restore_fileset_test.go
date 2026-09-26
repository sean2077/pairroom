package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rebuildBackupWithExtra copies a valid backup and adds extra entries that are
// correctly declared (size and hash) in its manifest, so only the path policy
// can reject them.
func rebuildBackupWithExtra(t *testing.T, backup string, extra map[string][]byte) string {
	t.Helper()
	input, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	gzIn, err := gzip.NewReader(bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	defer gzIn.Close()
	tr := tar.NewReader(gzIn)
	var manifest BackupManifest
	type entry struct {
		name string
		data []byte
	}
	var entries []entry
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "manifest.json" {
			if err := json.Unmarshal(data, &manifest); err != nil {
				t.Fatal(err)
			}
			continue
		}
		entries = append(entries, entry{name: header.Name, data: data})
	}
	for name, data := range extra {
		sum := sha256.Sum256(data)
		manifest.Files = append(manifest.Files, manifestFile{Path: name, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])})
		entries = append(entries, entry{name: name, data: data})
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	gzOut := gzip.NewWriter(&output)
	tw := tar.NewWriter(gzOut)
	if err := writeTarBytes(tw, "manifest.json", manifestData, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, value := range entries {
		if err := writeTarBytes(tw, value.name, value.data, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzOut.Close(); err != nil {
		t.Fatal(err)
	}
	crafted := filepath.Join(t.TempDir(), "crafted.tar.gz")
	if err := os.WriteFile(crafted, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return crafted
}

func TestRestoreAcceptsOnlyTheRoomBackupFileSet(t *testing.T) {
	dataDir, attached := makeValidDataDir(t)
	backup := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, err := Backup(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(backup, filepath.Join(t.TempDir(), "control"), false); err != nil {
		t.Fatalf("unmodified backup rejected: %v", err)
	}
	for name, path := range map[string]string{
		"alternate data stream":       "events.jsonl:hidden",
		"attachment alternate stream": "attachments/" + attached.ID + ".json:zone",
		"runtime state file":          "runtime/state.json",
		"top-level extra file":        "credentials",
		"nested attachment directory": "attachments/nested/x.png",
		"reserved device name":        "attachments/CON.png",
		"reserved device name bare":   "aux",
		"reserved device with digit":  "attachments/com1.json",
		"trailing dot":                "attachments/extra.png.",
		"trailing space":              "attachments/extra.png ",
		"control character":           "attachments/ex\x01tra.png",
		"orphan attachment content":   "attachments/att-000000000000000000000000.png",
		"attachment hidden temp file": "attachments/.upload-123.tmp",
	} {
		t.Run(name, func(t *testing.T) {
			crafted := rebuildBackupWithExtra(t, backup, map[string][]byte{path: []byte("extra")})
			parent := t.TempDir()
			target := filepath.Join(parent, "target")
			_, err := Restore(crafted, target, false)
			if err == nil {
				t.Fatalf("restore accepted undeclared Room path %q", path)
			}
			if !strings.Contains(err.Error(), "not part of a Room backup") {
				t.Fatalf("restore rejected %q for an unrelated reason: %v", path, err)
			}
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Fatalf("rejected restore published a target: %v", err)
			}
			leftovers, err := os.ReadDir(parent)
			if err != nil {
				t.Fatal(err)
			}
			if len(leftovers) != 0 {
				t.Fatalf("rejected restore left staging data: %v", leftovers)
			}
		})
	}
}
