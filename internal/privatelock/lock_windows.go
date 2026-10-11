//go:build windows

package privatelock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const mutexOwnerAndDACL = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION

// New per-user client and identity stores are shared across Windows logon
// sessions. Global mutexes coordinate a desktop CLI and a scheduled Service;
// the legacy workspace relay mutex namespace is deliberately unchanged.
// The directory file ID avoids separate locks for case or short-path aliases.
func mutexPolicy(dir string) (string, *windows.SECURITY_DESCRIPTOR, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return "", nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", nil, err
	}
	sid := user.User.Sid.String()
	if sid == "" {
		return "", nil, errors.New("private mutex owner is unavailable")
	}
	// MUTEX_ALL_ACCESS, expressed without generic-rights remapping.
	policy, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;0x001f0001;;;" + sid + ")")
	if err != nil {
		return "", nil, err
	}
	file, err := os.Open(dir)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return "", nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return "", nil, errors.New("private mutex requires a direct directory")
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s:%08x:%08x%08x", sid, info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)))
	return "Global\\PairRoomPrivate-" + hex.EncodeToString(digest[:]), policy, nil
}

func Lock(ctx context.Context, dir string) (func(), error) {
	name, policy, err := mutexPolicy(dir)
	if err != nil {
		return nil, err
	}
	encoded, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: policy}
	runtime.LockOSThread()
	handle, err := windows.CreateMutex(&attributes, false, encoded)
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		runtime.UnlockOSThread()
		return nil, err
	}
	closeHandle := func() { _ = windows.CloseHandle(handle); runtime.UnlockOSThread() }
	// Windows ignores a new descriptor when the named object already exists.
	// Inspect the actual opened handle before trusting or waiting on it.
	actual, err := windows.GetSecurityInfo(handle, windows.SE_KERNEL_OBJECT, mutexOwnerAndDACL)
	if err != nil || actual == nil || policy.String() == "" || actual.String() != policy.String() {
		closeHandle()
		return nil, errors.New("private mutex is not protected for the current user")
	}
	for {
		if err := ctx.Err(); err != nil {
			closeHandle()
			return nil, err
		}
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
			return nil, errors.New("private state mutex wait failed")
		}
	}
}
