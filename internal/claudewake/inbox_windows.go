//go:build windows

package claudewake

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// These entry points are used only by the existing Windows pipe-server tests.
// Production event and overlapped I/O calls use the typed windows APIs below.
var (
	kernel              = windows.NewLazySystemDLL("kernel32.dll")
	createEvent         = kernel.NewProc("CreateEventW")
	getOverlappedResult = kernel.NewProc("GetOverlappedResult")
)

// x/sys does not export GetKernelObjectSecurity or an error-returning SDDL
// formatter restricted to owner and DACL. Keep these exact operations and load
// their entry points only from System32.
var (
	advapi            = windows.NewLazySystemDLL("advapi32.dll")
	securityToString  = advapi.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	getObjectSecurity = advapi.NewProc("GetKernelObjectSecurity")
)

func validAddress(address string) bool {
	const prefix = `\\.\pipe\`
	return len(address) > len(prefix) && strings.EqualFold(address[:len(prefix)], prefix) && !strings.ContainsAny(address[len(prefix):], "/:\x00\r\n")
}

// The capability file has a protected, owner-only DACL even when its parent
// inherits broader workspace permissions. Windows chmod is not an ACL check.
func privateDirectory(_ os.FileInfo) bool { return true }

const ownerAndDACL = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION

func securityString(sd *byte) (string, error) {
	var text *uint16
	var length uint32
	ok, _, _ := securityToString.Call(uintptr(unsafe.Pointer(sd)), 1, uintptr(ownerAndDACL), uintptr(unsafe.Pointer(&text)), uintptr(unsafe.Pointer(&length)))
	if ok == 0 {
		return "", ErrUnavailable
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(text)))
	return windows.UTF16ToString(unsafe.Slice(text, int(length))), nil
}

func ownerSecurity() (*windows.SECURITY_DESCRIPTOR, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return nil, ErrUnavailable
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, ErrUnavailable
	}
	// SID.String reports conversion failure as an empty string; stay fail-closed.
	sid := user.User.Sid.String()
	if sid == "" {
		return nil, ErrUnavailable
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, ErrUnavailable
	}
	return sd, nil
}

func privateTemp(dir string) (*os.File, error) {
	sd, err := ownerSecurity()
	if err != nil {
		return nil, err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, ErrUnavailable
	}
	name := filepath.Join(dir, ".claude-inbox-"+hex.EncodeToString(random))
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, ErrUnavailable
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateFile(path, windows.GENERIC_WRITE, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	return os.NewFile(uintptr(h), name), nil
}

func privateFile(f *os.File, _ os.FileInfo) bool {
	sd, err := ownerSecurity()
	if err != nil {
		return false
	}
	want, err := securityString((*byte)(unsafe.Pointer(sd)))
	if err != nil {
		return false
	}
	var size uint32
	getObjectSecurity.Call(f.Fd(), uintptr(ownerAndDACL), 0, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 || size > 65536 {
		return false
	}
	buffer := make([]byte, size)
	ok, _, _ := getObjectSecurity.Call(f.Fd(), uintptr(ownerAndDACL), uintptr(unsafe.Pointer(&buffer[0])), uintptr(size), uintptr(unsafe.Pointer(&size)))
	if ok == 0 {
		return false
	}
	got, err := securityString(&buffer[0])
	return err == nil && got == want
}

func writeInbox(ctx context.Context, address string, frame []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !validAddress(address) || len(frame) == 0 {
		return ErrUnavailable
	}
	path, err := windows.UTF16PtrFromString(address)
	if err != nil {
		return ErrUnavailable
	}
	// Local pipes only; anonymous SQOS prevents the pipe server impersonating
	// the Service. Overlapped writes keep cancellation bounded without a helper.
	h, err := windows.CreateFile(path, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED|windows.SECURITY_SQOS_PRESENT, 0)
	if err != nil {
		return ErrUnavailable
	}
	defer windows.CloseHandle(h)
	if !sameUserPipe(h) {
		return ErrUnavailable
	}
	event, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		return ErrSend
	}
	defer windows.CloseHandle(event)
	overlapped := windows.Overlapped{HEvent: event}
	var written uint32
	err = windows.WriteFile(h, frame, &written, &overlapped)
	if err == windows.ERROR_IO_PENDING {
		cancelled := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = windows.CancelIoEx(h, &overlapped)
			close(cancelled)
		})
		err = windows.GetOverlappedResult(h, &overlapped, &written, true)
		// Drain cancellation before freeing the handle/OVERLAPPED/buffer.
		if !stop() {
			<-cancelled
		}
		runtime.KeepAlive(frame)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return ErrSend
		}
	}
	if err != nil || written != uint32(len(frame)) {
		return ErrSend
	}
	return nil
}

// A stale pipe name must not disclose the token to a server owned by another
// OS user. Same-user hostile code remains outside the native trust boundary.
func sameUserPipe(h windows.Handle) bool {
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(h, &pid); err != nil || pid == 0 {
		return false
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token) != nil {
		return false
	}
	defer token.Close()
	owner, err := token.GetTokenUser()
	if err != nil {
		return false
	}
	var current windows.Token
	if windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &current) != nil {
		return false
	}
	defer current.Close()
	user, err := current.GetTokenUser()
	if err != nil {
		return false
	}
	a := owner.User.Sid.String()
	if a == "" {
		return false
	}
	b := user.User.Sid.String()
	return b != "" && a == b
}
