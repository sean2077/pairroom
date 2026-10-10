//go:build !windows

package relayclient

import (
	"os"
	"testing"
)

// breakOwnerBoundary reproduces the boundary a joined workspace loses when it is
// restored from a backup or copied from another machine: the bytes stay, the
// owner-only mode does not.
func breakOwnerBoundary(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}
