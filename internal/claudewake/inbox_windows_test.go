//go:build windows

package claudewake

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

var createNamedPipe = kernel.NewProc("CreateNamedPipeW")
var connectNamedPipe = kernel.NewProc("ConnectNamedPipe")

func newTestPipe(t *testing.T) (string, syscall.Handle) {
	t.Helper()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	address := `\\.\pipe\pairroom-wake-test-` + hex.EncodeToString(nonce)
	path, _ := syscall.UTF16PtrFromString(address)
	// Inbound byte-mode local pipe; a small buffer lets the cancellation test
	// force a pending write without any installed Claude or vendor credentials.
	h, _, err := createNamedPipe.Call(uintptr(unsafe.Pointer(path)), 1|syscall.FILE_FLAG_OVERLAPPED, 0x8, 1, 1024, 1024, 0, 0)
	if syscall.Handle(h) == syscall.InvalidHandle {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.CloseHandle(syscall.Handle(h)) })
	return address, syscall.Handle(h)
}

func pipeIO(ctx context.Context, h syscall.Handle, start func(*syscall.Overlapped, *uint32) error) (uint32, error) {
	event, _, _ := createEvent.Call(0, 1, 0, 0)
	if event == 0 {
		return 0, ErrSend
	}
	defer syscall.CloseHandle(syscall.Handle(event))
	ov := syscall.Overlapped{HEvent: syscall.Handle(event)}
	var count uint32
	err := start(&ov, &count)
	if err != syscall.ERROR_IO_PENDING {
		return count, err
	}
	cancelled := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = syscall.CancelIoEx(h, &ov); close(cancelled) })
	ok, _, _ := getOverlappedResult.Call(uintptr(h), uintptr(unsafe.Pointer(&ov)), uintptr(unsafe.Pointer(&count)), 1)
	if !stop() {
		<-cancelled
	}
	if ok == 0 {
		return count, ErrSend
	}
	return count, nil
}
func connectPipe(ctx context.Context, h syscall.Handle) error {
	_, err := pipeIO(ctx, h, func(ov *syscall.Overlapped, _ *uint32) error {
		ok, _, err := connectNamedPipe.Call(uintptr(h), uintptr(unsafe.Pointer(ov)))
		if ok != 0 || err == syscall.Errno(535) {
			return nil
		}
		return err
	})
	return err
}
func testInbox(t *testing.T) (string, <-chan []byte) {
	t.Helper()
	address, h := newTestPipe(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	received := make(chan []byte, 1)
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() {
		defer close(done)
		if connectPipe(ctx, h) != nil {
			received <- nil
			return
		}
		data := []byte{}
		for bytes.Count(data, []byte{'\n'}) < 2 && len(data) < 16384 {
			buf := make([]byte, 16384)
			n, err := pipeIO(ctx, h, func(ov *syscall.Overlapped, n *uint32) error { return syscall.ReadFile(h, buf, n, ov) })
			data = append(data, buf[:n]...)
			if err != nil {
				break
			}
		}
		received <- data
	}()
	return address, received
}
func TestWindowsRejectsRemotePipeAndInsecureFile(t *testing.T) {
	for _, address := range []string{`\\server\pipe\inbox`, `C:\file`, `\\?\pipe\inbox`, `\\.\pipe\`, `\\.\pipe\x:y`} {
		if validAddress(address) {
			t.Fatal("non-local/invalid pipe accepted")
		}
	}
	dir := privateDir(t)
	// Deliberately inherited permissions are not accepted as a wake capability.
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	if privateFile(f, info) {
		t.Fatal("inherited DACL passed protected owner-only check")
	}
}
func TestWindowsBlockedPipeWriteCanBeCancelled(t *testing.T) {
	address, h := newTestPipe(t)
	serverCtx, stopServer := context.WithCancel(context.Background())
	defer stopServer()
	connected := make(chan struct{})
	done := make(chan struct{})
	go func() { defer close(done); _ = connectPipe(serverCtx, h); close(connected); <-serverCtx.Done() }()
	defer func() { stopServer(); <-done }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := writeInbox(ctx, address, make([]byte, 8<<20))
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatal("pipe write did not honor cancellation")
	}
	select {
	case <-connected:
	case <-time.After(time.Second):
		t.Fatal("pipe did not connect")
	}
}
