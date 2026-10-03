//go:build windows

package agent

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func nativeStdinPipe(cmd *exec.Cmd) (io.WriteCloser, func(), error) {
	if cmd.Stdin != nil || cmd.Process != nil {
		return nil, nil, errors.New("native stdin must be configured before process start")
	}
	// Anonymous Windows pipes use synchronous WriteFile. Close can miss an
	// operation that starts just after its single CancelIoEx call. Only the
	// parent writer is overlapped; the child keeps ordinary synchronous stdin.
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, fmt.Errorf("allocate native stdin identity: %w", err)
	}
	name, err := windows.UTF16PtrFromString(fmt.Sprintf(`\\.\pipe\pairroom-stdin-%x`, nonce))
	if err != nil {
		return nil, nil, err
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, fmt.Errorf("read native stdin owner: %w", err)
	}
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		return nil, nil, fmt.Errorf("protect native stdin: %w", err)
	}
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	child, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_INBOUND|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS, 1, 0, 64*1024, 0, &security)
	if err != nil {
		return nil, nil, fmt.Errorf("create native stdin: %w", err)
	}
	childOwned := true
	defer func() {
		if childOwned {
			_ = windows.CloseHandle(child)
		}
	}()
	parent, err := windows.CreateFile(name, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("connect native stdin writer: %w", err)
	}
	if err := windows.ConnectNamedPipe(child, nil); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		_ = windows.CloseHandle(parent)
		return nil, nil, fmt.Errorf("connect native stdin reader: %w", err)
	}
	reader := os.NewFile(uintptr(child), "native-stdin-child")
	writer := os.NewFile(uintptr(parent), "native-stdin-parent")
	childOwned = false
	if err := writer.SetWriteDeadline(time.Time{}); err != nil {
		_ = reader.Close()
		_ = writer.Close()
		return nil, nil, fmt.Errorf("native stdin writer is not pollable: %w", err)
	}
	cmd.Stdin = reader
	return writer, func() {
		// os/exec duplicates this externally supplied handle for the child.
		// It does not own our original reader or writer; release both on failed
		// startup, and let waitProcess close the captured writer after exit.
		_ = reader.Close()
		if cmd.Process == nil {
			_ = writer.Close()
		}
	}, nil
}
