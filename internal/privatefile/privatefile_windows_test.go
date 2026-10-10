//go:build windows

package privatefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIdentityRejectsInheritedWindowsDACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := Mkdir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "identity.json")
	// Ordinary WriteFile inherits the directory ACL. It has not established
	// the protected file-specific owner policy required for transport keys.
	if err := os.WriteFile(path, []byte(`{"key":"unprotected"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var value any
	if err := ReadJSON(path, 1024, &value); err == nil {
		t.Fatal("inherited Windows identity ACL was trusted")
	}
	if err := WriteJSON(path, map[string]string{"key": "protected"}); err != nil {
		t.Fatal(err)
	}
	if err := ReadJSON(path, 1024, &value); err != nil {
		t.Fatalf("protected owner-only identity was rejected: %v", err)
	}
}
