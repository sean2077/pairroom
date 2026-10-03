//go:build windows

package agent

import (
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsNativeStdinIsPollable(t *testing.T) {
	cmd := &exec.Cmd{}
	stdin, release, err := nativeStdinPipe(cmd)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	defer stdin.Close()
	// Deadlines require a pollable handle. A synchronous WriteFile can miss
	// concurrent Close cancellation and strand both Write and Close forever.
	if err := stdin.(*os.File).SetWriteDeadline(time.Time{}); err != nil {
		t.Fatalf("native stdin cannot use cancellable overlapped I/O: %v", err)
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(cmd.Stdin.(*os.File).Fd()), windows.SE_KERNEL_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("native stdin DACL is not protected")
	}
	acl, defaulted, err := descriptor.DACL()
	if err != nil || defaulted || acl == nil || acl.AceCount != 1 {
		t.Fatal("native stdin does not have one explicit owner ACE")
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(acl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !windows.EqualSid((*windows.SID)(unsafe.Pointer(&ace.SidStart)), user.User.Sid) {
		t.Fatal("native stdin grants access beyond the current owner")
	}
	// With no started process, startup cleanup must release both endpoints.
	release()
	if _, err := stdin.Write([]byte("unsent")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed startup retained its writer: %v", err)
	}
	if _, err := cmd.Stdin.(*os.File).Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("failed startup retained its reader: %v", err)
	}
}
