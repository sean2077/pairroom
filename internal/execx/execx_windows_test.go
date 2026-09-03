//go:build windows

package execx

import (
	"os/exec"
	"testing"
)

func TestNoConsoleHidesWindowsConsole(t *testing.T) {
	cmd := exec.Command("pairroom-test-child")
	NoConsole(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatal("child process was not configured with HideWindow")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatal("child process was not configured with CREATE_NO_WINDOW")
	}
}
