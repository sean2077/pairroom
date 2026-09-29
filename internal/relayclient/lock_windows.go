//go:build windows

package relayclient

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/sean2077/pairroom/internal/relay"
)

// A named, process-owned kernel mutex has no filesystem footprint and remains
// stable when credentials/state are atomically replaced. Abandoned is acquired.
func lockSlot(ctx context.Context, dir string) (func(), error) {
	name, err := windows.UTF16PtrFromString("Local\\PairRoomRelay-" + relay.Digest(strings.ToLower(filepath.Clean(dir))))
	if err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	handle, err := windows.CreateMutex(nil, false, name)
	// ERROR_ALREADY_EXISTS accompanies a valid handle for an existing mutex.
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		runtime.UnlockOSThread()
		return nil, err
	}
	closeHandle := func() { _ = windows.CloseHandle(handle); runtime.UnlockOSThread() }
	for {
		result, err := windows.WaitForSingleObject(handle, 10)
		switch result {
		case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
			return func() { _ = windows.ReleaseMutex(handle); closeHandle() }, nil
		case uint32(windows.WAIT_TIMEOUT):
		default:
			closeHandle()
			if err != nil {
				return nil, err
			}
			return nil, errors.New("relay mutex wait failed")
		}
		if err := ctx.Err(); err != nil {
			closeHandle()
			return nil, err
		}
	}
}
