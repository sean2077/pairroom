//go:build !windows

package service

import (
	"errors"
	"os"
	"syscall"
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
		if errors.Is(err, syscall.ESRCH) {
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
