//go:build !windows

package claudewake

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

func validAddress(address string) bool { return filepath.IsAbs(address) }

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
func privateDirectory(info os.FileInfo) bool { return owned(info) && info.Mode().Perm()&0077 == 0 }
func privateFile(_ *os.File, info os.FileInfo) bool {
	return owned(info) && info.Mode().Perm()&0077 == 0
}
func privateTemp(dir string) (*os.File, error) { return os.CreateTemp(dir, ".claude-inbox-*") }

func writeInbox(ctx context.Context, address string, frame []byte) error {
	info, err := os.Lstat(address)
	// Never follow a symlink, write a regular file, or contact another OS user's
	// endpoint. Read/execute bits alone do not grant Unix-socket write access.
	if err != nil || info.Mode()&os.ModeSocket == 0 || !owned(info) || info.Mode().Perm()&0022 != 0 {
		return ErrUnavailable
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", address)
	if err != nil {
		return ErrUnavailable
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_, err = io.Copy(conn, bytes.NewReader(frame))
	return err
}
