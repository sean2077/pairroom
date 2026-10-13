package enterpriseobject

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func physicalTmpfsRoot(t *testing.T) string {
	t.Helper()
	base := os.Getenv("PAIRROOM_OBJECT_FAULT_ROOT")
	if base == "" {
		t.Skip("PAIRROOM_OBJECT_FAULT_ROOT requires an isolated bounded Linux tmpfs; skip is not ENOSPC proof")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(base, &fs); err != nil {
		t.Fatal(err)
	}
	if fs.Type != 0x01021994 || uint64(fs.Bsize)*fs.Blocks > 256<<10 {
		t.Fatal("physical ENOSPC fixture must be a tmpfs no larger than 256 KiB")
	}
	root, err := os.MkdirTemp(base, "object-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	t.Logf("real tmpfs capacity=%d bytes, process uid=%d", uint64(fs.Bsize)*fs.Blocks, os.Getuid())
	return root
}

func TestObjectPhysicalENOSPCDoesNotInstallPartialObject(t *testing.T) {
	root := physicalTmpfsRoot(t)
	s, err := Open(root, "physical-enospc")
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("e"), 256<<10)
	verified, err := s.PutImmutable(t.Context(), objectA, textSpec(data), bytes.NewReader(data))
	if !errors.Is(err, syscall.ENOSPC) || verified != (Verified{}) {
		t.Fatalf("actual bounded filesystem did not fail with ENOSPC: verified=%t error=%v", verified != (Verified{}), err)
	}
	for _, name := range []string{"data", "manifest.json"} {
		if _, err := os.Lstat(filepath.Join(root, objectA, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed physical write installed %s: %v", name, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, objectA))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != ".lock" {
			t.Fatalf("failed physical write retained uninstalled bytes: %s", entry.Name())
		}
	}
	if _, err := s.Inspect(t.Context(), objectA); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial write yielded a verified object: %v", err)
	}
	t.Log("real ENOSPC preserved absence of final data/manifest and removed temporary bytes")
}

func TestObjectLinuxSymlinkBoundariesDoNotReadOrWriteOutside(t *testing.T) {
	for _, cut := range []string{"root-parent", "object-directory", "final-data"} {
		t.Run(cut, func(t *testing.T) {
			outside := t.TempDir()
			data := []byte("outside sentinel unchanged")
			marker := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(marker, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if cut == "root-parent" {
				link := filepath.Join(t.TempDir(), "redirect")
				if err := os.Symlink(outside, link); err != nil {
					t.Fatal("Linux symlink fixture must execute", err)
				}
				if _, err := Open(filepath.Join(link, "objects"), "symlink-boundary"); err == nil {
					t.Fatal("redirected root parent accepted")
				}
			} else {
				s := openTestStore(t)
				if cut == "object-directory" {
					if err := os.Symlink(outside, filepath.Join(s.root, objectA)); err != nil {
						t.Fatal("Linux symlink fixture must execute", err)
					}
					if _, err := s.PutImmutable(t.Context(), objectA, textSpec(data), bytes.NewReader(data)); err == nil {
						t.Fatal("redirected object directory accepted")
					}
				} else {
					if _, err := s.PutImmutable(t.Context(), objectA, textSpec(data), bytes.NewReader(data)); err != nil {
						t.Fatal(err)
					}
					path := filepath.Join(s.root, objectA, "data")
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(marker, path); err != nil {
						t.Fatal("Linux symlink fixture must execute", err)
					}
					if _, err := s.Inspect(t.Context(), objectA); err == nil {
						t.Fatal("followed final-data symlink, even though sentinel bytes matched")
					}
				}
			}
			got, err := os.ReadFile(marker)
			entries, directoryErr := os.ReadDir(outside)
			if err != nil || !bytes.Equal(got, data) || directoryErr != nil || len(entries) != 1 || entries[0].Name() != "sentinel" {
				t.Fatal("guard changed the external sentinel or wrote outside the object root")
			}
		})
	}
}
