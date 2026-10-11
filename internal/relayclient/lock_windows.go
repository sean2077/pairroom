//go:build windows

package relayclient

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/windows"

	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relay"
)

// A named, process-owned kernel mutex has no filesystem footprint and remains
// stable when credentials/state are atomically replaced. Abandoned is acquired.
func lockSlot(ctx context.Context, dir string) (func(), error) {
	if directLANLockDirectory(dir) {
		if err := privatefile.CheckDirectory(dir); err != nil {
			return nil, err
		}
		return privatelock.Lock(ctx, dir)
	}
	name, err := windows.UTF16PtrFromString("Local\\PairRoomRelay-" + relay.Digest(strings.ToLower(filepath.Clean(dir))))
	if err != nil {
		return nil, err
	}
	runtime.LockOSThread()
	handle, err := windows.CreateMutex(nil, false, name)
	// ERROR_ALREADY_EXISTS accompanies a valid handle for an existing mutex.
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		runtime.UnlockOSThread()
		return nil, err
	}
	closeHandle := func() { _ = windows.CloseHandle(handle); runtime.UnlockOSThread() }
	for {
		result, err := windows.WaitForSingleObject(handle, 10)
		switch result {
		case windows.WAIT_OBJECT_0, windows.WAIT_ABANDONED:
			return func() { _ = windows.ReleaseMutex(handle); closeHandle() }, nil
		case uint32(windows.WAIT_TIMEOUT):
		default:
			closeHandle()
			if err != nil {
				return nil, err
			}
			return nil, errors.New("relay mutex wait failed")
		}
		if err := ctx.Err(); err != nil {
			closeHandle()
			return nil, err
		}
	}
}

// directLANLockDirectory recognizes the LAN transport directories whose slot
// lock and credentials require the owner-private boundary: the joined slot
// directory .pairroom/rooms/lan_<id>/slots/<slot> and the join-attempt
// directory .pairroom/lan-joins/lan_<id>.
func directLANLockDirectory(dir string) bool {
	dir = strings.ToLower(filepath.Clean(dir))
	if !filepath.IsAbs(dir) {
		return false
	}
	parent := filepath.Dir(dir)
	if filepath.Base(parent) == "lan-joins" && strings.HasPrefix(filepath.Base(dir), "lan_") {
		return filepath.Base(filepath.Dir(parent)) == ".pairroom"
	}
	room := filepath.Dir(parent)
	return (filepath.Base(dir) == "slot1" || filepath.Base(dir) == "slot2") && filepath.Base(parent) == "slots" && strings.HasPrefix(filepath.Base(room), "lan_") && filepath.Base(filepath.Dir(room)) == "rooms" && filepath.Base(filepath.Dir(filepath.Dir(room))) == ".pairroom"
}

// lockSlotRecovery takes the same mutual-exclusion lock as lockSlot without
// requiring the sensitive-directory boundary first. The documented offline
// retirement removes the credentials instead of using them, so a workspace that
// lost its owner DACL (restored from a backup, copied from another machine)
// must remain retirable; concurrent normal users of the directory hold the same
// lock, so exclusion is unchanged.
func lockSlotRecovery(ctx context.Context, dir string) (func(), error) {
	if directLANLockDirectory(dir) {
		return privatelock.Lock(ctx, dir)
	}
	return lockSlot(ctx, dir)
}
