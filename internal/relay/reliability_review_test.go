package relay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func TestNativeCrashRecoveryRetainsOriginalReceipt(t *testing.T) {
	for _, replayAgain := range []bool{false, true} {
		t.Run(map[bool]string{false: "first-recovery", true: "second-replay"}[replayAgain], func(t *testing.T) {
			e, auth, dir := testEngine(t)
			if _, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "crash", Text: "accepted work"}); err != nil {
				t.Fatal(err)
			}
			claim, err := e.Claim(context.Background(), auth[model.ActorSlot2], false)
			if err != nil {
				t.Fatal(err)
			}
			// Crash window: close only the log, not Engine.Close's orderly reap.
			if err := e.cfg.Store.Close(); err != nil {
				t.Fatal(err)
			}
			e.closed = true
			reopen := func() *Engine {
				log, err := store.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				fresh, err := Open(Config{RoomID: "room", Store: log, Runtimes: e.cfg.Runtimes})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = fresh.Close() })
				return fresh
			}
			recovered := reopen()
			if replayAgain {
				if err := recovered.Close(); err != nil {
					t.Fatal(err)
				}
				recovered = reopen()
			}
			if got := recovered.Snapshot().Messages[0].State; got != "unknown" {
				t.Fatalf("crash recovery state = %q", got)
			}
			if err := recovered.Ack(auth[model.ActorSlot2], claim.ID, "foreign-receipt"); !errors.Is(err, ErrAuth) {
				t.Fatalf("foreign receipt accepted: %v", err)
			}
			for i := 0; i < 2; i++ {
				if err := recovered.Ack(auth[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
					t.Fatalf("original collector could not settle after restart: %v", err)
				}
			}
			data, err := json.Marshal(recovered.Snapshot())
			if err != nil || strings.Contains(string(data), claim.Receipt) || strings.Contains(string(data), `"receipt"`) {
				t.Fatal("public projection exposed the private receipt")
			}
		})
	}
}

func TestNativeSendIDCannotSilentlyChangePayload(t *testing.T) {
	e, auth, _ := testEngine(t)
	sender := auth[model.ActorSlot1]
	original := SendRequest{ID: "stable", Text: "original"}
	accepted, err := e.Send(sender, original)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []SendRequest{
		{ID: original.ID, Text: "different"},
		{ID: original.ID, Text: original.Text, To: model.ActorUser},
	} {
		before := e.Sequence()
		if _, err := e.Send(sender, req); err == nil || e.Sequence() != before {
			t.Fatal("reused ID changed payload or silently returned unrelated acceptance")
		}
	}
	if again, err := e.Send(sender, original); err != nil || again.ID != accepted.ID {
		t.Fatalf("identical retry lost idempotence: %v", err)
	}
	quote, err := e.Send(sender, SendRequest{ID: "quote", Text: "quoted context", To: model.ActorUser})
	if err != nil {
		t.Fatal(err)
	}
	quoted := SendRequest{ID: "quoted", Text: "reply", QuoteID: quote.ID}
	if _, err := e.Send(sender, quoted); err != nil {
		t.Fatal(err)
	}
	quoted.QuoteID = ""
	if _, err := e.Send(sender, quoted); err == nil {
		t.Fatal("reused ID silently dropped explicit quoted context")
	}
}

func TestNativeParkNoOpDoesNotGrowEventLog(t *testing.T) {
	e, auth, _ := testEngine(t)
	before := e.Sequence()
	if err := e.Park(model.ActorSlot1, true); err != nil {
		t.Fatal(err)
	}
	if err := e.ParkAs(auth[model.ActorSlot2], true); err != nil {
		t.Fatal(err)
	}
	if e.Sequence() != before {
		t.Fatal("unchanged park policy appended binding facts")
	}
}

func TestNativeWakeReservationRevalidatesQueueAndBinding(t *testing.T) {
	for _, change := range []string{"cancelled", "unbound", "disabled", "claimed", "wrong-target"} {
		t.Run(change, func(t *testing.T) {
			e, auth, _ := testEngine(t)
			message, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "wake", Text: "work"})
			if err != nil {
				t.Fatal(err)
			}
			target := model.ActorSlot2
			switch change {
			case "cancelled":
				err = e.Cancel(message.ID)
			case "unbound":
				err = e.Unbind(target)
			case "disabled":
				err = e.SetWakeEnabled(false)
			case "claimed":
				_, err = e.Claim(context.Background(), auth[target], false)
			case "wrong-target":
				target = model.ActorSlot1
			}
			if err != nil {
				t.Fatal(err)
			}
			before := e.Sequence()
			if err := e.ReserveWake(message.ID, target); err == nil || len(e.WakeReservations()) != 0 || e.Sequence() != before {
				t.Fatal("stale wake candidate authorized a vendor effect")
			}
		})
	}
}

func TestNativeWaitChangesSurfacesFatalWriter(t *testing.T) {
	e, auth, _ := testEngine(t)
	if err := e.cfg.Store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Send(auth[model.ActorSlot1], SendRequest{ID: "closed-log", Text: "work"}); err == nil {
		t.Fatal("closed writer accepted work")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := e.WaitChanges(ctx, e.Sequence()); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fatal writer was hidden behind an idle change wait: %v", err)
	}
}
