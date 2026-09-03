//go:build windows

package execx

import (
	"os/exec"
	"testing"
)

const createNoWindowFlag = 0x08000000

func TestNoConsoleHidesButKeepsInheritableConsole(t *testing.T) {
	cmd := exec.Command("pairroom-test-child")
	NoConsole(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatal("child process was not configured with HideWindow")
	}
	// CREATE_NO_WINDOW would leave the child without any console, causing each
	// console-subsystem descendant (MCP servers, npx shims) to allocate its own
	// visible console window. We must keep a hidden, inheritable console instead.
	if cmd.SysProcAttr.CreationFlags&createNoWindowFlag != 0 {
		t.Fatal("child process must not use CREATE_NO_WINDOW; descendants need an inheritable hidden console")
	}
}
