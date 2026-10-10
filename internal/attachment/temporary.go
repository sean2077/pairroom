package attachment

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/privatelock"
)

const (
	// MaxTemporaryBytes is a separate bound on shared upload staging and
	// interrupted writes. Existing debris remains charged across reopen;
	// reaching the limit never authorizes deleting evidence or cached files.
	MaxTemporaryBytes int64 = 32 << 20
	// Charge empty and tiny crash leftovers too, bounding directory debris
	// to 512 entries even when the process exited before writing any bytes.
	minTemporaryFileBytes int64 = 64 << 10
)

// ErrTemporaryQuota is safe to display without exposing a private cache path.
var ErrTemporaryQuota = errors.New("attachment temporary storage exceeds 32 MiB; inspect leftover uploads before retrying")

func attachmentTemporaryName(name string) bool {
	for _, prefix := range []string{".staged-", ".attachment-", ".image-", ".metadata-"} {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".tmp") && len(name) > len(prefix)+len(".tmp") {
			return true
		}
	}
	return false
}

// checkTemporaryQuota is called with Store.mu held and before any temporary
// file is created. The caller also serializes this check and its local writes
// across Store instances: stages use the attachment directory lock; verified
// guest imports already hold their separate private evidence directory lock.
// Active streams have their maximum size reserved in the file itself, so a
// newly opened Store cannot mistake a slow upload for an empty crash leftover.
func (s *Store) checkTemporaryQuota(additional int64) error {
	if additional < 0 || additional > MaxTemporaryBytes {
		return ErrTemporaryQuota
	}
	var total int64
	if additional > 0 {
		total = max(additional, minTemporaryFileBytes)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !attachmentTemporaryName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue // a completed writer may have removed its own temporary
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("attachment temporary storage contains a non-regular entry")
		}
		charge := max(info.Size(), minTemporaryFileBytes)
		if charge > MaxTemporaryBytes-total {
			return ErrTemporaryQuota
		}
		total += charge
	}
	return nil
}

// The kernel lock covers only quota accounting, allocation, and publication,
// never a request body read. Store.mu alone cannot serialize an old runtime's
// unfinished stage with another Store opened for the same attachment directory.
func (s *Store) lockTemporaryDirectory() (func(), error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return privatelock.Lock(ctx, s.root)
}

func (s *Store) createTemporaryStage() (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockTemporaryDirectory()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.checkTemporaryQuota(MaxImageBytes + 1); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(s.root, ".staged-*.tmp")
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	// Truncate reserves the full logical length before another allocator may
	// inspect the directory. Copy starts at offset zero and never grows past
	// this reservation, including its one-byte oversize sentinel.
	if err := file.Truncate(MaxImageBytes + 1); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

func (s *Store) discardTemporary(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockTemporaryDirectory()
	if err != nil {
		return err
	}
	defer unlock()
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
