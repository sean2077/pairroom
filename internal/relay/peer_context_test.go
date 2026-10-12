package relay

import (
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestPeerProjectsEffectiveRuntimeWithItsBinding(t *testing.T) {
	t.Run("legacy local", func(t *testing.T) {
		e, auth, _ := testEngine(t)
		peer, err := e.Peer(auth[model.ActorSlot1])
		if err != nil || peer.Runtime != model.RuntimeCodex || peer.SessionID != auth[model.ActorSlot2].SessionID || peer.Generation != 1 {
			t.Fatalf("legacy peer = %+v, %v", peer, err)
		}
		if err := e.Unbind(peer.Slot); err != nil {
			t.Fatal(err)
		}
		peer, err = e.Peer(auth[model.ActorSlot1])
		if err != nil || peer.Active || peer.Runtime != model.RuntimeCodex {
			t.Fatalf("legacy typed vacancy = %+v, %v", peer, err)
		}
	})
	t.Run("legacy LAN", func(t *testing.T) {
		e, owner, _ := lanEngine(t, nil)
		peer, err := e.Peer(owner)
		if err != nil || peer.Slot != model.ActorSlot2 || peer.Runtime != model.RuntimeAwaitingPeer || peer.Active {
			t.Fatalf("LAN vacancy = %+v, %v", peer, err)
		}
		member := admitLANRuntime(t, e, model.RuntimeCodex)
		peer, err = e.Peer(owner)
		if err != nil || peer.Runtime != model.RuntimeCodex || peer.Generation != member.Generation || peer.RemoteKey != member.MemberKey {
			t.Fatalf("admitted LAN peer = %+v, %v", peer, err)
		}
		if err := e.RevokeLANMember(); err != nil {
			t.Fatal(err)
		}
		peer, err = e.Peer(owner)
		if err != nil || peer.Active || peer.Runtime != model.RuntimeAwaitingPeer || peer.Generation != member.Generation {
			t.Fatalf("revoked LAN peer = %+v, %v", peer, err)
		}
	})
	t.Run("dynamic local", func(t *testing.T) {
		e, _ := dynamicEngine(t, "")
		a := dynamicBind(t, e, "", dynamicRequest("owner", model.RuntimeClaude))
		peer, err := e.Peer(a)
		if err != nil || peer.Slot != model.ActorSlot2 || peer.Runtime != model.RuntimeAwaitingBinding || peer.Active {
			t.Fatalf("dynamic vacancy = %+v, %v", peer, err)
		}
		first := dynamicBind(t, e, "", dynamicRequest("peer", model.RuntimeGrok))
		req := dynamicRequest("replacement", model.RuntimeGemini)
		req.Replace = true
		next := dynamicBind(t, e, first.Slot, req)
		peer, err = e.Peer(a)
		if err != nil || peer.Runtime != model.RuntimeGemini || peer.Generation != next.Generation || peer.BindID != next.BindID || peer.SessionID != next.SessionID {
			t.Fatalf("replacement peer = %+v, %v", peer, err)
		}
		if err := e.Unbind(next.Slot); err != nil {
			t.Fatal(err)
		}
		peer, err = e.Peer(a)
		if err != nil || peer.Active || peer.Runtime != model.RuntimeAwaitingBinding || peer.Generation != next.Generation || peer.SessionID != "" {
			t.Fatalf("dynamic unbound peer = %+v, %v", peer, err)
		}
	})
}
