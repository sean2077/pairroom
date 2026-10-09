package service

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/store"
)

// After a Service restart a suspended, wake-enabled Native Room with queued
// input for its bound session is resumed; a Room whose only input already had a
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

// Startup resumes durable input even when its target has no external wake
// transport. The active Runtime must still report input_waiting; excluding Grok
// or Gemini here would prevent the maintenance tick from ever reaching it.
func TestResumePendingNativeRoomsWithoutExternalWakeStillNotify(t *testing.T) {
	for _, kind := range []model.RuntimeKind{model.RuntimeGrok, model.RuntimeGemini} {
		t.Run(string(kind), func(t *testing.T) {
			registry, project := testRegistry(t, testGitRepo(t))
			noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
				t.Error("Native provisioning spawned an adapter")
				return Binding{}, nil, errors.New("must not spawn")
			})
			durable, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
				ProjectID: project.ID, Name: "Pending native input", HostMode: model.HostNative,
				Agents: map[model.ActorID]model.AgentSelection{model.ActorSlot1: {Runtime: model.RuntimeClaude}, model.ActorSlot2: {Runtime: kind}},
			}, noSpawn)
			if err != nil {
				t.Fatal(err)
			}
			log, err := store.OpenExistingForRoom(durable.DataDir, durable.ID)
			if err != nil {
				t.Fatal(err)
			}
			queuedAt := time.Now().Add(-inputWaitingThreshold - time.Minute).UTC()
			engine, err := relay.Open(relay.Config{
				RoomID: durable.ID, Store: log, Now: func() time.Time { return queuedAt },
				Runtimes: map[model.ActorID]model.RuntimeKind{model.ActorSlot1: model.RuntimeClaude, model.ActorSlot2: kind},
				CommitBinding: func(b relay.Binding, appendFact func() error) error {
					return registry.commitNativeBinding(durable.ID, b, appendFact)
				},
			})
			if err != nil {
				_ = log.Close()
				t.Fatal(err)
			}
			if _, err := engine.Bind(model.ActorSlot2, relay.BindRequest{BindID: "pending-binding", CredentialHash: relay.Digest("fixture-secret"), SessionID: "pending-session"}); err != nil {
				_ = engine.Close()
				t.Fatal(err)
			}
			if _, err := engine.SendUser(relay.SendRequest{ID: "pending-input", To: model.ActorSlot2, Text: "please review"}); err != nil {
				_ = engine.Close()
				t.Fatal(err)
			}
			if err := engine.Close(); err != nil {
				t.Fatal(err)
			}
			durable, _ = registry.Room(durable.ID)
			path := filepath.Join(durable.DataDir, "events.jsonl")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Unknown/missing runtime selections are rejected by Room validation,
			// so exercise only the read-only recovery predicate with those inputs.
			// The real activation below uses the valid Grok/Gemini Room unchanged.
			for _, configured := range []model.RuntimeKind{kind, "future-runtime", ""} {
				probe := cloneRoom(durable)
				selection := probe.Agents[model.ActorSlot2]
				selection.Runtime = configured
				probe.Agents[model.ActorSlot2] = selection
				if pending, err := roomHasResumableWork(probe); err != nil || !pending {
					t.Errorf("runtime %q recovery predicate = %v, %v; want pending input", configured, pending, err)
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("recovery detection changed durable facts: %v", err)
			}
			notifier := NewNotifier(NotifierConfig{})
			t.Cleanup(notifier.Close)
			factory := EmbeddedRuntimeFactory(registry, EmbeddedRuntimeConfig{Notifier: notifier, nativeWake: nativeWakerConfig{
				Run: func(context.Context, string, ...string) error {
					t.Error("unsupported runtime submitted an external wake")
					return errors.New("must not wake")
				},
			}})
			restarted, err := NewRuntimeManager(registry, factory, RuntimeManagerConfig{Limit: 5, IdleTimeout: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = restarted.Shutdown(ctx)
			})
			if phase := restarted.Status(durable.ID).Phase; phase != RuntimeSuspended {
				t.Fatalf("startup phase = %s", phase)
			}
			result := ResumePendingRooms(context.Background(), registry, restarted)
			if len(result.Requested) != 1 || result.Requested[0] != durable.ID || result.Skipped != 0 {
				t.Fatalf("startup dropped pending input for %s: %+v", kind, result)
			}
			items, changed := notifier.Since(0)
			if len(items) == 0 {
				select {
				case <-changed:
				case <-time.After(5 * time.Second):
					t.Fatal("resumed Runtime never reported its waiting input")
				}
				items, _ = notifier.Since(0)
			}
			if len(items) != 1 || items[0].Kind != relay.AttentionInputWaiting || items[0].RoomID != durable.ID || items[0].Slot != model.ActorSlot2 {
				t.Fatalf("waiting-input notification = %+v", items)
			}
			if state := restarted.Status(durable.ID); state.WakePending {
				t.Fatalf("unsupported runtime pinned an idle Runtime with a wake lease: %+v", state)
			}
		})
	}
}
