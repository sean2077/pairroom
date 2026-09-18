//go:build !windows && !linux

package service

import (
	"errors"
	"os"
	"syscall"
	"time"
)

func serviceLockProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}
	if err := process.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return false, nil
		}
		if errors.Is(err, syscall.EPERM) {
			// Permission to signal is not permission to conclude that the
			// process is absent. Keep recovery fail-closed.
			return true, nil
		}
		return false, err
	}
	return true, nil
}

// serviceLockProcessStartedAt has no portable stdlib source on this platform;
// reporting unknown keeps PID-reuse detection conservative (the caller treats
// a live PID as the owner).
func serviceLockProcessStartedAt(pid int) (time.Time, bool, error) {
	return time.Time{}, false, nil
}

// serviceLockProcessLooksLikeOwner is unreachable on this platform (startedAt
// is never ok), but the conservative answer keeps the link-time contract.
func serviceLockProcessLooksLikeOwner(pid int) bool {
	return true
}
