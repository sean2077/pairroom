package service

import (
	"context"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// After a Service restart a suspended Native Room with wake-eligible queued
// input is resumed so its waker can run; a Room whose only input already had a
// wake attempt, or whose wake is disabled, stays suspended. Detection appends
// nothing to the Event Log.
func TestResumePendingRoomsActivatesOnlyRoomsWithWakeWork(t *testing.T) {
	for _, tc := range []struct {
		name string
		prep func(t *testing.T, f *nativeFixture, sender relay.Auth)
		want bool
	}{
		{name: "queued input for a bound session", want: true, prep: func(t *testing.T, f *nativeFixture, sender relay.Auth) {
			if _, err := f.native.engine.Send(sender, relay.SendRequest{ID: "pending", Text: "please review"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "input already attempted", prep: func(t *testing.T, f *nativeFixture, sender relay.Auth) {
			m, err := f.native.engine.Send(sender, relay.SendRequest{ID: "attempted", Text: "please review"})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.native.engine.ReserveWake(m.ID, model.ActorSlot2); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wake disabled", prep: func(t *testing.T, f *nativeFixture, sender relay.Auth) {
			if err := f.native.engine.SetWakeEnabled(false); err != nil {
				t.Fatal(err)
			}
			if _, err := f.native.engine.Send(sender, relay.SendRequest{ID: "disabled", Text: "please review"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no queued input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := nativeHTTP(t)
			sender := f.bind(t, model.ActorSlot1)
			f.bind(t, model.ActorSlot2)
			if tc.prep != nil {
				tc.prep(t, f, sender)
			}
			// Simulate a Service restart: drain the old manager, start a new one.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := f.manager.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			before, err := readEventsReadOnly(f.room.DataDir + "/events.jsonl")
			if err != nil {
				t.Fatal(err)
			}
			factory := EmbeddedRuntimeFactory(f.registry, EmbeddedRuntimeConfig{Claude: agent.Config{Command: "missing-do-not-spawn-claude"}, Codex: agent.Config{Command: "missing-do-not-spawn-codex"}, nativeWake: nativeWakerConfig{Wait: func(context.Context, time.Duration) error { return context.Canceled }}})
			restarted, err := NewRuntimeManager(f.registry, factory, RuntimeManagerConfig{Limit: 5, IdleTimeout: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = restarted.Shutdown(ctx)
			})
			result := ResumePendingRooms(context.Background(), f.registry, restarted)
			requested := len(result.Requested) == 1 && result.Requested[0] == f.room.ID
			if requested != tc.want || result.Skipped != 0 || (!tc.want && len(result.Requested) != 0) {
				t.Fatalf("resume = %+v, want requested=%v", result, tc.want)
			}
			phase := restarted.Status(f.room.ID).Phase
			if tc.want && phase == RuntimeSuspended || !tc.want && phase != RuntimeSuspended {
				t.Fatalf("runtime phase = %s, want resumed=%v", phase, tc.want)
			}
			if !tc.want {
				after, err := readEventsReadOnly(f.room.DataDir + "/events.jsonl")
				if err != nil || len(after) != len(before) {
					t.Fatalf("detection wrote to the Event Log: %d -> %d (%v)", len(before), len(after), err)
				}
			}
		})
	}
}
