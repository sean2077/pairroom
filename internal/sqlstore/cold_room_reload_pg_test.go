package sqlstore

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/relay"
)

// The current Service has no resident Room engine cache. Discarding a borrowed
// projection within the same ownership epoch must not perform process recovery
// or retire the Store's live delivery claims and original operation receipts.
func TestSameOwnerColdRoomProjectionReloadPreservesLiveClaimAndReceiptPG(t *testing.T) {
	b, owner, member := recoveryPGPair(t)
	for i := range 24 {
		id := strings.Repeat("x", i+1)
		relayPGCommit(t, b, relayPGCommand(t, b, owner, "settled-"+id, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: strings.Repeat("history-", 512), ExpectedPeerGeneration: 1}))
		claim := relayPGCommit(t, b, relayPGCommand(t, b, member, "claim-"+id, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
		relayPGCommit(t, b, relayPGCommand(t, b, member, "ack-"+id, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: claim.Relay.Claim.ID, Receipt: claim.Relay.Claim.Receipt}))
	}
	relayPGCommit(t, b, relayPGCommand(t, b, owner, "live-on-idle-room", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "live durable claim", ExpectedPeerGeneration: 1}))
	claimCommand := relayPGCommand(t, b, member, "original-live-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim})
	original := relayPGCommit(t, b, claimCommand)
	if original.Relay.Claim == nil {
		t.Fatal("no live original claim")
	}
	epoch, pid, start := b.f.store.lease.epoch, b.f.store.lease.pid, b.f.store.lease.start
	auth := relay.EnterpriseAuth{Deployment: b.owner.Auth.Deployment, UserID: b.owner.Auth.UserID, SessionID: b.owner.Auth.SessionID}
	open := func() *relay.Engine {
		e, err := relay.OpenEnterprise(t.Context(), relay.Config{RoomID: "binding-room"}, NewRoomPort(b.f.store), auth)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	first := open()
	expected := first.Snapshot()
	liveProjected := false
	for _, message := range expected.Messages {
		if message.ID == original.Relay.Claim.ID && message.State == "delivering" {
			liveProjected = true
		}
	}
	if !liveProjected {
		t.Fatal("same-owner cold projection reclassified or lost its live claim")
	}
	if err := first.CloseContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	before := ackRetryPGCounts(t, b)
	// Warm bounded driver/replay allocations before measuring retained memory.
	for range 8 {
		e := open()
		if err := e.CloseContext(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	var baseline, final runtime.MemStats
	runtime.ReadMemStats(&baseline)
	for range 96 {
		e := open()
		if !reflect.DeepEqual(e.Snapshot(), expected) {
			t.Fatal("cold same-owner projection changed durable Room state")
		}
		if err := e.CloseContext(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	runtime.GC()
	runtime.ReadMemStats(&final)
	if final.HeapAlloc > baseline.HeapAlloc+(8<<20) {
		t.Fatalf("discarded Room projections retained unbounded fixture memory: before=%d after=%d", baseline.HeapAlloc, final.HeapAlloc)
	}
	if ackRetryPGCounts(t, b) != before || b.f.store.lease.epoch != epoch || b.f.store.lease.pid != pid || !b.f.store.lease.start.Equal(start) {
		t.Fatal("same-owner cold reload changed lease, facts, audit or operation receipts")
	}
	resolved, err := b.f.store.ResolveRelayOperation(t.Context(), claimCommand)
	if err != nil || resolved.Outcome != OutcomeReplayed || !reflect.DeepEqual(resolved.Relay, original.Relay) || !reflect.DeepEqual(resolved.Facts, original.Facts) {
		t.Fatal("same-owner cold reload lost the original accepted claim result")
	}
	messages := ackRetryPGMessages(t, b)
	if messages[original.Relay.Claim.ID].State != "delivering" || messages[original.Relay.Claim.ID].Receipt != original.Relay.Claim.Receipt {
		t.Fatal("same-owner cold reload reaped or replaced a live claim")
	}
	settled := relayPGCommit(t, b, relayPGCommand(t, b, member, "original-ack-after-cold-reload", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: original.Relay.Claim.ID, Receipt: original.Relay.Claim.Receipt}))
	if !settled.Relay.HandedOff {
		t.Fatal("original live receipt cannot settle after same-owner cold reload")
	}
	t.Logf("96 discarded cold projections: retained heap baseline=%d final=%d; original lease/claim preserved", baseline.HeapAlloc, final.HeapAlloc)
}
