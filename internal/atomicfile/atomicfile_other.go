//go:build !windows

package atomicfile

import "os"

// Replace atomically renames tmp over path in the same directory.
func Replace(tmp, path string) error { return os.Rename(tmp, path) }

// Open opens path read-only; POSIX readers never block a rename.
func Open(path string) (*os.File, error) { return os.Open(path) }
