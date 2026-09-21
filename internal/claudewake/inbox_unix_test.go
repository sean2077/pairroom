//go:build !windows

package claudewake

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testInbox(t *testing.T) (string, <-chan []byte) {
	t.Helper()
	dir, err := os.MkdirTemp("", "prw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	address := filepath.Join(dir, "inbox.sock")
	listener, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	received := make(chan []byte, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		data, _ := io.ReadAll(io.LimitReader(conn, 16384))
		received <- data
	}()
	return address, received
}
func TestUnixRejectsInsecureAndSymlinkCapabilities(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, FileName)
	if err := Capture(dir, testIdentity, testAddress(), testToken); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(dir, testIdentity); !errors.Is(err, ErrUnavailable) {
		t.Fatal("public capability accepted")
	}
	target := filepath.Join(dir, "target")
	if err := os.Rename(path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(dir, testIdentity); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := Capture(dir, testIdentity, testAddress(), testToken); err == nil {
		t.Fatal("capture followed symlink")
	}
	// Invalid env removes only the symlink, not its target.
	if err := Capture(dir, testIdentity, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("target removed")
	}
	root := privateDir(t)
	slot := filepath.Join(root, ".pairroom", "rooms", "room", "slots", "slot1")
	if err := os.MkdirAll(slot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, ".pairroom"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := SlotDir(root, "room", "slot1"); err == nil {
		t.Fatal("public parent accepted")
	}
}
func TestUnixNeverWritesRegularFileOrSymlinkEndpoint(t *testing.T) {
	dir := privateDir(t)
	path := filepath.Join(dir, "not-a-socket")
	original := []byte("keep this file unchanged")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeInbox(context.Background(), path, []byte("no")); err == nil {
		t.Fatal("regular file accepted")
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, original) {
		t.Fatal("regular file modified")
	}
	address, _ := testInbox(t)
	link := filepath.Join(dir, "symlink")
	if err := os.Symlink(address, link); err != nil {
		t.Fatal(err)
	}
	if err := writeInbox(context.Background(), link, []byte("no")); err == nil {
		t.Fatal("socket symlink accepted")
	}
	if err := os.Chmod(address, 0777); err != nil {
		t.Fatal(err)
	}
	if err := writeInbox(context.Background(), address, []byte("no")); err == nil {
		t.Fatal("world-writable socket accepted")
	}
}
func TestUnixBlockedWriteHonorsDeadline(t *testing.T) {
	dir, err := os.MkdirTemp("", "prw-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	address := filepath.Join(dir, "slow.sock")
	l, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		c, e := l.Accept()
		if e == nil {
			defer c.Close()
			<-done
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = writeInbox(ctx, address, make([]byte, 8<<20))
	if err == nil || time.Since(start) > time.Second {
		t.Fatal("write was not bounded")
	}
}
