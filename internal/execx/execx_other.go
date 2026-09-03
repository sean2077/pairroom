//go:build !windows

package execx

import "os/exec"

// NoConsole is intentionally a no-op on Unix-like platforms: there is no
// console-allocation behavior to suppress for captured child processes.
func NoConsole(*exec.Cmd) {}
