//go:build !windows

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/claudewake"
	"github.com/sean2077/pairroom/internal/model"
)

// Real local HTTP + actual UDS transport, with a deterministic inbox fixture.
// This is deliberately NOT authenticated Claude/model-acceptance E2E.
func TestNativeClaudeBindHookAndSocketWakeEndToEnd(t *testing.T) {
	f := nativeHTTPWithWake(t, nativeWakerConfig{Wait: func(context.Context, time.Duration) error { return nil }, Run: func(context.Context, string, ...string) error { t.Error("Claude wake spawned CLI"); return nil }})
	dir, err := os.MkdirTemp("", "prw-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	address := filepath.Join(dir, "inbox.sock")
	l, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	const token = "synthetic-private-inbox-token"
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", address)
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", token)
	receiver := associateCLI(t, f, model.ActorSlot1)
	sender := associateCLI(t, f, model.ActorSlot2)
	slotDir := filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", "slot1")
	identity := claudewake.Identity{BindID: receiver.BindID, Generation: receiver.Generation, SessionID: receiver.SessionID}
	if _, err := claudewake.Prepare(slotDir, identity); err != nil {
		t.Fatal("bind did not capture capability", err)
	}
	codexPath := filepath.Join(f.project.Root, ".pairroom", "rooms", f.room.ID, "slots", "slot2", claudewake.FileName)
	if _, err := os.Stat(codexPath); !os.IsNotExist(err) {
		t.Fatal("Codex inherited Claude capability")
	}
	// Confirmed Stop refreshes a rotated token before returning from the hook.
	const rotated = "synthetic-rotated-token"
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", rotated)
	if _, err := f.hook(t, receiver, "Done with this turn.", false); err != nil {
		t.Fatal(err)
	}
	received := make(chan []byte, 1)
	go func() {
		c, e := l.Accept()
		if e != nil {
			received <- nil
			return
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		data, _ := io.ReadAll(io.LimitReader(c, 16384))
		received <- data
	}()
	const body = "private task body stays in PairRoom FIFO"
	output, err := f.runAs(t, model.RuntimeCodex, sender.SessionID, []string{"send", "--id", "claude-socket-e2e", "--text", body}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var frame []byte
	select {
	case frame = <-received:
	case <-time.After(3 * time.Second):
		t.Fatal("no socket nudge")
	}
	if !bytes.Contains(frame, []byte(rotated)) || !bytes.Contains(frame, []byte(nativeWakeNudge)) || bytes.Contains(frame, []byte(body)) {
		t.Fatal("invalid wake wire payload")
	}
	// A socket write is only submitted. The message is untouched until the
	// original associated receiver chooses to collect through the relay.
	deadline := time.Now().Add(3 * time.Second)
	for {
		settled := false
		for _, audit := range f.native.engine.Snapshot().Audit {
			if audit.Detail == "wake submitted" {
				settled = true
			}
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("submitted audit missing")
		}
		time.Sleep(time.Millisecond)
	}
	snapshot := f.native.engine.Snapshot()
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].State != "queued" {
		t.Fatal("wake consumed input", snapshot.Messages)
	}
	for _, blob := range [][]byte{output, mustMarshalClaudeTest(t, f.native.snapshot())} {
		for _, secret := range []string{token, rotated, address} {
			if bytes.Contains(blob, []byte(secret)) {
				t.Fatal("public response leaked inbox capability")
			}
		}
	}
	events, err := readEventsReadOnly(filepath.Join(f.room.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		for _, secret := range []string{token, rotated, address} {
			if bytes.Contains(event.Data, []byte(secret)) {
				t.Fatal("Event Log leaked inbox capability")
			}
		}
		if strings.HasPrefix(event.Kind, "native.wake.") && bytes.Contains(event.Data, []byte(body)) {
			t.Fatal("wake audit leaked task")
		}
	}
	result, err := f.runAs(t, model.RuntimeClaude, receiver.SessionID, []string{"wait", "--timeout", "1"}, nil)
	if err != nil || !bytes.Contains(result, []byte(body)) {
		t.Fatal("receiver cannot collect original input", err)
	}
	if f.native.engine.Snapshot().Messages[0].State != "handed_off" {
		t.Fatal("normal collection did not acknowledge")
	}
	// Missing environment removes the cached socket on the next confirmed hook.
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	t.Setenv("CLAUDE_CODE_MESSAGING_TOKEN", "")
	if _, err := f.hook(t, receiver, "Done.", false); err != nil {
		t.Fatal(err)
	}
	if _, err := claudewake.Prepare(slotDir, identity); err == nil {
		t.Fatal("missing environment retained stale socket")
	}
}
func mustMarshalClaudeTest(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
