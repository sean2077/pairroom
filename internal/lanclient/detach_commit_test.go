package lanclient

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestDetachKeepsReservationWhenDurableRetirementFails(t *testing.T) {
	for _, status := range []string{"accepted", "pending"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			s, f := newStore(t), newRemote(t)
			f.mu.Lock()
			f.status = status
			f.mu.Unlock()
			options := f.options(t)
			c, joined, err := s.Join(ctx, options)
			if err != nil || joined.Status != status {
				t.Fatalf("join: %+v %v", joined, err)
			}
			original, err := c.withRecord(ctx, func(r *record) error {
				if r.Room != nil {
					r.Deliveries = []delivery{{ID: "retained-unknown", Receipt: "original-receipt", Generation: r.Room.Generation, State: "unknown"}}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			auth := relay.Auth{BindID: original.BindID, SessionID: original.SessionID, Secret: fixtureSecret}
			if original.Room != nil {
				auth.Slot, auth.Generation = original.Room.Slot, original.Room.Generation
			}
			f.server.Close()
			file := filepath.Join(c.dir, "client.json")
			backup := filepath.Join(c.dir, "retained-client.json")
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			// Obstruct the real atomic-write destination after the record has
			// been read under its lock. No production fault hook or fabricated
			// acknowledgement is needed to exercise the durable-effect failure.
			blocked := false
			_, err = c.withLockedRecord(ctx, func(r record) (record, error) {
				if err := os.Rename(file, backup); err != nil {
					return r, err
				}
				if err := privatefile.Mkdir(file); err != nil {
					return r, err
				}
				blocked = true
				return c.detachLocked(ctx, r)
			})
			if !blocked || err == nil {
				t.Fatalf("retirement write fault was not exercised: blocked=%v err=%v", blocked, err)
			}
			claim := reservation(original)
			if err := s.identities.Check(ctx, claim); err != nil {
				t.Fatalf("failed retirement released its native session: %v", err)
			}
			replacement := claim
			replacement.Association = nativeidentity.Hosted(options.Workspace, "other-room", model.ActorSlot1)
			replacement.BindID = "other-binding"
			if err := s.identities.Reserve(ctx, replacement); !errors.Is(err, nativeidentity.ErrOwned) {
				t.Fatalf("another Room acquired the session before durable retirement: %v", err)
			}
			saved, err := os.ReadFile(backup)
			if err != nil || !bytes.Equal(saved, before) {
				t.Fatal("failed retirement changed retained identity or unknown-delivery evidence")
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(backup, file); err != nil {
				t.Fatal(err)
			}
			if err := c.Detach(ctx, auth); err != nil {
				t.Fatalf("same offline retirement could not recover: %v", err)
			}
			got, err := c.read(ctx)
			original.Status = "detached"
			if err != nil || !reflect.DeepEqual(got, original) {
				t.Fatalf("retirement lost its original private record: %+v %v", got.Deliveries, err)
			}
			if err := s.identities.Check(ctx, claim); !errors.Is(err, nativeidentity.ErrUnowned) {
				t.Fatalf("successful retirement kept the old reservation: %v", err)
			}
			if _, err := c.Resume(ctx); !errors.Is(err, ErrInactive) {
				t.Fatalf("retirement reactivated remote membership: %v", err)
			}
			if len(f.actions()) != 1 {
				t.Fatal("offline retirement contacted the host")
			}
		})
	}
}

func TestDetachTerminalRetryReconcilesOnlyOriginalReservation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   string
		retained string
	}{
		{name: "detached-before-release", status: "detached", retained: "original"},
		{name: "admission-promotion-interrupted", status: "detached", retained: "pending"},
		{name: "left-before-release", status: "left", retained: "original"},
		{name: "revoked-before-release", status: "revoked", retained: "original"},
		{name: "expired-before-release", status: "expired", retained: "pending"},
		{name: "already-released", status: "detached", retained: "none"},
		{name: "session-reused", status: "detached", retained: "replacement"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s, f := newStore(t), newRemote(t)
			c, auth, options := f.join(t, s)
			original, err := c.withRecord(ctx, func(r *record) error {
				r.Deliveries = []delivery{{ID: "retained-unknown", Receipt: "original-receipt", Generation: auth.Generation, State: "unknown"}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			claim := reservation(original)
			retained := claim
			if tc.retained != "original" {
				if err := s.identities.Release(ctx, claim); err != nil {
					t.Fatal(err)
				}
				switch tc.retained {
				case "pending":
					retained.Generation = 0
				case "replacement":
					retained.Association = nativeidentity.Hosted(options.Workspace, "replacement-room", model.ActorSlot1)
					retained.BindID, retained.Generation = "replacement-binding", 1
				}
				if tc.retained != "none" {
					if err := s.identities.Reserve(ctx, retained); err != nil {
						t.Fatal(err)
					}
				}
			}
			// This is the recoverable cut after the retirement fact was
			// persisted, including a crash before its exact release completed.
			original.Status = tc.status
			if err := privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), original); err != nil {
				t.Fatal(err)
			}
			f.server.Close()
			reopened, err := OpenAt(s.Root())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			client, err := reopened.Get(ctx, c.id)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := client.Detach(ctx, auth); err != nil {
					t.Fatalf("durable terminal retry did not converge: %v", err)
				}
			}
			got, err := client.read(ctx)
			original.Status = "detached"
			if err != nil || !reflect.DeepEqual(got, original) {
				t.Fatalf("terminal retry changed original receipt evidence: %+v %v", got.Deliveries, err)
			}
			if tc.retained == "replacement" {
				if err := s.identities.Check(ctx, retained); err != nil {
					t.Fatalf("terminal retry changed a replacement owner: %v", err)
				}
			} else if err := s.identities.Check(ctx, retained); !errors.Is(err, nativeidentity.ErrUnowned) {
				t.Fatalf("terminal retry did not release only its original claim: %v", err)
			}
			if err := client.Maintenance(ctx); !errors.Is(err, ErrInactive) {
				t.Fatalf("retired observer became active: %v", err)
			}
			if len(f.actions()) != 1 {
				t.Fatal("terminal retry contacted the host")
			}
		})
	}
}

func TestDetachActiveConflictDoesNotClaimSuccessfulRetirement(t *testing.T) {
	ctx := context.Background()
	s, f := newStore(t), newRemote(t)
	c, auth, options := f.join(t, s)
	original, err := c.read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	claim := reservation(original)
	if err := s.identities.Release(ctx, claim); err != nil {
		t.Fatal(err)
	}
	replacement := claim
	replacement.Association = nativeidentity.Hosted(options.Workspace, "replacement-room", model.ActorSlot1)
	replacement.BindID = "replacement-binding"
	if err := s.identities.Reserve(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	if err := c.Detach(ctx, auth); !errors.Is(err, nativeidentity.ErrOwned) {
		t.Fatalf("active identity conflict was reported as retirement: %v", err)
	}
	got, err := c.read(ctx)
	if err != nil || !reflect.DeepEqual(got, original) {
		t.Fatal("conflicted retirement changed an active private record")
	}
	if err := s.identities.Check(ctx, replacement); err != nil {
		t.Fatalf("conflicted retirement changed the current owner: %v", err)
	}
	if len(f.actions()) != 1 {
		t.Fatal("conflicted offline retirement contacted the host")
	}
}
