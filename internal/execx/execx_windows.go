//go:build windows

package execx

import (
	"os/exec"
	"syscall"
)

// NoConsole keeps captured console-subsystem children (native Agent CLIs, git,
// PowerShell scripts) from flashing a console window when the service itself
// runs without one (e.g. under Task Scheduler), while still giving the child a
// hidden console that its own console-subsystem descendants inherit.
//
// Deliberately uses STARTF_USESHOWWINDOW + SW_HIDE instead of CREATE_NO_WINDOW:
// with CREATE_NO_WINDOW the child has no console at all, so every console
// descendant (vendor CLI MCP servers, npx shims, git helpers) allocates its
// own visible console window. A hidden-but-present console is inherited down
// the whole process tree, keeping every descendant windowless on the desktop.
func NoConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
}
