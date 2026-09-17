package service

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

type nativeWakeCommand struct {
	name string
	args []string
}

func TestNativeRelaySendSchedulesRedactedCodexWake(t *testing.T) {
	commands := make(chan nativeWakeCommand, 1)
	f := nativeHTTPWithWake(t, nativeWakerConfig{
		Grace: time.Millisecond,
		Wait:  func(context.Context, time.Duration) error { return nil },
		Run: func(_ context.Context, name string, args ...string) error {
			commands <- nativeWakeCommand{name: name, args: append([]string(nil), args...)}
			return nil
		},
	})
	sender := associateCLI(t, f, model.ActorSlot1)
	receiver := associateCLI(t, f, model.ActorSlot2)
	body := "private relay payload must stay out of wake audit"
	if _, err := f.runAs(t, model.RuntimeClaude, sender.SessionID, []string{"send", "--id", "wake-live", "--text", body}, nil); err != nil {
		t.Fatal(err)
	}
	var command nativeWakeCommand
	select {
	case command = <-commands:
	case <-time.After(3 * time.Second):
		t.Fatal("native send did not schedule a wake")
	}
	if command.name != "codex" {
		t.Fatalf("command = %q", command.name)
	}
	want := []string{"queue", "--thread", receiver.SessionID, "--message", nativeWakeNudge}
	if strings.Join(command.args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("args = %#v, want %#v", command.args, want)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		reservations := f.native.engine.WakeReservations()
		accepted := false
		for _, audit := range f.native.engine.Snapshot().Audit {
			if audit.Kind == "native.wake.attempted" && audit.Detail == "wake accepted" {
				accepted = true
			}
		}
		if len(reservations) == 1 && reservations[0].Target == model.ActorSlot2 && accepted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("wake audit did not settle: reservations=%#v audit=%#v", reservations, f.native.engine.Snapshot().Audit)
		}
		time.Sleep(10 * time.Millisecond)
	}
	events, err := readEventsReadOnly(filepath.Join(f.room.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if !strings.HasPrefix(event.Kind, "native.wake.") {
			continue
		}
		for _, forbidden := range []string{receiver.SessionID, nativeWakeNudge, body} {
			if bytes.Contains(event.Data, []byte(forbidden)) {
				t.Fatalf("%s leaked %q: %s", event.Kind, forbidden, event.Data)
			}
		}
	}
}
