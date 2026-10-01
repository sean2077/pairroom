//go:build !windows

package relayclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func platformProcessBirth(pid int) (string, error) {
	id := strconv.Itoa(pid)
	if data, err := os.ReadFile("/proc/" + id + "/stat"); err == nil {
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return "", errors.New("invalid process stat")
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 20 {
			return "", errors.New("incomplete process stat")
		}
		boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(boot)) + ":" + fields[19], nil
	}
	// macOS/BSD: lstart is stable for the lifetime of a process and includes
	// the full calendar date, unlike elapsed time or the process name alone.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "ps", "-p", id, "-o", "lstart=").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
