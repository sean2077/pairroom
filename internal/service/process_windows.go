//go:build windows

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION (0x1000).
// The frozen dependency closure is stdlib-only, and Go's syscall package does
// not export this constant; GetProcessTimes accepts this access right.
const processQueryLimitedInformation = 0x1000

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

// serviceLockProcessStartedAt reports the recorded process's creation time so
// a reused PID can be distinguished from the original lock owner. Unknown or
// inaccessible start times report ok=false and leave the caller conservative.
func serviceLockProcessStartedAt(pid int) (time.Time, bool, error) {
	if pid <= 0 {
		return time.Time{}, false, nil
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		if errors.Is(err, syscall.Errno(87)) || errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) || errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	defer syscall.CloseHandle(handle)
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return time.Time{}, false, nil
	}
	return time.Unix(0, creation.Nanoseconds()).UTC(), true, nil
}

// serviceLockProcessLooksLikeOwner reports whether the PID's process image
// looks like a PairRoom service. Any uncertainty answers true (conservative);
// only a clearly foreign image supports the reuse conclusion.
func serviceLockProcessLooksLikeOwner(pid int) bool {
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return true
	}
	defer syscall.CloseHandle(handle)
	query := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")
	var buf [syscall.MAX_PATH]uint16
	size := uint32(len(buf))
	ok, _, _ := query.Call(uintptr(handle), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ok == 0 || size == 0 {
		return true
	}
	image := strings.ToLower(filepath.Base(syscall.UTF16ToString(buf[:size])))
	if image == "pairroom.exe" {
		return true
	}
	self, err := os.Executable()
	if err != nil {
		return true
	}
	return image == strings.ToLower(filepath.Base(self))
}
