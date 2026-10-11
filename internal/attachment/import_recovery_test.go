package attachment

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportedContentRecoversMissingManifestWithoutDoubleChargingQuota(t *testing.T) {
	host, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	const content = "#!/bin/sh\necho reproduction\n"
	meta, err := host.SaveEvidence("repro.sh", strings.NewReader(content), "lan")
	if err != nil {
		t.Fatal(err)
	}
	manifestInfo, err := os.Stat(filepath.Join(host.Root(), meta.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	guest, err := Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce exit after content rename and before the manifest commit.
	path := filepath.Join(guest.Root(), meta.ID+".data")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	filler, err := os.Create(filepath.Join(guest.Root(), "quota.data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := filler.Truncate(MaxRoomSharedBytes - meta.Size - manifestInfo.Size()); err != nil {
		t.Fatal(err)
	}
	if err := filler.Close(); err != nil {
		t.Fatal(err)
	}
	guest, err = Open(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := guest.Resolve(meta.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("orphan fixture is already published: %v", err)
	}
	got, err := guest.ImportVerified(meta, strings.NewReader(content))
	if err != nil || !sameAttachment(got, meta) {
		t.Fatalf("verified retry failed to finish original import: %+v %v", got, err)
	}
	now, err := os.Stat(path)
	if err != nil || !os.SameFile(original, now) {
		t.Fatal("recovery replaced or duplicated already verified content")
	}
	if _, _, err := guest.Resolve(meta.ID); err != nil {
		t.Fatalf("repaired attachment cannot be consumed: %v", err)
	}
	if _, err := guest.ImportVerified(meta, strings.NewReader(content)); err != nil {
		t.Fatalf("repeated recovery was not idempotent: %v", err)
	}
}

func TestImportedContentRecoveryRejectsDifferentOrNonregularOrphans(t *testing.T) {
	host, err := Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := host.SaveEvidence("repro.sh", strings.NewReader("expected"), "lan")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"different_bytes", "directory", "conflicting_extension"} {
		t.Run(name, func(t *testing.T) {
			guest, err := Open(t.TempDir(), "")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(guest.Root(), meta.ID+".data")
			switch name {
			case "different_bytes":
				err = os.WriteFile(path, []byte("tampered"), 0o600)
			case "directory":
				err = os.Mkdir(path, 0o700)
			case "conflicting_extension":
				err = os.WriteFile(filepath.Join(guest.Root(), meta.ID+".png"), []byte("ambiguous"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := guest.ImportVerified(meta, strings.NewReader("expected")); err == nil {
				t.Fatal("ambiguous orphan was adopted or overwritten")
			}
			if _, err := os.Stat(filepath.Join(guest.Root(), meta.ID+".json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected recovery published a manifest")
			}
		})
	}
}
