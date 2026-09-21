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
	"syscall"
	"unsafe"
)

var (
	kernel              = syscall.NewLazyDLL("kernel32.dll")
	createEvent         = kernel.NewProc("CreateEventW")
	getOverlappedResult = kernel.NewProc("GetOverlappedResult")
	getPipeServerPID    = kernel.NewProc("GetNamedPipeServerProcessId")
	advapi              = syscall.NewLazyDLL("advapi32.dll")
	stringToSecurity    = advapi.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	securityToString    = advapi.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	getObjectSecurity   = advapi.NewProc("GetKernelObjectSecurity")
)

func validAddress(address string) bool {
	const prefix = `\\.\pipe\`
	return len(address) > len(prefix) && strings.EqualFold(address[:len(prefix)], prefix) && !strings.ContainsAny(address[len(prefix):], "/:\x00\r\n")
}

// The capability file has a protected, owner-only DACL even when its parent
// inherits broader workspace permissions. Windows chmod is not an ACL check.
func privateDirectory(_ os.FileInfo) bool { return true }

const ownerAndDACL = 0x1 | 0x4

func securityString(sd *byte) (string, error) {
	var text *uint16
	var length uint32
	ok, _, _ := securityToString.Call(uintptr(unsafe.Pointer(sd)), 1, ownerAndDACL, uintptr(unsafe.Pointer(&text)), uintptr(unsafe.Pointer(&length)))
	if ok == 0 {
		return "", ErrUnavailable
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(text)))
	return syscall.UTF16ToString(unsafe.Slice(text, int(length))), nil
}

func ownerSecurity() (*byte, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return nil, ErrUnavailable
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, ErrUnavailable
	}
	sid, err := user.User.Sid.String()
	if err != nil {
		return nil, ErrUnavailable
	}
	text, err := syscall.UTF16PtrFromString("O:" + sid + "D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, ErrUnavailable
	}
	var sd *byte
	ok, _, _ := stringToSecurity.Call(uintptr(unsafe.Pointer(text)), 1, uintptr(unsafe.Pointer(&sd)), 0)
	if ok == 0 {
		return nil, ErrUnavailable
	}
	return sd, nil
}

func privateTemp(dir string) (*os.File, error) {
	sd, err := ownerSecurity()
	if err != nil {
		return nil, err
	}
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(sd)))
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return nil, ErrUnavailable
	}
	name := filepath.Join(dir, ".claude-inbox-"+hex.EncodeToString(random))
	path, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, ErrUnavailable
	}
	attributes := syscall.SecurityAttributes{Length: uint32(unsafe.Sizeof(syscall.SecurityAttributes{})), SecurityDescriptor: uintptr(unsafe.Pointer(sd))}
	h, err := syscall.CreateFile(path, syscall.GENERIC_WRITE, 0, &attributes, syscall.CREATE_NEW, syscall.FILE_ATTRIBUTE_NORMAL, 0)
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
	defer syscall.LocalFree(syscall.Handle(unsafe.Pointer(sd)))
	want, err := securityString(sd)
	if err != nil {
		return false
	}
	var size uint32
	getObjectSecurity.Call(f.Fd(), ownerAndDACL, 0, 0, uintptr(unsafe.Pointer(&size)))
	if size == 0 || size > 65536 {
		return false
	}
	buffer := make([]byte, size)
	ok, _, _ := getObjectSecurity.Call(f.Fd(), ownerAndDACL, uintptr(unsafe.Pointer(&buffer[0])), uintptr(size), uintptr(unsafe.Pointer(&size)))
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
	path, err := syscall.UTF16PtrFromString(address)
	if err != nil {
		return ErrUnavailable
	}
	// Local pipes only; anonymous SQOS prevents the pipe server impersonating
	// the Service. Overlapped writes keep cancellation bounded without a helper.
	h, err := syscall.CreateFile(path, syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OVERLAPPED|0x00100000, 0)
	if err != nil {
		return ErrUnavailable
	}
	defer syscall.CloseHandle(h)
	if !sameUserPipe(h) {
		return ErrUnavailable
	}
	event, _, _ := createEvent.Call(0, 1, 0, 0)
	if event == 0 {
		return ErrSend
	}
	defer syscall.CloseHandle(syscall.Handle(event))
	overlapped := syscall.Overlapped{HEvent: syscall.Handle(event)}
	var written uint32
	err = syscall.WriteFile(h, frame, &written, &overlapped)
	if err == syscall.ERROR_IO_PENDING {
		cancelled := make(chan struct{})
		stop := context.AfterFunc(ctx, func() {
			_ = syscall.CancelIoEx(h, &overlapped)
			close(cancelled)
		})
		ok, _, _ := getOverlappedResult.Call(uintptr(h), uintptr(unsafe.Pointer(&overlapped)), uintptr(unsafe.Pointer(&written)), 1)
		// Drain cancellation before freeing the handle/OVERLAPPED/buffer.
		if !stop() {
			<-cancelled
		}
		runtime.KeepAlive(frame)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if ok == 0 {
			return ErrSend
		}
		err = nil
	}
	if err != nil || written != uint32(len(frame)) {
		return ErrSend
	}
	return nil
}

// A stale pipe name must not disclose the token to a server owned by another
// OS user. Same-user hostile code remains outside the native trust boundary.
func sameUserPipe(h syscall.Handle) bool {
	var pid uint32
	ok, _, _ := getPipeServerPID.Call(uintptr(h), uintptr(unsafe.Pointer(&pid)))
	if ok == 0 || pid == 0 {
		return false
	}
	process, err := syscall.OpenProcess(0x1000, false, pid) // QUERY_LIMITED_INFORMATION
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(process)
	var token syscall.Token
	if syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token) != nil {
		return false
	}
	defer token.Close()
	owner, err := token.GetTokenUser()
	if err != nil {
		return false
	}
	current, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return false
	}
	defer current.Close()
	user, err := current.GetTokenUser()
	if err != nil {
		return false
	}
	a, err := owner.User.Sid.String()
	if err != nil {
		return false
	}
	b, err := user.User.Sid.String()
	return err == nil && a == b
}
