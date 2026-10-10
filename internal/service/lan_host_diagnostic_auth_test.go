package service

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestLANColdHostDiagnosticsRequireExactMembership(t *testing.T) {
	for _, state := range []string{"suspended", "fail_closed"} {
		t.Run(state, func(t *testing.T) {
			relayclient.IsolateNativeCaller(t)
			fixture := newLANHostFixture(t)
			member, pending, _ := fixture.join(t)
			newClient := func() *http.Client {
				t.Helper()
				identity, err := lanshare.NewIdentity()
				if err != nil {
					t.Fatal(err)
				}
				client, err := lanshare.NewClient(fixture.invite, identity)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(client.CloseIdleConnections)
				return client
			}
			waiting := newClient()
			if err := lanshare.Call(context.Background(), waiting, fixture.invite, "join", lanshare.JoinRequest{InviteID: fixture.invite.InviteID, RequestID: "other-pending-request", Runtime: model.RuntimeGrok}, nil); err != nil {
				t.Fatal(err)
			}
			admitted := fixture.accept(t, member, pending)
			stranger := newClient()
			stranger.Transport = lanScopedTransport{base: stranger.Transport, room: admitted.Room}
			base := member.Transport.(lanScopedTransport).base
			missing := *member
			missing.Transport = base
			staleGeneration := *admitted.Room
			staleGeneration.Generation++
			wrongGeneration := *member
			wrongGeneration.Transport = lanScopedTransport{base: base, room: &staleGeneration}
			staleBinding := *admitted.Room
			staleBinding.BindID = "different-binding"
			wrongBinding := *member
			wrongBinding.Transport = lanScopedTransport{base: base, room: &staleBinding}
			if err := fixture.management.runtimes.Suspend(context.Background(), fixture.room.ID); err != nil {
				t.Fatal(err)
			}
			if state == "fail_closed" {
				fixture.management.registry.mu.Lock()
				_ = fixture.management.registry.poisonLocked(errors.New("fixture checkpoint failure"))
				fixture.management.registry.mu.Unlock()
			}
			assertCold := func() {
				t.Helper()
				status := fixture.management.runtimes.Status(fixture.room.ID)
				if status.Phase != RuntimeSuspended || status.OccupiesCapacity {
					t.Fatalf("diagnostic changed the hosting Room lifecycle: %+v", status)
				}
			}
			for name, client := range map[string]*http.Client{"unknown_key": stranger, "pending": waiting, "missing_headers": &missing, "wrong_generation": &wrongGeneration, "wrong_binding": &wrongBinding} {
				for _, action := range []string{"doctor", "status"} {
					err := lanshare.Call(context.Background(), client, fixture.invite, action, nil, nil)
					var remote *lanshare.Error
					if !errors.As(err, &remote) || remote.Status != http.StatusForbidden || remote.Code == lanshare.HostUnavailableCode {
						t.Fatalf("%s %s received host diagnostics before membership authentication: %v", name, action, err)
					}
					assertCold()
				}
			}
			err := lanshare.Call(context.Background(), member, fixture.invite, "doctor", nil, nil)
			var remote *lanshare.Error
			if !errors.As(err, &remote) || remote.Status != http.StatusServiceUnavailable || remote.Code != lanshare.HostUnavailableCode {
				t.Fatalf("authenticated member lost the actionable host diagnostic: %v", err)
			}
			assertCold()
			if state == "fail_closed" {
				err = lanshare.Call(context.Background(), member, fixture.invite, "status", nil, nil)
				if !errors.As(err, &remote) || remote.Status != http.StatusServiceUnavailable || remote.Code != lanshare.HostUnavailableCode {
					t.Fatalf("valid membership was mistaken for Registry failure: %v", err)
				}
				assertCold()
				return
			}
			if err := lanshare.Call(context.Background(), member, fixture.invite, "status", nil, nil); err != nil {
				t.Fatalf("authenticated activating command failed: %v", err)
			}
			if err := lanshare.Call(context.Background(), member, fixture.invite, "doctor", nil, nil); err != nil {
				t.Fatalf("doctor failed after explicit member activation: %v", err)
			}
			native, err := fixture.management.sharedNativeRuntime(context.Background(), fixture.room.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := native.engine.RevokeLANMember(fixture.owner); err != nil {
				t.Fatal(err)
			}
			if err := fixture.management.runtimes.Suspend(context.Background(), fixture.room.ID); err != nil {
				t.Fatal(err)
			}
			err = lanshare.Call(context.Background(), member, fixture.invite, "doctor", nil, nil)
			if !errors.As(err, &remote) || remote.Status != http.StatusForbidden || remote.Code == lanshare.HostUnavailableCode {
				t.Fatalf("revoked member received a valid-membership diagnostic: %v", err)
			}
			assertCold()
		})
	}
}
