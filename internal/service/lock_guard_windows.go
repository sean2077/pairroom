//go:build windows

package service

import (
	"errors"
	"fmt"
	"runtime"

	"golang.org/x/sys/windows"
)

// Use the directory's filesystem identity, so case, junction and path aliases
// share one guard. Global makes it common to desktop and daemon sessions. The
// mutex is thread-owned, so pin acquisition through release to one OS thread.
// No persistent guard file is needed; abandoned ownership is safe to acquire
// because the caller still validates the existing PID/nonce metadata.
func lockServiceRoot(root string) (func(), error) {
	path, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return nil, err
	}
	directory, err := windows.CreateFile(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(directory)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(directory, &info); err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(fmt.Sprintf("Global\\PairRoomServiceRoot-%08x-%08x%08x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow))
	if err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	handle, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		runtime.UnlockOSThread()
		return nil, err
	}
	closeHandle := func() { _ = windows.CloseHandle(handle); runtime.UnlockOSThread() }
	result, err := windows.WaitForSingleObject(handle, windows.INFINITE)
	if result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED {
		closeHandle()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected Service root mutex wait state 0x%x", result)
	}
	return func() { _ = windows.ReleaseMutex(handle); closeHandle() }, nil
}
