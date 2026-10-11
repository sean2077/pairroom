package service

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestNativeSessionOwnershipSpansPendingDirectAndHostedBindings(t *testing.T) {
	f := nativeHTTP(t)
	ctx := context.Background()
	remote := nativeidentity.Claim{Runtime: model.RuntimeCodex, SessionID: "remote-pending-session", Association: nativeidentity.Remote("remote-pin", "remote-room"), BindID: "remote-pending"}
	if err := f.registry.identities.Reserve(ctx, remote); err != nil {
		t.Fatal(err)
	}
	request := relay.BindRequest{BindID: "host-operation", SessionID: remote.SessionID, CredentialHash: relay.Digest("host-secret")}
	if _, err := f.native.engine.Bind(model.ActorSlot2, request); !errors.Is(err, ErrBindingOwned) {
		t.Fatalf("host took a pending direct native session: %v", err)
	}
	request.SessionID = "independent-hosted-session"
	bound, err := f.native.engine.Bind(model.ActorSlot2, request)
	if err != nil {
		t.Fatal(err)
	}
	claim := f.registry.nativeClaims[nativeClaimKey(f.room.ID, model.ActorSlot2)]
	if err := f.registry.identities.Check(ctx, claim); err != nil {
		t.Fatal(err)
	}
	other := remote
	other.SessionID, other.BindID = request.SessionID, "other-remote"
	if err := f.registry.identities.Reserve(ctx, other); !errors.Is(err, nativeidentity.ErrOwned) {
		t.Fatal("direct client took a hosted session")
	}
	request.BindID, request.SessionID, request.Replace = "replacement-host-operation", remote.SessionID, true
	if _, err := f.native.engine.Bind(model.ActorSlot2, request); !errors.Is(err, ErrBindingOwned) {
		t.Fatal("host replacement ignored a pending direct owner")
	}
	if err := f.registry.identities.Check(ctx, claim); err != nil {
		t.Fatal("failed replacement released original hosted identity")
	}
	if err := f.native.engine.UnbindAs(relay.Auth{Slot: bound.Slot, BindID: bound.BindID, Generation: bound.Generation, SessionID: bound.SessionID, Secret: "host-secret"}); err != nil {
		t.Fatal(err)
	}
	if err := f.registry.identities.Reserve(ctx, other); err != nil {
		t.Fatal("explicit hosted unbind did not release session")
	}
	if err := f.registry.identities.Check(ctx, remote); err != nil {
		t.Fatal("host unbind changed unrelated remote pending owner")
	}
}

func TestNativeConcurrentHostAndDirectReservationHasExactlyOneOwner(t *testing.T) {
	f := nativeHTTP(t)
	remote := nativeidentity.Claim{Runtime: model.RuntimeCodex, SessionID: "competing-session", Association: nativeidentity.Remote("peer", "remote-room"), BindID: "direct-operation"}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var hostErr, directErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, hostErr = f.native.engine.Bind(model.ActorSlot2, relay.BindRequest{BindID: "host-operation", SessionID: remote.SessionID, CredentialHash: relay.Digest("secret")})
	}()
	go func() {
		defer wg.Done()
		<-start
		directErr = f.registry.identities.Reserve(context.Background(), remote)
	}()
	close(start)
	wg.Wait()
	if (hostErr == nil) == (directErr == nil) {
		t.Fatalf("ownership did not serialize: host=%v direct=%v", hostErr, directErr)
	}
	if hostErr != nil && !errors.Is(hostErr, ErrBindingOwned) {
		t.Fatalf("host conflict lost its typed ownership failure: %v", hostErr)
	}
	if directErr != nil && !errors.Is(directErr, nativeidentity.ErrOwned) {
		t.Fatalf("direct conflict lost its typed ownership failure: %v", directErr)
	}
}

func TestNativeRestartReconcilesOnlyDurablySupersededBindingClaims(t *testing.T) {
	f := nativeHTTP(t)
	ctx := context.Background()
	request := relay.BindRequest{BindID: "original-binding", SessionID: "original-session", CredentialHash: relay.Digest("secret")}
	if _, err := f.native.engine.Bind(model.ActorSlot2, request); err != nil {
		t.Fatal(err)
	}
	original := f.registry.nativeClaims[nativeClaimKey(f.room.ID, model.ActorSlot2)]
	request.BindID, request.SessionID, request.Replace = "new-binding", "new-session", true
	if _, err := f.native.engine.Bind(model.ActorSlot2, request); err != nil {
		t.Fatal(err)
	}
	current := f.registry.nativeClaims[nativeClaimKey(f.room.ID, model.ActorSlot2)]
	// Reconstruct the real crash cut: new binding fact committed, old claim
	// still present because the process stopped before its final release.
	if err := f.registry.identities.Reserve(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenRegistry(ctx, RegistryConfig{Root: f.registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.identities.Check(ctx, current); err != nil {
		t.Fatal("restart lost active binding ownership")
	}
	if err := restarted.identities.Check(ctx, original); !errors.Is(err, nativeidentity.ErrUnowned) {
		t.Fatal("superseded old session remained reserved after restart")
	}
	other := original
	other.Association, other.BindID, other.Generation = nativeidentity.Remote("peer", "remote-room"), "new-remote-binding", 0
	if err := restarted.identities.Reserve(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(ctx, RegistryConfig{Root: restarted.Root()}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.identities.Check(ctx, other); err != nil {
		t.Fatal("replaying an old retirement released another Room's newer owner")
	}
}

func TestNativeArchiveAndInterruptedDeletionKeepExactSessionOwnership(t *testing.T) {
	for _, missing := range []bool{false, true} {
		for _, outcome := range []string{"uncommitted", "committed", "reused"} {
			name := outcome
			if missing {
				name += "_missing_data"
			}
			t.Run(name, func(t *testing.T) {
				f := nativeHTTP(t)
				ctx := context.Background()
				if _, err := f.native.engine.Bind(model.ActorSlot2, relay.BindRequest{BindID: "archived-operation", SessionID: "archived-session", CredentialHash: relay.Digest("secret")}); err != nil {
					t.Fatal(err)
				}
				claim := f.registry.nativeClaims[nativeClaimKey(f.room.ID, model.ActorSlot2)]
				if err := f.manager.Suspend(ctx, f.room.ID); err != nil {
					t.Fatal(err)
				}
				room, err := f.registry.ArchiveRoom(ctx, f.room.ID)
				if err != nil {
					t.Fatal(err)
				}
				if missing {
					if err := os.RemoveAll(room.DataDir); err != nil {
						t.Fatal(err)
					}
				}
				registry, err := OpenRegistry(ctx, RegistryConfig{Root: f.registry.Root()})
				if err != nil {
					t.Fatal(err)
				}
				if retained := registry.nativeClaims[nativeClaimKey(room.ID, model.ActorSlot2)]; retained != claim {
					t.Fatal("archive checkpoint replaced exact native binding identity")
				}
				if err := registry.identities.Check(ctx, claim); err != nil {
					t.Fatal("archive released native session")
				}
				registry.provisionMu.Lock()
				staged, _, err := registry.stageManagedRoom(ctx, room)
				if err != nil {
					registry.provisionMu.Unlock()
					t.Fatal(err)
				}
				if staged == nil || len(staged.intent.NativeClaims) != 1 {
					registry.provisionMu.Unlock()
					t.Fatal("native deletion lost its exact durable release intent")
				}
				if outcome != "uncommitted" {
					registry.mu.Lock()
					delete(registry.rooms, room.ID)
					_, err = registry.writeCheckpointLocked()
					registry.mu.Unlock()
				}
				registry.provisionMu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				other := claim
				other.Association, other.BindID, other.Generation = nativeidentity.Remote("different-peer", "different-room"), "new-remote-owner", 0
				if outcome == "reused" {
					if err := registry.identities.Release(ctx, claim); err != nil {
						t.Fatal(err)
					}
					if err := registry.identities.Reserve(ctx, other); err != nil {
						t.Fatal(err)
					}
				}
				recovered, err := OpenRegistry(ctx, RegistryConfig{Root: registry.Root()})
				if err != nil {
					t.Fatal(err)
				}
				_, exists := recovered.Room(room.ID)
				if exists != (outcome == "uncommitted") {
					t.Fatal("deletion recovery disagreed with the authoritative checkpoint")
				}
				switch outcome {
				case "uncommitted":
					if err := recovered.identities.Check(ctx, claim); err != nil {
						t.Fatal("uncommitted deletion released archived session")
					}
				case "committed":
					if err := recovered.identities.Check(ctx, claim); !errors.Is(err, nativeidentity.ErrUnowned) {
						t.Fatal("committed deletion retained session after restart")
					}
				case "reused":
					if err := recovered.identities.Check(ctx, other); err != nil {
						t.Fatal("old deletion released a newer Room association")
					}
				}
				if _, err := os.Stat(staged.container); !os.IsNotExist(err) {
					t.Fatal("resolved deletion intent remained pending")
				}
			})
		}
	}
}
