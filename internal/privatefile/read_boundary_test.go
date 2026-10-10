package privatefile

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIdentityReadSeparatesInvalidInputFromOwnerBoundary(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.json")
	if err := WriteJSON(path, map[string]string{"key": "original"}); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{0, 2} {
		var value any
		err := ReadJSON(path, limit, &value)
		if !errors.Is(err, ErrInvalid) || errors.Is(err, ErrPrivate) {
			t.Fatalf("read bound %d was classified as a repairable owner boundary: %v", limit, err)
		}
	}
	var value any
	if err := ReadJSON(dir, 4096, &value); !errors.Is(err, ErrInvalid) {
		t.Fatalf("directory was treated as a private JSON file: %v", err)
	}
	if err := CheckDirectory(path); !errors.Is(err, ErrInvalid) {
		t.Fatalf("regular file was treated as a private directory: %v", err)
	}
	var original struct {
		Key string `json:"key"`
	}
	if err := ReadJSON(path, 4096, &original); err != nil || original.Key != "original" {
		t.Fatalf("failed reads changed the private identity: %+v %v", original, err)
	}
}

func TestIdentityPermissionErrorsRemainDistinctFromRedirectedInputs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.json")
	// A normal file inherits its Windows DACL; on Unix make its shared mode
	// explicit so the test exercises the real owner-only check on both systems.
	if err := os.WriteFile(path, []byte(`{"key":"private"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var value any
	if err := ReadJSON(path, 4096, &value); !errors.Is(err, ErrPrivate) || errors.Is(err, ErrInvalid) {
		t.Fatalf("owner boundary was not classified separately: %v", err)
	}
	if runtime.GOOS == "windows" {
		return // Windows symlink creation can require a separate OS privilege.
	}
	alias := filepath.Join(dir, "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(alias, 4096, &value); !errors.Is(err, ErrInvalid) {
		t.Fatalf("redirected identity was classified as intact private data: %v", err)
	}
	if err := CheckDirectory(alias); !errors.Is(err, ErrInvalid) {
		t.Fatalf("redirected directory was classified as intact private data: %v", err)
	}
}
