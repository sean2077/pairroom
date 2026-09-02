//go:build windows

package service

import (
	"errors"
	"fmt"
	"syscall"
)

func serviceLockProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	const access = syscall.PROCESS_QUERY_INFORMATION | syscall.SYNCHRONIZE
	handle, err := syscall.OpenProcess(access, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER (87) and ERROR_FILE_NOT_FOUND (2) both mean
		// that Windows has no process with the recorded PID. The former is not
		// exported by every Go Windows syscall surface.
		if errors.Is(err, syscall.Errno(87)) || errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
			return false, nil
		}
		if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			// Permission to inspect is not permission to conclude that the
			// process is absent. Keep recovery fail-closed.
			return true, nil
		}
		return false, err
	}
	defer syscall.CloseHandle(handle)
	state, err := syscall.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	switch state {
	case syscall.WAIT_TIMEOUT:
		return true, nil
	case syscall.WAIT_OBJECT_0:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected process wait state 0x%x", state)
	}
}
