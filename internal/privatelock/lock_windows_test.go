//go:build windows

package privatelock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestGlobalPrivateMutexCoordinatesProcesses(t *testing.T) {
	if dir := os.Getenv("PAIRROOM_TEST_PRIVATE_MUTEX_DIRECTORY"); dir != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		unlock, err := Lock(ctx, dir)
		if os.Getenv("PAIRROOM_TEST_PRIVATE_MUTEX_BLOCKED") == "1" {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("another process's mutex did not exclude us: %v", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		unlock()
		return
	}
	dir := t.TempDir()
	name, _, err := mutexPolicy(dir)
	if err != nil || !strings.HasPrefix(name, "Global\\PairRoomPrivate-") {
		t.Fatalf("private per-user store is not coordinated across logon sessions: %v", err)
	}
	unlock, err := Lock(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	held := true
	defer func() {
		if held {
			unlock()
		}
	}()
	for _, blocked := range []bool{true, false} {
		if !blocked {
			unlock()
			held = false
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestGlobalPrivateMutexCoordinatesProcesses$")
		cmd.Env = append(os.Environ(), "PAIRROOM_TEST_PRIVATE_MUTEX_DIRECTORY="+dir)
		if blocked {
			cmd.Env = append(cmd.Env, "PAIRROOM_TEST_PRIVATE_MUTEX_BLOCKED=1")
		}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("private mutex subprocess: %v %s", err, output)
		}
	}
}

func TestGlobalPrivateMutexRejectsExistingBroadDACLAndWrongObject(t *testing.T) {
	for _, kind := range []string{"broad-dacl", "wrong-object"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			name, policy, err := mutexPolicy(dir)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := windows.UTF16PtrFromString(name)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "broad-dacl" {
				policy, err = windows.SecurityDescriptorFromString(policy.String() + "(A;;0x001f0001;;;WD)")
				if err != nil {
					t.Fatal(err)
				}
			}
			attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: policy}
			var handle windows.Handle
			if kind == "broad-dacl" {
				handle, err = windows.CreateMutex(&attributes, false, encoded)
			} else {
				handle, err = windows.CreateEvent(&attributes, 0, 0, encoded)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer windows.CloseHandle(handle)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			unlock, err := Lock(ctx, dir)
			if err == nil {
				unlock()
				t.Fatal("preexisting unsafe global object was trusted")
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("unsafe object was waited on before validating its policy")
			}
		})
	}
}
