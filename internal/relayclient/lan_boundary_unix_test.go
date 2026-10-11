//go:build !windows

package relayclient

import (
	"os"
	"testing"
)

// breakOwnerBoundary reproduces the boundary a joined workspace loses when it is
// restored from a backup or copied to another machine: the bytes stay, the
// owner-only mode does not. A directory keeps its traversal bits so the copy
// remains usable.
func breakOwnerBoundary(t *testing.T, path string) {
	t.Helper()
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.IsDir() {
		mode = 0o755
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
