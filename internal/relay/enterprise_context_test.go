package relay

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestEnterpriseContextUsesSharedBodyFreeProjectionWithoutClaimOrReap(t *testing.T) {
	snapshot := publicEnterpriseProjectionFixture(t)
	context, err := DescribeEnterpriseSnapshot(snapshot, model.ActorSlot2, "binding-two", 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if !context.Summary.Bindings[model.ActorSlot1].Associated || !context.Summary.Bindings[model.ActorSlot2].Associated || context.Binding.ParkEnabled || context.Binding.SessionID != "" || context.Peer.SessionID != "" || context.PeerGeneration != 1 || context.Summary.Inboxes[model.ActorSlot2].Delivering != 1 {
		t.Fatalf("incorrect enterprise transport projection: %+v", context)
	}
	if !strings.Contains(context.Bootstrap, "@agent1") || context.Collaboration == "" {
		t.Fatal("frozen protocol/context missing")
	}
	data, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"visible body", "visible quote", "private-original-claim-receipt", `"receipt"`, `"session_id"`, `"messages"`, `"transcript_path"`, `"credential_hash"`} {
		if strings.Contains(string(data), private) {
			t.Fatalf("context exposed private transcript material %s", private)
		}
	}
	if _, err := DescribeEnterpriseSnapshot(snapshot, model.ActorSlot2, "replaced-binding", 1, time.Now().UTC()); err == nil {
		t.Fatal("old binding received current bootstrap")
	}
	if _, err := DescribeEnterpriseSnapshot(snapshot, model.ActorSlot2, "binding-two", 2, time.Now().UTC()); err == nil {
		t.Fatal("wrong generation received current bootstrap")
	}
}

func TestEnterpriseContextPeekDistinguishesLeaseExpiryWithoutWriting(t *testing.T) {
	snapshot := publicEnterpriseProjectionFixture(t)
	var claim messageFact
	if err := json.Unmarshal(snapshot.Events[len(snapshot.Events)-1].Data, &claim); err != nil {
		t.Fatal(err)
	}
	before, err := DescribeEnterpriseSnapshot(snapshot, model.ActorSlot2, "binding-two", 1, claim.ClaimedAt.Add(DeliveryLease-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	after, err := DescribeEnterpriseSnapshot(snapshot, model.ActorSlot2, "binding-two", 1, claim.ClaimedAt.Add(DeliveryLease))
	if err != nil {
		t.Fatal(err)
	}
	if before.ClaimReady || !after.ClaimReady || before.Summary.Inboxes[model.ActorSlot2].Delivering != 1 || after.Summary.Inboxes[model.ActorSlot2].Delivering != 1 {
		t.Fatal("peek claimed/reaped delivery or hid the expiry work")
	}
}
