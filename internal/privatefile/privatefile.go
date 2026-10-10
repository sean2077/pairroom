// Package privatefile stores transport identities in owner-only files on Unix
// and Windows. Chmod alone does not make a Windows secret file private.
package privatefile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/sean2077/pairroom/internal/atomicfile"
)

// ErrPrivate identifies an owner or permission boundary that cannot be trusted.
var ErrPrivate = errors.New("transport identity requires an owner-private regular file and direct directory")

// ErrInvalid identifies an unsafe file shape or an input outside its read bound.
// Callers may tolerate a damaged permission boundary, but must not mistake an
// uninterpretable identity for an intact private record.
var ErrInvalid = errors.New("transport identity is not a bounded regular file or direct directory")

// Mkdir creates exactly one private directory, or checks an existing one. It
// never changes the permissions of an existing shared or redirected directory.
func Mkdir(path string) error {
	if err := makeDirectory(path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return CheckDirectory(path)
}

// CheckDirectory validates an existing identity directory without creating,
// repairing, or changing it. Startup preflight must remain read-only.
func CheckDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalid
	}
	if !privateDirectory(path, info) {
		return ErrPrivate
	}
	return nil
}

func ReadJSON(path string, limit int64, value any) error {
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if limit <= 0 || !before.Mode().IsRegular() || before.Size() > limit {
		return ErrInvalid
	}
	f, err := atomicfile.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) {
		return ErrInvalid
	}
	if !privateFile(f, after) {
		return ErrPrivate
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return errors.New("invalid transport identity JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("transport identity has trailing JSON")
	}
	return nil
}

func WriteJSON(path string, value any) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrInvalid
	}
	if !privateDirectory(dir, info) {
		return ErrPrivate
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return ErrInvalid
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil || len(data) > 2<<20 {
		return errors.New("transport identity JSON exceeds its 2 MiB bound or cannot be encoded")
	}
	data = append(data, '\n')
	f, err := privateTemp(dir)
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := atomicfile.Replace(f.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
