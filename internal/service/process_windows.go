//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func serviceLockProcessAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	const access = windows.PROCESS_QUERY_INFORMATION | windows.SYNCHRONIZE
	handle, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		// ERROR_INVALID_PARAMETER and ERROR_FILE_NOT_FOUND both mean
		// that Windows has no process with the recorded PID.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			return false, nil
		}
		if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			// Permission to inspect is not permission to conclude that the
			// process is absent. Keep recovery fail-closed.
			return true, nil
		}
		return false, err
	}
	defer windows.CloseHandle(handle)
	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return false, err
	}
	switch state {
	case uint32(windows.WAIT_TIMEOUT):
		return true, nil
	case windows.WAIT_OBJECT_0:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected process wait state 0x%x", state)
	}
}

// serviceLockProcessStartedAt reports the recorded process's creation time so
// a reused PID can be distinguished from the original lock owner. Unknown or
// inaccessible start times report ok=false and leave the caller conservative.
func serviceLockProcessStartedAt(pid int) (time.Time, bool, error) {
	if pid <= 0 {
		return time.Time{}, false, nil
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	defer windows.CloseHandle(handle)
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, false, nil
	}
	return time.Unix(0, creation.Nanoseconds()).UTC(), true, nil
}

// serviceLockProcessLooksLikeOwner reports whether the PID's process image
// looks like a PairRoom service. Any uncertainty answers true (conservative);
// only a clearly foreign image supports the reuse conclusion.
func serviceLockProcessLooksLikeOwner(pid int) bool {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return true
	}
	defer windows.CloseHandle(handle)
	var buf [windows.MAX_PATH]uint16
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(handle, 0, &buf[0], &size); err != nil || size == 0 {
		return true
	}
	image := strings.ToLower(filepath.Base(windows.UTF16ToString(buf[:size])))
	if image == "pairroom.exe" {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	return image == strings.ToLower(filepath.Base(self))
}
