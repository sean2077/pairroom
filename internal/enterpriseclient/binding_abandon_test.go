package enterpriseclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
)

func TestEnterpriseBindingDetachPreservesUnknownAndDoesNotReleaseNewOwner(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "abandon", model.ActorSlot1, model.RuntimeClaude)
	ctx := context.Background()
	r, err := e.binding.read()
	if err != nil {
		t.Fatal(err)
	}
	// A crash fixture retains the exact private candidate after an uncertain
	// completion. Offline retirement cannot infer whether the server committed.
	r.Phase = "completion_unknown"
	if err := e.binding.save(r); err != nil {
		t.Fatal(err)
	}
	f.server.Close()
	if err := e.binding.DetachLocal(ctx, e.auth); err != nil {
		t.Fatal(err)
	}
	next, err := e.binding.read()
	if err != nil || next.DetachedAt.IsZero() {
		t.Fatalf("local marker: %v", err)
	}
	next.DetachedAt = time.Time{}
	if next != r {
		t.Fatal("detach changed original unknown operation, candidate, or receipt")
	}
	if _, err := e.binding.ResumeBinding(ctx); !errors.Is(err, ErrBindingInactive) {
		t.Fatalf("detached attempt resumed: %v", err)
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		t.Fatal(err)
	}
	newClaim := nativeidentity.Claim{Runtime: e.auth.Runtime, SessionID: e.auth.SessionID, Association: nativeidentity.Enterprise(f.server.URL, "another-deployment", "another-room"), BindID: "new-binding"}
	if err := identities.Reserve(ctx, newClaim); err != nil {
		t.Fatalf("explicit detach did not release exact pending reservation: %v", err)
	}
	if err := e.binding.DetachLocal(ctx, e.auth); err != nil {
		t.Fatal(err)
	}
	if err := identities.Check(ctx, newClaim); err != nil {
		t.Fatalf("repeated old detach released new owner: %v", err)
	}
}

func TestEnterpriseBindingDeniedOrExpiredRequiresExplicitLocalExit(t *testing.T) {
	for _, phase := range []string{"denied", "expired"} {
		t.Run(phase, func(t *testing.T) {
			f := newNativeWireFixture(t)
			e := f.endpoint(t, phase, model.ActorSlot1, model.RuntimeClaude)
			ctx := context.Background()
			meta, err := e.binding.ResumeBinding(ctx)
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			if phase == "denied" {
				f.requests[meta.RequestID].info.Status = "denied"
			} else {
				f.requests[meta.RequestID].info.RequestedAt = time.Now().Add(-11 * time.Minute)
				f.requests[meta.RequestID].info.ExpiresAt = time.Now().Add(-time.Minute)
			}
			f.mu.Unlock()
			meta, err = e.binding.ResumeBinding(ctx)
			want := ErrBindingRequestDenied
			if phase == "expired" {
				want = ErrBindingRequestExpired
			}
			if !errors.Is(err, want) || meta.Status != phase {
				t.Fatalf("terminal admission: %s %v", meta.Status, err)
			}
			f.server.Close()
			if err := e.binding.DetachLocal(ctx, e.auth); err != nil {
				t.Fatal(err)
			}
			r, err := e.binding.read()
			if err != nil || r.Phase != phase || r.DetachedAt.IsZero() {
				t.Fatalf("terminal result lost at local exit: %s %v", r.Phase, err)
			}
		})
	}
}

func TestEnterpriseBindingDetachReleasesConfirmedRecordWithPendingNativeIndex(t *testing.T) {
	f := newNativeWireFixture(t)
	e := f.endpoint(t, "pending-index", model.ActorSlot1, model.RuntimeClaude)
	ctx := context.Background()
	meta, err := e.binding.ResumeBinding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.requests[meta.RequestID].approved = true
	f.mu.Unlock()
	meta, err = e.binding.ResumeBinding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately omit local generation promotion, as if the process stopped
	// between saved completion and workspace activation.
	e.auth.Generation = meta.Generation
	if err := e.binding.DetachLocal(ctx, e.auth); err != nil {
		t.Fatal(err)
	}
	identities, err := nativeidentity.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := identities.Reserve(ctx, nativeidentity.Claim{Runtime: e.auth.Runtime, SessionID: e.auth.SessionID, Association: nativeidentity.Enterprise(f.server.URL, "dep-other", "room-other"), BindID: "new-attempt"}); err != nil {
		t.Fatalf("pending generation-zero reservation remained: %v", err)
	}
}
