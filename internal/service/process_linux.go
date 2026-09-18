//go:build linux

package service

import (
	"errors"
	"os"
	"strconv"
	"strings"
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

// serviceLockProcessStartedAt derives the process creation wall time from
// /proc/<pid>/stat field 22 (starttime, USER_HZ ticks since boot) plus
// /proc/stat btime. USER_HZ is 100 per the documented procfs ABI; the
// caller's reuse tolerance absorbs any residual error. Any unreadable or
// vanished input reports ok=false and keeps the caller conservative.
func serviceLockProcessStartedAt(pid int) (time.Time, bool, error) {
	if pid <= 0 {
		return time.Time{}, false, nil
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return time.Time{}, false, nil
	}
	text := string(data)
	closing := strings.LastIndexByte(text, ')')
	if closing < 0 {
		return time.Time{}, false, nil
	}
	fields := strings.Fields(text[closing+1:])
	// fields[0] is stat field 3 (state), so starttime (field 22) is index 19.
	if len(fields) < 20 {
		return time.Time{}, false, nil
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil || ticks < 0 {
		return time.Time{}, false, nil
	}
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, false, nil
	}
	var btime int64
	for _, line := range strings.Split(string(stat), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			btime, _ = strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			break
		}
	}
	if btime <= 0 {
		return time.Time{}, false, nil
	}
	const userHZ = 100
	seconds := btime + ticks/userHZ
	nanos := (ticks % userHZ) * int64(time.Second/userHZ)
	return time.Unix(seconds, nanos).UTC(), true, nil
}
