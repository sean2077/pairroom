package privatefile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOwnerPrivateIdentityRoundTripAndReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := Mkdir(dir); err != nil {
		t.Fatalf("existing private directory: %v", err)
	}
	path := filepath.Join(dir, "identity.json")
	for _, secret := range []string{"first-secret", "replacement-secret"} {
		if err := WriteJSON(path, map[string]string{"secret": secret}); err != nil {
			t.Fatal(err)
		}
		var got struct {
			Secret string `json:"secret"`
		}
		if err := ReadJSON(path, 4096, &got); err != nil || got.Secret != secret {
			t.Fatalf("identity round trip failed: %v", err)
		}
	}
	var value any
	if err := ReadJSON(path, 2, &value); err == nil {
		t.Fatal("oversized identity was read")
	}
	if err := WriteJSON(path, map[string]string{"unexpected": "field"}); err != nil {
		t.Fatal(err)
	}
	var strict struct {
		Secret string `json:"secret"`
	}
	if err := ReadJSON(path, 4096, &strict); err == nil {
		t.Fatal("unknown identity field was ignored")
	}
}

func TestIdentityRejectsSharedOrRedirectedFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes and symlinks; Windows protected DACL tested separately")
	}
	dir := filepath.Join(t.TempDir(), "identity")
	if err := Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.json")
	if err := WriteJSON(path, map[string]string{"secret": "private"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := ReadJSON(path, 4096, &value); err == nil {
		t.Fatal("world-readable identity was trusted")
	}
	alias := filepath.Join(dir, "alias.json")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(alias, map[string]string{"secret": "replacement"}); err == nil {
		t.Fatal("symlink identity was replaced")
	}
	if err := ReadJSON(alias, 4096, &value); err == nil {
		t.Fatal("symlink identity was read")
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Mkdir(dir); err == nil {
		t.Fatal("shared existing identity directory was accepted")
	}
}
