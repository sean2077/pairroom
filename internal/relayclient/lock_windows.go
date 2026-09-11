//go:build windows

package relayclient

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/sean2077/pairroom/internal/relay"
)

// A named, process-owned kernel mutex has no filesystem footprint and remains
// stable when credentials/state are atomically replaced. Abandoned is acquired.
func lockSlot(ctx context.Context, dir string) (func(), error) {
	kernel := syscall.NewLazyDLL("kernel32.dll")
	name, err := syscall.UTF16PtrFromString("Local\\PairRoomRelay-" + relay.Digest(strings.ToLower(filepath.Clean(dir))))
	if err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	handle, _, callErr := kernel.NewProc("CreateMutexW").Call(0, 0, uintptr(unsafe.Pointer(name)))
	if handle == 0 {
		runtime.UnlockOSThread()
		return nil, callErr
	}
	closeHandle := func() { _, _, _ = kernel.NewProc("CloseHandle").Call(handle); runtime.UnlockOSThread() }
	for {
		result, _, callErr := kernel.NewProc("WaitForSingleObject").Call(handle, 10)
		switch result {
		case 0, 0x80:
			return func() { _, _, _ = kernel.NewProc("ReleaseMutex").Call(handle); closeHandle() }, nil
		case 0x102:
		default:
			closeHandle()
			if callErr != syscall.Errno(0) {
				return nil, callErr
			}
			return nil, errors.New("relay mutex wait failed")
		}
		if err := ctx.Err(); err != nil {
			closeHandle()
			return nil, err
		}
	}
}
