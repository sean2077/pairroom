//go:build windows

package daemon

import "golang.org/x/sys/windows"

const swHide = windows.SW_HIDE

// x/sys does not wrap these console/window entry points. Resolve them only
// from System32, rather than searching the current directory for DLLs.
var (
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	user32               = windows.NewLazySystemDLL("user32.dll")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procFreeConsole      = kernel32.NewProc("FreeConsole")
	procShowWindow       = user32.NewProc("ShowWindow")
)

// detachOwnedWindowsConsole hides and releases a console allocated for the
// current process. Windows console-subsystem binaries still get a conhost /
// default-terminal window when Task Scheduler launches them via wscript
// shell.Run(..., 0), so the daemon must drop that window itself.
func detachOwnedWindowsConsole() {
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd != 0 {
		_, _, _ = procShowWindow.Call(hwnd, uintptr(swHide))
	}
	_, _, _ = procFreeConsole.Call()
}
