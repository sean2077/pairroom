package relay

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

// Replacing an active binding reports the old generation's inbox by ID and
// raises delivery_uncertain for a delivery the binding fact made unknown,
// which no standalone message fact records.
func TestReplaceReportsPreviousGenerationWork(t *testing.T) {
	log, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var attention []Attention
	e, err := Open(Config{RoomID: "room", Store: log, Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: model.RuntimeCodex},
		OnAttention: func(a Attention) { attention = append(attention, a) }})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	a := map[model.ActorID]Auth{}
	for _, slot := range model.SlotActors() {
		b, err := e.Bind(slot, BindRequest{BindID: "bind-" + string(slot), CredentialHash: Digest("secret"), SessionID: "session-" + string(slot)})
		if err != nil {
			t.Fatal(err)
		}
		a[slot] = Auth{Slot: slot, BindID: b.BindID, Generation: b.Generation, SessionID: b.SessionID, Secret: "secret"}
	}
	sender, target := a[model.ActorSlot1], a[model.ActorSlot2]
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	send := func(id string) Message {
		m, err := e.Send(sender, SendRequest{ID: id, Text: "work " + id})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	claim := func() *Claim {
		c, err := e.Claim(ctx, target, false)
		if err != nil || c == nil {
			t.Fatalf("claim = %+v, %v", c, err)
		}
		return c
	}
	handed := send("handed")
	if c := claim(); e.Ack(target, c.ID, c.Receipt) != nil {
		t.Fatal("ack failed")
	}
	delivering := send("delivering")
	claim()
	queued := send("queued")

	// Idempotent recovery of the current binding reports nothing.
	if _, replaced, err := e.BindReport(model.ActorSlot2, BindRequest{BindID: target.BindID, CredentialHash: Digest("secret"), SessionID: target.SessionID}); err != nil || replaced != nil {
		t.Fatalf("recovery = %+v, %v", replaced, err)
	}
	attention = nil
	b, replaced, err := e.BindReport(model.ActorSlot2, BindRequest{BindID: "replacement", CredentialHash: Digest("secret"), SessionID: "new-session", Replace: true})
	if err != nil || b.Generation != target.Generation+1 {
		t.Fatalf("replace = %+v, %v", b, err)
	}
	if replaced == nil || replaced.Generation != target.Generation ||
		!slices.Equal(replaced.Cancelled, []string{queued.ID}) ||
		!slices.Equal(replaced.Unknown, []string{delivering.ID}) ||
		!slices.Equal(replaced.HandedOff, []string{handed.ID}) || replaced.HandedOffTotal != 1 || replaced.Truncated {
		t.Fatalf("replaced = %+v", replaced)
	}
	if len(attention) != 1 || attention[0] != (Attention{Kind: AttentionDeliveryUncertain, Slot: model.ActorSlot2, Key: delivering.ID}) {
		t.Fatalf("attention = %+v", attention)
	}
	// Replay never re-raises derived attention.
	attention = nil
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	log, err = store.Open(e.cfg.Store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(Config{RoomID: "room", Store: log, Runtimes: e.cfg.Runtimes, OnAttention: func(a Attention) { attention = append(attention, a) }})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if len(attention) != 0 {
		t.Fatalf("replay raised attention: %+v", attention)
	}
}

// Handed-off IDs are bounded, newest first, with the full count and a flag.
func TestReplaceReportBoundsHandedOff(t *testing.T) {
	e, a, _ := testEngine(t)
	sender, target := a[model.ActorSlot1], a[model.ActorSlot2]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ids []string
	for i := range maxPreviousHandedOff + 2 {
		m, err := e.Send(sender, SendRequest{ID: "m" + string(rune('a'+i)), Text: "x"})
		if err != nil {
			t.Fatal(err)
		}
		c, err := e.Claim(ctx, target, false)
		if err != nil || c == nil || e.Ack(target, c.ID, c.Receipt) != nil {
			t.Fatalf("delivery %d failed: %v", i, err)
		}
		ids = append(ids, m.ID)
	}
	_, replaced, err := e.BindReport(model.ActorSlot2, BindRequest{BindID: "replacement", CredentialHash: Digest("x"), SessionID: "new-session", Replace: true})
	if err != nil || replaced == nil {
		t.Fatalf("replace = %+v, %v", replaced, err)
	}
	slices.Reverse(ids)
	if !slices.Equal(replaced.HandedOff, ids[:maxPreviousHandedOff]) || replaced.HandedOffTotal != len(ids) || !replaced.Truncated {
		t.Fatalf("replaced = %+v", replaced)
	}
}
