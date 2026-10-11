//go:build windows

package relayclient

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestLANDirectWorkspaceLocksUseProtectedGlobalIdentity(t *testing.T) {
	if dir := os.Getenv("PAIRROOM_TEST_DIRECT_WAL_LOCK"); dir != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		unlock, err := lockSlot(ctx, strings.ToUpper(dir))
		if os.Getenv("PAIRROOM_TEST_DIRECT_WAL_BLOCKED") == "1" {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("direct WAL did not share private global mutex: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		return
	}
	for _, parts := range [][]string{{"lan-joins", "lan_" + relay.Digest("attempt")[:32]}, {"rooms", "lan_" + relay.Digest("room")[:32], "slots", "slot2"}} {
		dir, err := secureLANJoinDir(t.TempDir(), parts...)
		if err != nil {
			t.Fatal(err)
		}
		unlock, err := privatelock.Lock(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, blocked := range []bool{true, false} {
			if !blocked {
				unlock()
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestLANDirectWorkspaceLocksUseProtectedGlobalIdentity$")
			cmd.Env = append(os.Environ(), "PAIRROOM_TEST_DIRECT_WAL_LOCK="+dir)
			if blocked {
				cmd.Env = append(cmd.Env, "PAIRROOM_TEST_DIRECT_WAL_BLOCKED=1")
			}
			output, runErr := cmd.CombinedOutput()
			if runErr != nil {
				if blocked {
					unlock()
				}
				t.Fatalf("direct WAL subprocess: %v %s", runErr, output)
			}
		}
	}
}

func TestLANWorkspaceLockRecognitionPreservesLegacyDirectories(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{filepath.Join(root, ".pairroom", "rooms", "local-room", "slots", "slot1"), filepath.Join(root, "lan-clients", "lan_room"), filepath.Join(root, ".pairroom", "rooms", "lan_room", "other", "slot1")} {
		if directLANLockDirectory(path) {
			t.Fatalf("legacy/non-WAL path switched mutex namespace: %q", path)
		}
	}
	path := filepath.Join(root, ".pairroom", "lan-joins", "lan_unprotected")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := lockSlot(context.Background(), path); !errors.Is(err, privatefile.ErrPrivate) {
		t.Fatalf("direct WAL inherited an unprotected Windows directory: %v", err)
	}
}
