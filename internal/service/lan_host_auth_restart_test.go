package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

// Restart the Registry and both Service managers, retaining only durable facts
// and the original pinned listener configuration. The old fixture's cleanup is
// idempotent; no old Engine or authorization projection is used by the restart.
func restartLANAuthorizationFixture(t *testing.T, old *lanHostFixture) *lanHostFixture {
	t.Helper()
	old.management.lanHost.mu.Lock()
	config := old.management.lanHost.config
	old.management.lanHost.mu.Unlock()
	if err := writeLANHostConfig(old.management.lanHost.path, config); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := old.management.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if old.remote != nil {
		old.remote.Close()
	}
	old.local.Close()
	if err := old.management.runtimes.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(ctx, RegistryConfig{Root: old.management.registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	noSpawn := ProvisionerFunc(func(context.Context, Project, model.ActorID, BindingSpec, string) (Binding, func(context.Context) error, error) {
		t.Error("cold LAN admission attempted to start a vendor adapter")
		return Binding{}, nil, errors.New("unexpected adapter")
	})
	factory := EmbeddedRuntimeFactory(registry, EmbeddedRuntimeConfig{Claude: agent.Config{Command: "missing-do-not-spawn"}, nativeWake: nativeWakerConfig{Wait: func(context.Context, time.Duration) error { return context.Canceled }}})
	manager, err := NewRuntimeManager(registry, factory, RuntimeManagerConfig{Limit: 5, IdleTimeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewManagementServer(ManagementServerConfig{Registry: registry, Runtimes: manager, Provisioner: noSpawn, Token: old.management.Token()})
	if err != nil {
		_ = manager.Shutdown(ctx)
		t.Fatal(err)
	}
	local := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		local.Close()
		if err := manager.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	if status := s.lanHost.status(); !status.Enabled || status.Endpoint != old.invite.Endpoint || status.HostPin != old.invite.HostPin || status.Diagnostic != "" {
		t.Fatalf("restart changed the original listener identity: %+v", status)
	}
	room, ok := registry.Room(old.room.ID)
	if !ok {
		t.Fatal("Registry restart lost the shared Room")
	}
	return &lanHostFixture{management: s, local: local, room: room, owner: old.owner, invite: old.invite}
}

func TestLANColdAuthorizationRebuildPreservesAdmissionAndRejectsStaleMembership(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	client.CloseIdleConnections()
	f = restartLANAuthorizationFixture(t, f)
	ctx := context.Background()
	assertCold := func() {
		t.Helper()
		status := f.management.runtimes.Status(f.room.ID)
		if status.Phase != RuntimeSuspended || status.OccupiesCapacity {
			t.Fatalf("unapproved request activated the cold Room: %+v", status)
		}
		if _, err := f.management.runtimes.runtimeForCompletion(f.room.ID); !errors.Is(err, ErrRuntimeNotReady) {
			t.Fatalf("cold Registry retained an old runtime: %v", err)
		}
	}
	assertDenied := func(c *http.Client, action string, payload any) {
		t.Helper()
		err := lanshare.Call(ctx, c, f.invite, action, payload, nil)
		var remote *lanshare.Error
		if !errors.As(err, &remote) || remote.Status != http.StatusForbidden {
			t.Fatalf("cold %s was not rejected before activation: %v", action, err)
		}
		assertCold()
	}
	assertCold()
	// Even a real pending request's certificate has no member authority.
	assertDenied(client, "summary", nil)
	stranger, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	strangerClient, err := lanshare.NewClient(f.invite, stranger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(strangerClient.CloseIdleConnections)
	assertDenied(strangerClient, "join-status", lanshare.JoinStatusRequest{RequestID: "remote-request"})
	var recovered lanshare.JoinResponse
	if err := lanshare.Call(ctx, client, f.invite, "join-status", lanshare.JoinStatusRequest{RequestID: "remote-request"}, &recovered); err != nil || recovered.Status != "pending" || recovered.Receipt != pending.Receipt || recovered.Room != nil {
		t.Fatalf("cold pending receipt was not reconstructed: %+v, %v", recovered, err)
	}
	if err := f.management.runtimes.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	assertCold()
	if err := lanshare.Call(ctx, client, f.invite, "join", lanshare.JoinRequest{InviteID: f.invite.InviteID, RequestID: "remote-request", Runtime: model.RuntimeCodex, Label: "colleague"}, &recovered); err != nil || recovered.Status != "pending" || recovered.Receipt != pending.Receipt {
		t.Fatalf("same request/key changed after Registry restart: %+v, %v", recovered, err)
	}
	first := f.accept(t, client, pending)
	if first.Room == nil || first.Room.Generation != 1 {
		t.Fatalf("original approval changed its generation: %+v", first)
	}
	if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/lan/revoke", map[string]any{}, nil, f.management.Token()); status != http.StatusOK {
		t.Fatalf("revoke first member: HTTP %d", status)
	}
	var issued struct{ Invite string }
	if status := f.localCall(t, "/api/v1/rooms/"+f.room.ID+"/lan/invite", map[string]any{}, &issued, f.management.Token()); status != http.StatusOK {
		t.Fatalf("issue replacement invitation: HTTP %d", status)
	}
	f.invite, err = lanshare.ParseInvite(issued.Invite)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := lanshare.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	current, err := lanshare.NewClient(f.invite, identity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(current.CloseIdleConnections)
	request := lanshare.JoinRequest{InviteID: f.invite.InviteID, RequestID: "replacement-request", Runtime: model.RuntimeCodex}
	var next lanshare.JoinResponse
	if err := lanshare.Call(ctx, current, f.invite, "join", request, &next); err != nil || next.Status != "pending" {
		t.Fatalf("replacement join: %+v, %v", next, err)
	}
	admitted := f.accept(t, current, next)
	if admitted.Room == nil || admitted.Room.Generation != first.Room.Generation+1 || admitted.Room.BindID == first.Room.BindID {
		t.Fatalf("replacement did not obtain its exact next membership: %+v", admitted)
	}
	f = restartLANAuthorizationFixture(t, f)
	assertCold()
	// The old certificate and its genuine old member headers cannot activate
	// the Room, even though its original request remains retained evidence.
	assertDenied(client, "summary", nil)
	stale := *admitted.Room
	stale.Generation = first.Room.Generation
	base := current.Transport.(lanScopedTransport).base
	current.Transport = lanScopedTransport{base: base, room: &stale}
	assertDenied(current, "status", nil)
	stale.Generation = admitted.Room.Generation
	stale.BindID = first.Room.BindID
	assertDenied(current, "summary", nil)
	current.Transport = base
	assertDenied(current, "summary", nil)
	current.Transport = lanScopedTransport{base: base, room: admitted.Room}
	var summary relay.Summary
	if err := lanshare.Call(ctx, current, f.invite, "summary", nil, &summary); err != nil || !summary.Bindings[admitted.Room.Slot].Associated {
		t.Fatalf("rebuilt active membership could not activate its Room: %+v, %v", summary, err)
	}
	if err := f.management.runtimes.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	assertCold()
	if err := lanshare.Call(ctx, current, f.invite, "join-status", lanshare.JoinStatusRequest{RequestID: request.RequestID}, &recovered); err != nil || recovered.Status != "accepted" || recovered.Receipt != admitted.Receipt || recovered.Room == nil || recovered.Room.BindID != admitted.Room.BindID || recovered.Room.Generation != admitted.Room.Generation {
		t.Fatalf("cold accepted receipt or generation was changed: %+v, %v", recovered, err)
	}
	if err := f.management.runtimes.Suspend(ctx, f.room.ID); err != nil {
		t.Fatal(err)
	}
	assertCold()
	if err := lanshare.Call(ctx, current, f.invite, "join", request, &recovered); err != nil || recovered.Status != "accepted" || recovered.Receipt != admitted.Receipt || recovered.Room == nil || recovered.Room.BindID != admitted.Room.BindID || recovered.Room.Generation != admitted.Room.Generation {
		t.Fatalf("consumed invitation lost same-request admission recovery: %+v, %v", recovered, err)
	}
}
