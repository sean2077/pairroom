package relay

import (
	"context"
	"fmt"
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
	// A delivery that was already unknown before the bind is reported apart
	// from the one this bind invalidates.
	expired := send("expired")
	claim()
	e.mu.Lock()
	e.cfg.Lease = time.Nanosecond
	e.mu.Unlock()
	time.Sleep(time.Millisecond)
	if err := e.Reap(); err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.cfg.Lease = time.Hour
	e.mu.Unlock()
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
	if replaced == nil || replaced.Generation != target.Generation || !replaced.ScanComplete ||
		!slices.Equal(replaced.Cancelled.IDs, []string{queued.ID}) || replaced.Cancelled.Count != 1 ||
		!slices.Equal(replaced.Unknown.IDs, []string{delivering.ID}) || replaced.Unknown.Count != 1 ||
		!slices.Equal(replaced.AlreadyUnknown.IDs, []string{expired.ID}) || replaced.AlreadyUnknown.Count != 1 ||
		!slices.Equal(replaced.HandedOff.IDs, []string{handed.ID}) || replaced.HandedOff.Count != 1 {
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

// Each list is bounded with an exact count; handed-off IDs are newest first.
func TestReplaceReportBoundsHandedOff(t *testing.T) {
	e, a, _ := testEngine(t)
	sender, target := a[model.ActorSlot1], a[model.ActorSlot2]
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var ids []string
	for i := range maxReplacedIDs + 2 {
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
	if !slices.Equal(replaced.HandedOff.IDs, ids[:maxReplacedIDs]) || replaced.HandedOff.Count != len(ids) || !replaced.ScanComplete {
		t.Fatalf("replaced = %+v", replaced)
	}
}

// Beyond the handed-off scan window the count is a lower bound and says so;
// work this bind invalidates is still counted exactly from the unresolved set.
func TestReplaceReportMarksIncompleteHandedOffScan(t *testing.T) {
	e, a, _ := testEngine(t)
	queued, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "early-queued", Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// Retained history directly, as the long-Room benchmark does: the property
	// is the bounded projection, not storage admission.
	for i := range maxPreviousScan + 1 {
		e.putMessage(Message{ID: fmt.Sprintf("old-%d", i), To: model.ActorSlot2, State: "handed_off", TargetGeneration: a[model.ActorSlot2].Generation})
	}
	_, r, err := e.BindReport(model.ActorSlot2, BindRequest{BindID: "new", CredentialHash: Digest("x"), SessionID: "new-session", Replace: true})
	if err != nil || r == nil {
		t.Fatalf("replace = %+v, %v", r, err)
	}
	if r.ScanComplete || r.HandedOff.Count != maxPreviousScan || len(r.HandedOff.IDs) != maxReplacedIDs ||
		r.Cancelled.Count != 1 || r.Cancelled.IDs[0] != queued.ID {
		t.Fatalf("replaced = %+v", r)
	}
}
