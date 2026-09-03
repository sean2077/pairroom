//go:build windows

package execx

import (
	"os/exec"
	"syscall"
)

// createNoWindow prevents Windows from allocating a console for a
// console-subsystem child. HideWindow alone only requests SW_HIDE and still
// permits a console/pseudo-console host to be created by command wrappers.
const createNoWindow = 0x08000000

// NoConsole keeps captured console-subsystem children (native Agent CLIs, git,
// PowerShell scripts) from flashing a console window when the service itself
// runs without one (e.g. under Task Scheduler). Redirected standard streams are
// retained, so JSON protocols and captured output keep working.
func NoConsole(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
