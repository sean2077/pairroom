//go:build !windows

package service

import (
	"errors"
	"os"
	"syscall"
)

// Lock the stable root directory, never the service.lock inode that recovery
// replaces. Independent opens serialize goroutines and processes alike, and
// process exit releases the kernel guard without a second stale-lock file.
// The existing PID/nonce metadata remains the durable ownership boundary.
func lockServiceRoot(root string) (func(), error) {
	file, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
}
