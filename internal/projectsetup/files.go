package projectsetup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxFileSize = 2 << 20

type fileState struct {
	data         []byte
	mode         os.FileMode
	exists       bool
	parentExists bool
}

// openParent pins each real directory. No external-manager symlinks (including
// a symlinked skills parent) are followed, even when replacement is confirmed.
func openParent(root *os.Root, path string, create bool) (*os.Root, error) {
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(filepath.ToSlash(filepath.Dir(path)), "/") {
		info, err := current.Lstat(name)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = current.Mkdir(name, 0o755); err == nil || errors.Is(err, os.ErrExist) {
				info, err = current.Lstat(name)
			}
		}
		if err != nil {
			current.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			current.Close()
			return nil, ErrBlocked
		}
		next, err := current.OpenRoot(name)
		if err != nil {
			current.Close()
			return nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			next.Close()
			current.Close()
			return nil, ErrChanged
		}
		current.Close()
		current = next
	}
	return current, nil
}

func readFile(root *os.Root, path string) (fileState, error) {
	parent, err := openParent(root, path, false)
	if errors.Is(err, os.ErrNotExist) {
		return fileState{mode: 0o644}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	defer parent.Close()
	return readLeaf(parent, filepath.Base(path))
}

func readLeaf(parent *os.Root, name string) (fileState, error) {
	result := fileState{parentExists: true, mode: 0o644}
	info, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxFileSize {
		return result, ErrBlocked
	}
	file, err := parent.Open(name)
	if err != nil {
		return result, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return result, ErrChanged
	}
	data, err := io.ReadAll(io.LimitReader(file, maxFileSize+1))
	if err != nil {
		return result, err
	}
	if len(data) > maxFileSize {
		return result, ErrBlocked
	}
	result.data, result.mode, result.exists = data, info.Mode().Perm(), true
	return result, nil
}

func replaceFile(root *os.Root, path string, before fileState, after []byte) error {
	// Check before creating parents, so stale previews do not mutate the tree.
	current, err := readFile(root, path)
	if err != nil {
		return err
	}
	if revision(path, current) != revision(path, before) {
		return ErrChanged
	}
	parent, err := openParent(root, path, true)
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(path)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	temporary := ".pairroom-setup-" + hex.EncodeToString(random[:])
	file, err := parent.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, before.mode)
	if err != nil {
		return err
	}
	defer parent.Remove(temporary)
	if err = file.Chmod(before.mode); err == nil {
		_, err = file.Write(after)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	current, err = readLeaf(parent, name)
	if err != nil {
		return err
	}
	// Parent directories may have been created by this call, but no file was.
	current.parentExists = before.parentExists
	if revision(path, current) != revision(path, before) {
		return ErrChanged
	}
	if !before.exists {
		// Publish without replacing a file that appeared after the last check.
		return parent.Link(temporary, name)
	}
	// Rename replaces the directory entry, never truncates a hardlink's target.
	return parent.Rename(temporary, name)
}
