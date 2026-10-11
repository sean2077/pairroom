package service

import (
	"context"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/relayclient"
)

func TestLANPeerSelectionTracksRevocationAndSuccessorAcrossRestart(t *testing.T) {
	relayclient.IsolateNativeCaller(t)
	f := newLANHostFixture(t)
	client, pending, _ := f.join(t)
	f.accept(t, client, pending)
	// Starting with an admitted peer captures that selection in the Runtime's
	// durable Room snapshot, rather than its original awaiting-peer selection.
	f = restartLANAuthorizationFixture(t, f)
	ctx := context.Background()
	n, err := f.management.sharedNativeRuntime(ctx, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertSelection := func(native *nativeHostRuntime, want model.RuntimeKind, handle string) {
		t.Helper()
		room, ok := f.management.registry.Room(f.room.ID)
		if !ok {
			t.Fatal("shared Room disappeared")
		}
		snapshot := native.snapshot()
		for _, current := range []Room{room, snapshot["room"].(Room)} {
			peer := current.Agents[model.ActorSlot2]
			if want == model.RuntimeAwaitingPeer {
				if !peer.AwaitingPeer || peer.Runtime != "" || current.RuntimeNames[model.ActorSlot2] != "" {
					t.Fatalf("retired member retained a Runtime: %+v", current)
				}
			} else if peer.AwaitingPeer || peer.Runtime != want {
				t.Fatalf("peer selection = %+v, want %s", peer, want)
			}
			if current.Agents[model.ActorSlot1].Runtime != model.RuntimeClaude {
				t.Fatal("peer replacement changed the owner's Runtime")
			}
		}
		identities := snapshot["identities"].(map[model.ActorID]model.ParticipantIdentity)
		if identities[model.ActorSlot2].MentionHandle != handle {
			t.Fatalf("peer handle = %q, want %q", identities[model.ActorSlot2].MentionHandle, handle)
		}
	}
	assertSelection(n, model.RuntimeCodex, "@codex")
	if err := n.engine.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	assertSelection(n, model.RuntimeAwaitingPeer, "")
	invite, err := n.engine.CreateLANInvite()
	if err != nil {
		t.Fatal(err)
	}
	key := relay.Digest("gemini-successor")
	if _, _, err := n.engine.RequestLANJoin(relay.LANJoinRequest{InviteID: invite.ID, RequestID: "gemini-successor", Key: key, Runtime: model.RuntimeGemini}); err != nil {
		t.Fatal(err)
	}
	if _, err := n.engine.AcceptLANJoin("gemini-successor", key); err != nil {
		t.Fatal(err)
	}
	assertSelection(n, model.RuntimeGemini, "@gemini")
	if err := n.engine.RevokeLANMember(); err != nil {
		t.Fatal(err)
	}
	assertSelection(n, model.RuntimeAwaitingPeer, "")
	f = restartLANAuthorizationFixture(t, f)
	n, err = f.management.sharedNativeRuntime(ctx, f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	assertSelection(n, model.RuntimeAwaitingPeer, "")
}
