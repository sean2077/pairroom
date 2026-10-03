//go:build !windows

package agent

import (
	"io"
	"os/exec"
)

func nativeStdinPipe(cmd *exec.Cmd) (io.WriteCloser, func(), error) {
	stdin, err := cmd.StdinPipe()
	return stdin, func() {}, err
}
