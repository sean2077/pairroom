//go:build !windows

package relayclient

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// Lock the stable slot directory, not the inode replaced by atomic state or
// credential writes. Kernel locks disappear on process death; no lock files.
func lockSlot(ctx context.Context, dir string) (func(), error) {
	file, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
