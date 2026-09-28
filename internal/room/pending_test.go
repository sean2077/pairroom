package room

import (
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

// A Service decides whether to resume a suspended Room from this read-only
// replay; it must agree with what activation actually rebuilds, and must not
// write to the log.
func TestHasRecoverableWorkMatchesRestoreWithoutWriting(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delivery model.DeliveryState
		process  model.ProcessingState
		want     bool
	}{
		{"pending FIFO input", model.DeliveryPending, "", true},
		{"queued FIFO input", model.DeliveryQueued, model.ProcessingWaiting, true},
		{"uncertain submission is not replayed", model.DeliverySubmitting, model.ProcessingWaiting, false},
		{"accepted input is not replayed", model.DeliveryStarted, model.ProcessingWorking, false},
		{"cancelled queue entry", model.DeliveryQueued, model.ProcessingCancelled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			eventStore, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			engine, err := New(Config{Name: "resume", Repo: t.TempDir(), Store: eventStore, Settings: model.DefaultRoomSettings()})
			if err != nil {
				t.Fatal(err)
			}
			message := model.Message{
				ID: model.NewID("msg"), From: model.ActorUser, To: []model.ActorID{model.ActorSlot2},
				Text: "work", ThreadID: model.NewID("thread"), CreatedAt: time.Now().UTC(),
				Delivery: map[model.ActorID]model.DeliveryState{model.ActorSlot2: tc.delivery},
			}
			if tc.process != "" {
				message.Processing = map[model.ActorID]model.ProcessingState{model.ActorSlot2: tc.process}
			}
			if _, err := engine.record(EventMessageCreated, model.ActorUser, message); err != nil {
				t.Fatal(err)
			}
			if err := engine.Close(); err != nil {
				t.Fatal(err)
			}
			reader, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			events, err := reader.Load()
			_ = reader.Close()
			if err != nil {
				t.Fatal(err)
			}
			got, err := HasRecoverableWork(events)
			if err != nil || got != tc.want {
				t.Fatalf("HasRecoverableWork = %v, %v; want %v", got, err, tc.want)
			}
			after, _ := store.Open(dir)
			again, _ := after.Load()
			_ = after.Close()
			if len(again) != len(events) {
				t.Fatalf("probe appended events: %d -> %d", len(events), len(again))
			}
		})
	}
}
