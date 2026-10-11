//go:build !windows

package privatefile

import (
	"os"
	"syscall"
)

func makeDirectory(path string) error { return os.Mkdir(path, 0o700) }

func privateDirectory(_ string, info os.FileInfo) bool { return privateInfo(info) }

func privateInfo(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && info.Mode().Perm()&0o077 == 0
}

func privateFile(_ *os.File, info os.FileInfo) bool { return privateInfo(info) }

func privateTemp(dir string) (*os.File, error) {
	return os.CreateTemp(dir, ".identity-*")
}
