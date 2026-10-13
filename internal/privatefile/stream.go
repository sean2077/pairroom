package privatefile

import (
	"os"
	"path/filepath"
	"runtime"

	"github.com/sean2077/pairroom/internal/atomicfile"
)

// Open opens a bounded owner-private regular file without accepting a redirected
// pathname or a changed identity between pathname and handle inspection.
func Open(path string, limit int64) (*os.File, error) {
	if err := CheckDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || !before.Mode().IsRegular() || before.Size() > limit {
		return nil, ErrInvalid
	}
	f, err := atomicfile.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Size() > limit {
		f.Close()
		return nil, ErrInvalid
	}
	if !privateFile(f, after) {
		f.Close()
		return nil, ErrPrivate
	}
	return f, nil
}

// CreateTemp reuses the platform's explicit owner ACL rather than relying on
// chmod or inherited Windows permissions. The caller removes abandoned files.
func CreateTemp(dir string) (*os.File, error) {
	if err := CheckDirectory(dir); err != nil {
		return nil, err
	}
	return privateTemp(dir)
}

// Install durably renames a closed private temporary file within one directory.
// The caller serializes competing installs and decides whether replacement is
// permitted; this helper does not relax any existing file's permissions.
func Install(temp, path string) error {
	if filepath.Dir(temp) != filepath.Dir(path) {
		return ErrInvalid
	}
	f, err := Open(temp, 1<<30)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if info, e := os.Lstat(path); e == nil && !info.Mode().IsRegular() {
		return ErrInvalid
	} else if e != nil && !os.IsNotExist(e) {
		return e
	} else if e == nil {
		existing, e := Open(path, 1<<30)
		if e != nil {
			return e
		}
		if e = existing.Close(); e != nil {
			return e
		}
	}
	if err := atomicfile.Replace(temp, path); err != nil {
		return err
	}
	return SyncDirectory(filepath.Dir(path))
}

// SyncDirectory makes directory-entry changes durable where supported.
func SyncDirectory(dir string) error {
	if err := CheckDirectory(dir); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
