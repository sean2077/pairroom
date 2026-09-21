package service

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

	"github.com/sean2077/pairroom/internal/claudewake"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func claudeCandidate(id string) relay.WakeCandidate {
	c := codexCandidate(id)
	c.Runtime = model.RuntimeClaude
	c.BindID = "claude-bind"
	c.Generation = 3
	return c
}
func TestNativeClaudeWakeSubmissionFailureAndNoRetry(t *testing.T) {
	for _, tc := range []struct {
		name            string
		err             error
		outcome, reason string
	}{
		{"submitted", nil, "submitted", ""},
		{"error", errors.New("private-token-and-socket"), "failed", "socket_failed"},
		{"timeout", context.DeadlineExceeded, "failed", "socket_timeout"},
		{"cancelled", context.Canceled, "failed", "socket_cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{"m": claudeCandidate("m")}}
			now := time.Now().UTC()
			calls := 0
			cfg := nativeWakerConfig{Relay: state, Now: func() time.Time { return now }, Wait: func(context.Context, time.Duration) error { return nil }, Run: func(context.Context, string, ...string) error { t.Fatal("Claude wake spawned a CLI"); return nil },
				Claude: func(c relay.WakeCandidate) (claudewake.Send, error) {
					if c.BindID != "claude-bind" || c.Generation != 3 {
						t.Fatal("lost binding identity")
					}
					return func(ctx context.Context, nudge string) error {
						reserved, _ := state.snapshot()
						if len(reserved) != 1 {
							t.Fatal("effect before durable reservation")
						}
						if _, ok := ctx.Deadline(); !ok {
							t.Fatal("unbounded wake")
						}
						if nudge != nativeWakeNudge {
							t.Fatal("non-fixed wake body")
						}
						calls++
						return tc.err
					}, nil
				},
			}
			w := newNativeWaker(cfg)
			if err := w.Wake(context.Background(), "m"); err != nil {
				t.Fatal(err)
			}
			reservations, records := state.snapshot()
			if calls != 1 || len(reservations) != 1 || len(records) != 1 || records[0].outcome != tc.outcome || records[0].reason != tc.reason {
				t.Fatalf("calls=%d records=%#v", calls, records)
			}
			// Rehydrate after the rate window. Durable message reservation, not just an
			// in-memory timer, still forbids replay of a possibly submitted socket write.
			now = now.Add(2 * time.Hour)
			if err := newNativeWaker(cfg).Wake(context.Background(), "m"); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("wake retried after restart")
			}
		})
	}
}
func TestNativeClaudeWakeSuppressesBeforeReadingSecrets(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*relay.WakeCandidate)
	}{
		{"disabled", func(c *relay.WakeCandidate) { c.Enabled = false }},
		{"collector", func(c *relay.WakeCandidate) { c.WaiterActive = true }},
		{"delivering", func(c *relay.WakeCandidate) { c.Delivering = true }},
		{"unbound", func(c *relay.WakeCandidate) { c.SessionID = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := claudeCandidate("m")
			tc.change(&c)
			state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{"m": c}}
			w := newNativeWaker(nativeWakerConfig{Relay: state, Claude: func(relay.WakeCandidate) (claudewake.Send, error) {
				t.Fatal("read suppressed capability")
				return nil, nil
			}})
			if err := w.Wake(context.Background(), "m"); err != nil {
				t.Fatal(err)
			}
			reserved, _ := state.snapshot()
			if len(reserved) != 0 {
				t.Fatal("suppression consumed reservation")
			}
		})
	}
}
func TestNativeClaudeWakeCapabilityAndReservationRecheck(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		prepareErr, reserveErr error
		reason                 string
	}{
		{"missing capability", claudewake.ErrUnavailable, nil, "capability_unavailable"},
		{"revoked after prepare", nil, relay.ErrWakeIneligible, "invalid_message"},
		{"already reserved", nil, relay.ErrWakeReserved, "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &fakeNativeWakeRelay{candidates: map[string]relay.WakeCandidate{"m": claudeCandidate("m")}, reserveErr: tc.reserveErr}
			w := newNativeWaker(nativeWakerConfig{Relay: state, Wait: func(context.Context, time.Duration) error { return nil }, Claude: func(relay.WakeCandidate) (claudewake.Send, error) {
				return func(context.Context, string) error { t.Fatal("ineligible socket write"); return nil }, tc.prepareErr
			}})
			if err := w.Wake(context.Background(), "m"); err != nil {
				t.Fatal(err)
			}
			reserved, records := state.snapshot()
			if len(reserved) != 0 || len(records) != 1 || records[0].reason != tc.reason {
				t.Fatalf("unexpected facts: %#v", records)
			}
		})
	}
}
func TestNativeClaudeCapabilityBoundToWorkspaceAndGeneration(t *testing.T) {
	root := t.TempDir()
	room := "room"
	c := claudeCandidate("m")
	dir := filepath.Join(root, ".pairroom", "rooms", room, "slots", string(c.Target))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	prepare := prepareNativeClaudeWake(root, room)
	if _, err := prepare(c); err == nil {
		t.Fatal("missing capability accepted")
	}
	address := "/tmp/inbox.sock"
	if runtime.GOOS == "windows" {
		address = `\\.\pipe\pairroom-unit-inbox`
	}
	identity := claudewake.Identity{BindID: c.BindID, Generation: c.Generation, SessionID: c.SessionID}
	if err := claudewake.Capture(dir, identity, address, "secret-test-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(c); err != nil {
		t.Fatal(err)
	}
	c.Generation++
	if _, err := prepare(c); err == nil {
		t.Fatal("old generation reused")
	}
	identity.Generation = c.Generation
	if err := claudewake.Capture(dir, identity, address, "rotated-test-token"); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(c); err != nil {
		t.Fatal("refreshed capability unavailable", err)
	}
	// The private selection identity added for wake must not enter projections.
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), c.BindID) || strings.Contains(string(b), "generation") {
		t.Fatal("private capability identity leaked")
	}
}
func TestNativeClaudeSubmittedFactSurvivesRegistryRebuild(t *testing.T) {
	f := nativeHTTP(t)
	if err := f.native.engine.RecordWake("submitted", "", model.ActorSlot1); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.registry.Root(), "service-registry.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: f.registry.Root()}); err != nil {
		t.Fatal("submitted fact rejected on rebuild", err)
	}
}

func TestNativeMockDoesNotReadExternalClaudeCapability(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		t.Error("native mock spawned adapter")
		return Binding{}, nil, errors.New("must not spawn")
	})
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "Mock native", HostMode: model.HostNative}, noSpawn)
	if err != nil {
		t.Fatal(err)
	}
	factory := EmbeddedRuntimeFactory(registry, EmbeddedRuntimeConfig{Mock: true})
	rt, err := factory(context.Background(), room)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(context.Background())
	native, ok := rt.(*nativeHostRuntime)
	if !ok || native.waker.claude != nil {
		t.Fatal("Mock configured a real Claude inbox reader")
	}
}
