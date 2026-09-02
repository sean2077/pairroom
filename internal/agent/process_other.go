//go:build !windows

package agent

import "os/exec"

// configureChildProcess is intentionally a no-op on Unix-like platforms;
// their child-process visibility is governed by the host terminal/session.
func configureChildProcess(*exec.Cmd) {}
