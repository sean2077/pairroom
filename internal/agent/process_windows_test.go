//go:build windows

package agent

import (
	"os/exec"
	"testing"
)

func TestConfigureChildProcessHidesWindowsConsole(t *testing.T) {
	cmd := exec.Command("pairroom-test-child")
	configureChildProcess(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow {
		t.Fatal("Agent child process was not configured with HideWindow")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatal("Agent child process was not configured with CREATE_NO_WINDOW")
	}
}
