package enterpriseobject

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/privatefile"
)

const objectA = "att-0123456789abcdef01234567"

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "objects"), "dep-test")
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func textSpec(data []byte) Spec {
	hash := sha256.Sum256(data)
	return Spec{Name: "evidence.txt", Kind: "file", Size: int64(len(data)), SHA256: hex.EncodeToString(hash[:])}
}
func TestImmutableObjectRecoveryAndPin(t *testing.T) {
	s := openTestStore(t)
	data := []byte("evidence\n")
	spec := textSpec(data)
	v, e := s.PutImmutable(t.Context(), objectA, spec, bytes.NewReader(data))
	if e != nil {
		t.Fatal(e)
	}
	if v.DeploymentID() != "dep-test" || v.Metadata().MediaType != "text/plain" {
		t.Fatal("metadata was not derived")
	}
	m := v.Metadata()
	m.CreatedAt = time.Now().UTC()
	got, e := s.ReadVerified(t.Context(), m)
	if e != nil || !bytes.Equal(got, data) {
		t.Fatalf("read: %q %v", got, e)
	}
	if _, e = Open(s.root, "wrong-deployment"); e == nil {
		t.Fatal("accepted wrong deployment")
	}
	if _, e = s.PutImmutable(t.Context(), objectA, textSpec([]byte("different")), bytes.NewBufferString("different")); !errors.Is(e, ErrConflict) {
		t.Fatalf("overwrote immutable object: %v", e)
	}
	if e = os.Remove(filepath.Join(s.root, objectA, "manifest.json")); e != nil {
		t.Fatal(e)
	}
	if _, e = s.PutImmutable(t.Context(), objectA, spec, bytes.NewReader(data)); e != nil {
		t.Fatalf("partial-install recovery: %v", e)
	}
	if _, e = s.Inspect(t.Context(), objectA); e != nil {
		t.Fatal(e)
	}
}
func TestObjectRejectsMismatchesAndTraversal(t *testing.T) {
	s := openTestStore(t)
	data := []byte("valid")
	spec := textSpec(data)
	for _, id := range []string{"../outside", "att-0123456789ABCDEF01234567", "digest", "att-0123456789abcdef012345678"} {
		if _, e := s.PutImmutable(t.Context(), id, spec, bytes.NewReader(data)); e == nil {
			t.Fatalf("accepted %s", id)
		}
	}
	for _, data := range [][]byte{[]byte("vali"), []byte("valid-extra"), []byte("\x00bad"), {0xff, 0xff}} {
		candidate := spec
		if bytes.IndexByte(data, 0) >= 0 || bytes.Equal(data, []byte{0xff, 0xff}) {
			candidate = textSpec(data)
		}
		if _, e := s.PutImmutable(t.Context(), objectA, candidate, bytes.NewReader(data)); e == nil {
			t.Fatal("accepted invalid bytes")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, e := s.PutImmutable(ctx, objectA, spec, bytes.NewReader(data)); e == nil {
		t.Fatal("accepted cancelled installation")
	}
	if _, e := s.Inspect(t.Context(), objectA); e == nil {
		t.Fatal("failed uploads left verified object")
	}
}
func TestObjectImageValidationAndPrivateContent(t *testing.T) {
	s := openTestStore(t)
	var b bytes.Buffer
	if e := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3))); e != nil {
		t.Fatal(e)
	}
	spec := textSpec(b.Bytes())
	spec.Name = "image.png"
	spec.Kind = "image"
	v, e := s.PutImmutable(t.Context(), objectA, spec, bytes.NewReader(b.Bytes()))
	if e != nil {
		t.Fatal(e)
	}
	meta := v.Metadata()
	if meta.MediaType != "image/png" || meta.Width != 2 || meta.Height != 3 {
		t.Fatal(meta)
	}
	f, e := privatefile.Open(filepath.Join(s.root, objectA, "data"), spec.Size)
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	if _, e = s.PutImmutable(t.Context(), "att-1123456789abcdef01234567", Spec{Name: "fake.png", Kind: "image", Size: 5, SHA256: textSpec([]byte("hello")).SHA256}, bytes.NewBufferString("hello")); e == nil {
		t.Fatal("accepted fake image")
	}
}
func TestObjectBackupGateAndPermanentDeletion(t *testing.T) {
	s := openTestStore(t)
	data := []byte("retained")
	spec := textSpec(data)
	if _, e := s.PutImmutable(t.Context(), objectA, spec, bytes.NewReader(data)); e != nil {
		t.Fatal(e)
	}
	if e := s.BeginBackup(t.Context(), "backup-one"); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(s.root, "dep-test")
	if e != nil {
		t.Fatal(e)
	}
	if e = reopened.Delete(t.Context(), objectA); !errors.Is(e, ErrBackup) {
		t.Fatalf("deletion ignored persistent barrier: %v", e)
	}
	if e = reopened.EndBackup(t.Context(), "backup-one"); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(s.root, objectA)
	temp, e := privatefile.CreateTemp(dir)
	if e != nil {
		t.Fatal(e)
	}
	temp.Write([]byte("crash residue"))
	temp.Close()
	if e = reopened.Delete(t.Context(), objectA); e != nil {
		t.Fatal(e)
	}
	if e = reopened.Delete(t.Context(), objectA); e != nil {
		t.Fatal(e)
	}
	if _, e = reopened.PutImmutable(t.Context(), objectA, spec, bytes.NewReader(data)); !errors.Is(e, ErrDeleted) {
		t.Fatalf("late upload resurrected deleted object: %v", e)
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 1 || entries[0].Name() != "deleted.json" {
		t.Fatalf("deleted bytes retained: %v %v", entries, e)
	}
}
func TestObjectConcurrentInstallAndDelete(t *testing.T) {
	s := openTestStore(t)
	data := []byte("same content")
	spec := textSpec(data)
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.PutImmutable(t.Context(), objectA, spec, bytes.NewReader(data))
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	errs = make(chan error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); errs <- s.Delete(t.Context(), objectA) }()
	go func() {
		defer wg.Done()
		_, e := s.PutImmutable(t.Context(), objectA, spec, bytes.NewReader(data))
		if errors.Is(e, ErrDeleted) {
			e = nil
		}
		errs <- e
	}()
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.Inspect(t.Context(), objectA); !errors.Is(e, ErrDeleted) {
		t.Fatalf("delete race did not leave tombstone: %v", e)
	}
}
func TestObjectRejectsSymlink(t *testing.T) {
	s := openTestStore(t)
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(s.root, objectA)); e != nil {
		t.Skipf("symlink fixture unavailable: %v", e)
	}
	if _, e := s.PutImmutable(t.Context(), objectA, textSpec([]byte("safe")), bytes.NewBufferString("safe")); e == nil {
		t.Fatal("accepted redirected object directory")
	}
	entries, e := os.ReadDir(outside)
	if e != nil || len(entries) != 0 {
		t.Fatal("wrote through symlink")
	}
}
