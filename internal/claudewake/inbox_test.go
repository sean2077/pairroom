package claudewake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var testIdentity = Identity{BindID: "binding", Generation: 3, SessionID: "session-private"}

const testToken = "secret-should-never-be-logged"

func testAddress() string {
	if runtime.GOOS == "windows" {
		return `\\.\pipe\pairroom-unit-test`
	}
	return "/tmp/pairroom-unit-test.sock"
}
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}
func TestCapabilityIdentityAndRevocation(t *testing.T) {
	dir := privateDir(t)
	if _, err := Prepare(dir, testIdentity); !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing capability did not fail closed")
	}
	if err := Capture(dir, testIdentity, testAddress(), testToken); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(dir, testIdentity); err != nil {
		t.Fatal(err)
	}
	for _, id := range []Identity{{"other", 3, "session-private"}, {"binding", 4, "session-private"}, {"binding", 3, "other"}} {
		if _, err := Prepare(dir, id); !errors.Is(err, ErrUnavailable) {
			t.Fatal("mismatched binding accepted")
		}
	}
	if err := Capture(dir, testIdentity, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); !os.IsNotExist(err) {
		t.Fatal("stale capability retained")
	}
	for _, c := range []capability{
		{testIdentity, testAddress(), ""}, {testIdentity, "relative", testToken},
		{testIdentity, testAddress(), "bad\nframe"}, {Identity{}, testAddress(), testToken},
	} {
		if err := Capture(dir, c.Identity, c.Address, c.Token); !errors.Is(err, ErrUnavailable) {
			t.Fatal("invalid environment accepted")
		}
		if _, err := Prepare(dir, testIdentity); !errors.Is(err, ErrUnavailable) {
			t.Fatal("invalid environment left usable capability")
		}
	}
}
func TestCapabilityRefreshAndBoundedPrivateRead(t *testing.T) {
	dir := privateDir(t)
	for i := 0; i < 3; i++ {
		if err := Capture(dir, testIdentity, testAddress(), testToken); err != nil {
			t.Fatal(err)
		}
		if _, err := Prepare(dir, testIdentity); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != FileName {
		t.Fatal("temporary secret file leaked")
	}
	path := filepath.Join(dir, FileName)
	// An identical capability is not rewritten on every Stop; a rotated token
	// and a tampered file are.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Capture(dir, testIdentity, testAddress(), testToken); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(path); err != nil || !os.SameFile(before, after) {
		t.Fatal("unchanged capability was rewritten")
	}
	if err := Capture(dir, testIdentity, testAddress(), testToken+"-rotated"); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(path); err != nil || os.SameFile(before, after) {
		t.Fatal("rotated capability was not rewritten")
	}
	for _, text := range []string{"not-json", strings.Repeat(" ", 16385)} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Prepare(dir, testIdentity); !errors.Is(err, ErrUnavailable) {
			t.Fatal("unbounded/malformed capability accepted")
		}
	}
}
func TestSlotDirIsBoundToPrivateWorkspace(t *testing.T) {
	root := privateDir(t)
	dir := filepath.Join(root, ".pairroom", "rooms", "room-1", "slots", "slot2")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := SlotDir(root, "room-1", "slot2")
	if err != nil || got != dir {
		t.Fatal("cannot resolve private slot", err)
	}
	for _, room := range []string{"..", "../room-1", "a/b", `a\b`, ""} {
		if _, err := SlotDir(root, room, "slot2"); err == nil {
			t.Fatal("unsafe room accepted")
		}
	}
	if _, err := SlotDir(root, "room-1", "claude"); err == nil {
		t.Fatal("runtime used as durable slot")
	}
	if _, err := SlotDir("relative", "room-1", "slot2"); err == nil {
		t.Fatal("relative root accepted")
	}
}
func TestInboxSubmitsAuthThenNudgeWithoutWaitingForAck(t *testing.T) {
	address, received := testInbox(t)
	dir := privateDir(t)
	if err := Capture(dir, testIdentity, address, testToken); err != nil {
		t.Fatal(err)
	}
	send, err := Prepare(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	const nudge = "PairRoom inbox has messages for you. Run: pairroom relay wait"
	if err := send(ctx, nudge); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-received:
		lines := strings.Split(strings.TrimSuffix(string(frame), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("expected exactly 2 frames; got %d", len(lines))
		}
		var auth struct{ Type, Token string }
		var msg struct {
			Type    string
			Message struct{ Role, Content string }
		}
		if json.Unmarshal([]byte(lines[0]), &auth) != nil || auth.Type != "auth" || auth.Token != testToken {
			t.Fatal("invalid auth frame")
		}
		if json.Unmarshal([]byte(lines[1]), &msg) != nil || msg.Type != "user" || msg.Message.Role != "user" || msg.Message.Content != nudge {
			t.Fatal("invalid nudge frame")
		}
	case <-ctx.Done():
		t.Fatal("no complete socket submission")
	}
}
func TestInboxFailureAndCancellationAreRedacted(t *testing.T) {
	dir := privateDir(t)
	if err := Capture(dir, testIdentity, testAddress(), testToken); err != nil {
		t.Fatal(err)
	}
	send, err := Prepare(dir, testIdentity)
	if err != nil {
		t.Fatal(err)
	}
	err = send(context.Background(), "fixed nudge")
	if !errors.Is(err, ErrSend) {
		t.Fatal("missing endpoint did not fail closed")
	}
	for _, secret := range []string{testAddress(), testToken, testIdentity.SessionID} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("error leaked a secret")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := send(ctx, "fixed nudge"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled send not cancelled")
	}
}
