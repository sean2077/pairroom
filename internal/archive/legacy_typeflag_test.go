package archive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Legacy writers mark regular files with the pre-POSIX NUL typeflag. The
// standard reader normalizes it to TypeReg, so accepting it requires no
// deprecated-constant comparison; this pins that compatibility boundary.
func TestRestoreAcceptsLegacyNULTypeflagRegularFiles(t *testing.T) {
	dataDir, _ := makeValidDataDir(t)
	backup := filepath.Join(t.TempDir(), "valid.tar.gz")
	if _, err := Backup(dataDir, backup); err != nil {
		t.Fatal(err)
	}
	compressed, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	gzIn, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(gzIn)
	if err != nil {
		t.Fatal(err)
	}
	// archive/tar's writer promotes TypeRegA to TypeReg, so patch the encoded
	// ustar headers directly: typeflag at offset 156, checksum at 148..155.
	legacy := 0
	for offset := 0; offset+512 <= len(raw); {
		block := raw[offset : offset+512]
		if bytes.Equal(block, make([]byte, 512)) {
			break
		}
		var size int64
		if _, err := fmt.Sscanf(string(bytes.Trim(block[124:136], "\x00 ")), "%o", &size); err != nil {
			t.Fatal(err)
		}
		next := offset + 512 + int((size+511)/512*512)
		switch block[156] {
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			// PAX metadata records describe the following entry; leave them.
			offset = next
			continue
		case tar.TypeReg:
		default:
			t.Fatalf("unexpected backup entry type %q", block[156])
		}
		block[156] = 0
		copy(block[148:156], "        ")
		var sum int64
		for _, b := range block {
			sum += int64(b)
		}
		copy(block[148:156], fmt.Sprintf("%06o\x00 ", sum))
		legacy++
		offset = next
	}
	if legacy == 0 {
		t.Fatal("backup contained no entries to rewrite")
	}
	var output bytes.Buffer
	gzOut := gzip.NewWriter(&output)
	if _, err := gzOut.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gzOut.Close(); err != nil {
		t.Fatal(err)
	}
	legacyBackup := filepath.Join(t.TempDir(), "legacy.tar.gz")
	if err := os.WriteFile(legacyBackup, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	// Confirm the fixture really carries the legacy flag on disk.
	gzCheck, err := gzip.NewReader(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(gzCheck)
	if err != nil || !bytes.Equal(encoded, raw) {
		t.Fatalf("fixture was not preserved: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "restored")
	report, err := Restore(legacyBackup, restored, false)
	if err != nil {
		t.Fatalf("legacy regular-file entries rejected: %v", err)
	}
	if !report.OK || !Verify(restored).OK {
		t.Fatalf("legacy restore report = %#v", report)
	}
}
