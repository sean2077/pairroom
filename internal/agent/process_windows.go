//go:build windows

package agent

import (
	"os/exec"
	"syscall"
)

// CREATE_NO_WINDOW prevents Windows from allocating a console for a
// console-subsystem child. HideWindow alone only requests SW_HIDE and still
// permits a console/pseudo-console host to be created by command wrappers.
const createNoWindow = 0x08000000

// configureChildProcess keeps native Agent CLIs out of the taskbar while
// retaining their redirected standard streams for the JSON protocols. The
// desktop host is a GUI process, so Windows otherwise creates a visible
// console/pseudo-console window for console-subsystem children.
func configureChildProcess(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
