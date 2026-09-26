//go:build windows

package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const errorSharingViolation syscall.Errno = 32 // ERROR_SHARING_VIOLATION

// replaceBudget bounds how long a save waits for a foreign reader that opened
// the target without FILE_SHARE_DELETE. Hooks and deliveries have seconds of
// budget; a longer hold fails the save visibly rather than blocking it.
const replaceBudget = time.Second

// sleep is replaced only by deterministic retry tests.
var sleep = time.Sleep

// Replace atomically renames tmp over path, which must share tmp's directory.
// os.Root.Rename uses POSIX rename semantics, so readers opened through Open
// (sharing delete) keep their old handle while the new file takes the name.
// Access-denied or sharing-violation failures are retried with a short backoff
// until replaceBudget elapses; the caller still owns cleanup of tmp.
func Replace(tmp, path string) error {
	dir := filepath.Dir(path)
	if filepath.Dir(tmp) != dir {
		return &os.LinkError{Op: "rename", Old: tmp, New: path, Err: errors.New("replacement must stay in one directory")}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	oldName, newName := filepath.Base(tmp), filepath.Base(path)
	delay := 5 * time.Millisecond
	deadline := time.Now().Add(replaceBudget)
	for {
		err = root.Rename(oldName, newName)
		if err == nil || !transient(err) || !time.Now().Before(deadline) {
			return err
		}
		sleep(delay)
		if delay < 100*time.Millisecond {
			delay *= 2
		}
	}
}

func transient(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) || errors.Is(err, errorSharingViolation)
}

// Open opens path read-only with read, write and delete sharing so that an
// atomic Replace of the same name is never blocked by this handle. It follows
// the same links os.Open does; callers keep their own Lstat/type checks.
func Open(path string) (*os.File, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}
