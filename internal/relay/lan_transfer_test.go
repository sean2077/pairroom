package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestLANTransferRevocationRevalidatesOwnerAfterUnlockedWait(t *testing.T) {
	e, owner, _ := lanEngine(t, nil)
	a := admitLAN(t, e, Digest("transfer peer"))
	interrupted := make(chan struct{})
	transfer, err := e.BeginLANTransfer(context.Background(), a, func() { close(interrupted) })
	if err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		<-done
	}()
	go func() {
		defer close(done)
		defer transfer.Close()
		_ = transfer.Effect(func() error { close(started); <-release; return nil })
	}()
	<-started
	revoked := make(chan error, 1)
	go func() { revoked <- e.RevokeLANMember(owner) }()
	select {
	case <-interrupted:
	case <-time.After(2 * time.Second):
		t.Fatal("revocation failed to interrupt the in-flight transfer")
	}
	if next, err := e.BeginLANTransfer(context.Background(), a, nil); !errors.Is(err, ErrAuth) {
		if next != nil {
			next.Close()
		}
		t.Fatal("new transfer entered a stopping membership")
	}
	// The network write is still blocked, but the Room remains usable. A
	// replaced owner invalidates the old caller's authority before commit.
	b, err := e.Bind(model.ActorSlot1, BindRequest{BindID: "new-owner", CredentialHash: Digest("new-secret"), SessionID: "new-session", Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-revoked; !errors.Is(err, ErrAuth) {
		t.Fatalf("stale owner revoked membership after unlocked wait: %v", err)
	}
	<-done
	if _, err := e.Inspect(a); err != nil {
		t.Fatalf("failed stale-owner operation committed revocation: %v", err)
	}
	if err := transfer.Effect(func() error { t.Error("closed transfer released another effect"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled transfer became usable again")
	}
	currentOwner := Auth{Slot: b.Slot, BindID: b.BindID, Generation: b.Generation, SessionID: "new-session", Secret: "new-secret"}
	if err := e.RevokeLANMember(currentOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := e.BeginLANTransfer(context.Background(), a, nil); !errors.Is(err, ErrAuth) {
		t.Fatal("revoked exact generation started another transfer")
	}
}

func TestLANTransferRequestCancellationAndCloseAreIdempotent(t *testing.T) {
	e, owner, _ := lanEngine(t, nil)
	a := admitLAN(t, e, Digest("cancelled peer"))
	if _, err := e.BeginLANTransfer(context.Background(), owner, nil); !errors.Is(err, ErrAuth) {
		t.Fatal("local owner was treated as a LAN member")
	}
	ctx, cancel := context.WithCancel(context.Background())
	interrupted := make(chan struct{})
	transfer, err := e.BeginLANTransfer(ctx, a, func() { close(interrupted) })
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-interrupted:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled HTTP request retained its transfer")
	}
	transfer.Close()
	transfer.Close()
	if err := transfer.Effect(func() error { t.Error("cancelled transfer ran an effect"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request still admitted: %v", err)
	}
	if _, err := e.BeginLANTransfer(ctx, a, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("already cancelled request registered another transfer")
	}
	if err := e.RevokeLANMember(owner); err != nil {
		t.Fatal(err)
	}
}
