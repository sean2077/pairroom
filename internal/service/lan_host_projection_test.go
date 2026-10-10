package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func lanAuthorizationFact(t *testing.T, room, kind string, at time.Time, value any) model.Event {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return model.Event{RoomID: room, Kind: kind, CreatedAt: at, Data: data}
}

func TestLANColdAuthorizationBoundsRepeatedRequestsDuringAppendAndRebuild(t *testing.T) {
	now := time.Now().UTC()
	room := Room{ID: "bounded-joins", Sharing: "lan"}
	registry := &Registry{}
	invite := relay.LANInvite{ID: "one-invitation", ExpiresAt: now.Add(time.Minute)}
	events := []model.Event{lanAuthorizationFact(t, room.ID, relay.EventLANInvite, now.Add(-time.Minute), invite)}
	registry.observeLANAuthorization(events[0])
	key := relay.Digest("one-requesting-key")
	const attempts = 5000
	for i := 0; i < attempts; i++ {
		at := now.Add(-time.Minute).Add(time.Duration(i+1) * time.Microsecond)
		request := relay.LANJoinRequest{InviteID: invite.ID, RequestID: fmt.Sprintf("attempt-%04d", i), Key: key, Runtime: model.RuntimeCodex, CreatedAt: at}
		event := lanAuthorizationFact(t, room.ID, relay.EventLANJoin, at, request)
		events = append(events, event)
		registry.observeLANAuthorization(event)
	}
	assertBounded := func() {
		t.Helper()
		registry.lanAuthMu.RLock()
		defer registry.lanAuthMu.RUnlock()
		projection := registry.lanAuth[room.ID]
		if projection == nil || projection.invalid || len(projection.requests) != 1 || len(projection.invites) != 1 || len(projection.admitted) != 0 {
			t.Fatalf("historical retry count escaped the current authorization projection: %+v", projection)
		}
		if _, ok := projection.requests["attempt-4999"]; !ok {
			t.Fatal("projection did not retain the newest recoverable request")
		}
	}
	assertBounded()
	if err := registry.resetLANAuthorization(room, events); err != nil {
		t.Fatal(err)
	}
	assertBounded()
}

func TestLANColdAuthorizationPrunesAfterAdmissionFactsAndKeepsExpiryAnswers(t *testing.T) {
	now := time.Now().UTC()
	room := Room{ID: "retained-admissions", Sharing: "lan"}
	oldAt := now.Add(-time.Hour)
	oldInvite := relay.LANInvite{ID: "old-invite", ExpiresAt: oldAt.Add(10 * time.Minute)}
	oldRequest := relay.LANJoinRequest{InviteID: oldInvite.ID, RequestID: "retired-admission", Key: relay.Digest("old-key"), Runtime: model.RuntimeCodex, CreatedAt: oldAt.Add(time.Minute)}
	oldMember := relay.LANMember{RequestID: oldRequest.RequestID, InviteID: oldInvite.ID, Binding: relay.Binding{Slot: model.ActorSlot2, Runtime: oldRequest.Runtime, RemoteKey: oldRequest.Key, BindID: "old-binding", Generation: 1, Active: true}}
	revoked := oldMember
	revoked.Binding.Active = false
	windowInvite := relay.LANInvite{ID: "expired-invite", ExpiresAt: now.Add(-time.Minute)}
	windowRequest := relay.LANJoinRequest{InviteID: windowInvite.ID, RequestID: "expired-request", Key: relay.Digest("waiting-key"), Runtime: model.RuntimeGrok, CreatedAt: now.Add(-2 * time.Minute)}
	newInvite := relay.LANInvite{ID: "current-invite", ExpiresAt: now.Add(10 * time.Minute)}
	newRequest := relay.LANJoinRequest{InviteID: newInvite.ID, RequestID: "current-admission", Key: relay.Digest("new-key"), Runtime: model.RuntimeGemini, CreatedAt: now}
	newMember := relay.LANMember{RequestID: newRequest.RequestID, InviteID: newInvite.ID, Binding: relay.Binding{Slot: model.ActorSlot2, Runtime: newRequest.Runtime, RemoteKey: newRequest.Key, BindID: "new-binding", Generation: 2, Active: true}}
	events := []model.Event{
		lanAuthorizationFact(t, room.ID, relay.EventLANInvite, oldAt, oldInvite),
		lanAuthorizationFact(t, room.ID, relay.EventLANJoin, oldRequest.CreatedAt, oldRequest),
		lanAuthorizationFact(t, room.ID, relay.EventLANMember, oldAt.Add(2*time.Minute), oldMember),
		lanAuthorizationFact(t, room.ID, relay.EventLANMember, oldAt.Add(3*time.Minute), revoked),
		lanAuthorizationFact(t, room.ID, relay.EventLANInvite, now.Add(-11*time.Minute), windowInvite),
		lanAuthorizationFact(t, room.ID, relay.EventLANJoin, windowRequest.CreatedAt, windowRequest),
		lanAuthorizationFact(t, room.ID, relay.EventLANInvite, now, newInvite),
		lanAuthorizationFact(t, room.ID, relay.EventLANJoin, newRequest.CreatedAt, newRequest),
		lanAuthorizationFact(t, room.ID, relay.EventLANMember, now, newMember),
	}
	registry := &Registry{}
	if err := registry.resetLANAuthorization(room, events); err != nil {
		t.Fatal(err)
	}
	projection := registry.lanAuth[room.ID]
	if len(projection.requests) != 3 || !projection.admitted[oldRequest.RequestID] || !projection.admitted[newRequest.RequestID] {
		t.Fatalf("replay pruned a later admission or a still-answerable expiry: %+v", projection)
	}
	if _, ok := projection.invites[windowInvite.ID]; !ok {
		t.Fatal("a new invitation discarded the prior request's expiry-answer window")
	}
	projection.prune(now.Add(11 * time.Minute))
	if len(projection.requests) != 2 || len(projection.admitted) != 2 || projection.member.RequestID != newRequest.RequestID {
		t.Fatalf("expiry clipping lost durable admission facts: %+v", projection)
	}
	if _, ok := projection.requests[windowRequest.RequestID]; ok {
		t.Fatal("unadmitted request survived beyond its expiry-answer window")
	}
	if _, ok := projection.invites[windowInvite.ID]; ok {
		t.Fatal("forgotten request kept an unneeded expired invitation")
	}
}

func TestLANColdForgottenJoinDoesNotActivateAfterRegistryRestart(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	fixture := newLANHostFixture(t)
	client, _, _ := fixture.join(t)
	var newest lanshare.JoinResponse
	if err := lanshare.Call(context.Background(), client, fixture.invite, "join", lanshare.JoinRequest{InviteID: fixture.invite.InviteID, RequestID: "newest-request", Runtime: model.RuntimeCodex}, &newest); err != nil || newest.Status != "pending" {
		t.Fatalf("new request did not supersede the same key's earlier attempt: %+v %v", newest, err)
	}
	client.CloseIdleConnections()
	fixture = restartLANAuthorizationFixture(t, fixture)
	err := lanshare.Call(context.Background(), client, fixture.invite, "join-status", lanshare.JoinStatusRequest{RequestID: "remote-request"}, nil)
	var remote *lanshare.Error
	if !errors.As(err, &remote) || remote.Status != http.StatusForbidden {
		t.Fatalf("forgotten request survived cold authorization: %v", err)
	}
	if status := fixture.management.runtimes.Status(fixture.room.ID); status.Phase != RuntimeSuspended || status.OccupiesCapacity {
		t.Fatalf("forgotten request activated or occupied its Room: %+v", status)
	}
	var recovered lanshare.JoinResponse
	if err := lanshare.Call(context.Background(), client, fixture.invite, "join-status", lanshare.JoinStatusRequest{RequestID: "newest-request"}, &recovered); err != nil || recovered.Status != "pending" || recovered.Receipt != newest.Receipt {
		t.Fatalf("bounded projection lost the current request's recovery: %+v %v", recovered, err)
	}
}
