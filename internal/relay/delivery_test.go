package relay

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestMessageDeliveryProjectionIsBoundedReplayableAndBodyFree(t *testing.T) {
	e, a, dir := testEngine(t)
	now := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	e.cfg.Now = func() time.Time { return now }
	m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "latency", Text: "private body sentinel"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		now = now.Add(100 * time.Millisecond)
		if err := e.RecordWake("suppressed", "minimum_interval", model.ActorSlot2); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(500 * time.Millisecond)
	if err := e.ReserveWake(m.ID, model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	claim, err := e.Claim(context.Background(), a[model.ActorSlot2], false)
	if err != nil {
		t.Fatal(err)
	}
	// The known outcome may arrive after the collector claimed the input.
	now = now.Add(time.Second)
	if err := e.RecordWakeAttempt(m.ID, "submitted", "", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if err := e.Ack(a[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	snap := e.SnapshotTail()
	got := snap.Delivery[m.ID]
	if got.QueueWaitMS == nil || *got.QueueWaitMS != 2000 || got.ReservedAt == nil || got.InferredOutcome == nil || got.InferredOutcome.Outcome != "submitted" {
		t.Fatalf("delivery=%+v", got)
	}
	if len(got.SlotObservations) != 3 || got.SlotObservationCount != 5 {
		t.Fatalf("unbounded/wrong slot window: %+v", got)
	}
	page, err := e.History(HistoryQuery{ID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Delivery, snap.Delivery) {
		t.Fatal("history/snapshot evidence differs")
	}
	data, _ := json.Marshal(page.Delivery)
	for _, private := range []string{"private body sentinel", claim.Receipt, a[model.ActorSlot2].SessionID, "receipt", "session_id"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("delivery projection leaked %q", private)
		}
	}
	page.Delivery[m.ID].SlotObservations[0].Outcome = "mutated"
	again, _ := e.History(HistoryQuery{ID: m.ID})
	if again.Delivery[m.ID].SlotObservations[0].Outcome == "mutated" {
		t.Fatal("caller mutated engine projection")
	}
	events, err := e.cfg.Store.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if strings.Contains(string(ev.Data), "queue_wait_ms") || strings.Contains(string(ev.Data), "inferred_outcome") {
			t.Fatal("derived view was persisted")
		}
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	restored := reopenEngine(t, dir)
	if !reflect.DeepEqual(restored.Snapshot().Delivery, snap.Delivery) {
		t.Fatalf("replay changed delivery evidence: %+v", restored.Snapshot().Delivery)
	}
}

func TestMessageDeliveryDoesNotInventWakeAssociationOrNegativeTiming(t *testing.T) {
	e, a, _ := testEngine(t)
	now := time.Now().UTC()
	e.cfg.Now = func() time.Time { return now }
	m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "clock-backwards", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWake("failed", "command_failed", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	now = now.Add(-time.Second)
	claim, err := e.Claim(context.Background(), a[model.ActorSlot2], false)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Ack(a[model.ActorSlot2], claim.ID, claim.Receipt); err != nil {
		t.Fatal(err)
	}
	got := e.Snapshot().Delivery[m.ID]
	if got.QueueWaitMS != nil || got.ReservedAt != nil || got.InferredOutcome != nil {
		t.Fatalf("invented duration or message wake: %+v", got)
	}
	if len(got.SlotObservations) != 1 || got.SlotObservations[0].Outcome != "failed" {
		t.Fatal("slot-only observation lost")
	}
}

func TestMessageWakeRenewalInfersOnlyTheNewestReservation(t *testing.T) {
	e, a, _ := testEngine(t)
	now := time.Now().UTC()
	e.cfg.Now = func() time.Time { return now }
	first, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "first", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ReserveWake(first.ID, model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	second, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "second", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(WakeRenewAfter + time.Second)
	if err := e.ReserveWake(second.ID, model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWakeAttempt(second.ID, "accepted", "", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	page, err := e.History(HistoryQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Delivery) != 1 || page.Delivery[second.ID].InferredOutcome == nil {
		t.Fatal("history projection not scoped to its page")
	}
	if e.Snapshot().Delivery[first.ID].InferredOutcome != nil {
		t.Fatal("new attempt attached to old abandoned reservation")
	}
}

func TestUnknownDeliveryRetainsQueueToClaimTiming(t *testing.T) {
	e, a, _ := testEngine(t)
	now := time.Now().UTC()
	e.cfg.Now = func() time.Time { return now }
	m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "unknown", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(1500 * time.Millisecond)
	if _, err := e.Claim(context.Background(), a[model.ActorSlot2], false); err != nil {
		t.Fatal(err)
	}
	now = now.Add(e.cfg.Lease + time.Second)
	if err := e.Reap(); err != nil {
		t.Fatal(err)
	}
	page, err := e.History(HistoryQuery{ID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if page.Messages[0].State != "unknown" || page.Delivery[m.ID].QueueWaitMS == nil || *page.Delivery[m.ID].QueueWaitMS != 1500 {
		t.Fatalf("unknown lost claim timing: %+v", page)
	}
}

func TestCancelledMessageStopsItsSlotObservationWindow(t *testing.T) {
	e, a, _ := testEngine(t)
	m, err := e.Send(a[model.ActorSlot1], SendRequest{ID: "cancelled-window", Text: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWake("suppressed", "minimum_interval", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	if err := e.Cancel(m.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.RecordWake("failed", "command_failed", model.ActorSlot2); err != nil {
		t.Fatal(err)
	}
	page, err := e.History(HistoryQuery{ID: m.ID})
	if err != nil {
		t.Fatal(err)
	}
	if got := page.Delivery[m.ID]; got.SlotObservationCount != 1 || got.SlotObservations[0].Outcome != "suppressed" {
		t.Fatalf("later observation attached after cancellation: %+v", got)
	}
}
