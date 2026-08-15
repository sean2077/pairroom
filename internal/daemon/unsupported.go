//go:build !linux && !darwin && !windows

package daemon

import (
	"fmt"
	"runtime"
)

func newPlatformManager() (Manager, error) {
	return nil, fmt.Errorf("daemon management is not supported on %s", runtime.GOOS)
}

func CheckLinger() (bool, string) { return true, "" }
