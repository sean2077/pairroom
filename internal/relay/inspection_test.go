package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestNativeSummaryIsBoundedBodyFreeAndAuthenticated(t *testing.T) {
	e, auth, _ := testEngine(t)
	text := strings.Repeat("private-evidence-", 512)
	for i := 0; i < 40; i++ {
		if _, err := e.Send(auth["slot1"], SendRequest{ID: fmt.Sprintf("m%d", i), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	// Make one real delivery uncertain without changing or replaying its body.
	claim, err := e.Claim(context.Background(), auth["slot2"], false)
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	err = e.reapLocked(true)
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	summary, err := e.AuthSummary(auth["slot1"])
	if err != nil {
		t.Fatal(err)
	}
	if summary.Inboxes["slot2"].Queued != 39 || summary.Inboxes["slot2"].Unknown != 1 || len(summary.Recovery) != 1 || summary.Recovery[0].ID != claim.ID {
		t.Fatalf("summary lost transport counts: %+v", summary)
	}
	data, err := json.Marshal(summary)
	if err != nil || len(data) > 2048 {
		t.Fatalf("summary grew with history: bytes=%d err=%v", len(data), err)
	}
	for _, forbidden := range []string{"private-evidence", "session-slot1", "session-slot2", "/optional/transcript", "secret-slot1", claim.Receipt} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("summary leaked %q", forbidden)
		}
	}
	bad := auth["slot1"]
	bad.Secret = "wrong"
	if _, err := e.AuthSummary(bad); !errors.Is(err, ErrAuth) {
		t.Fatal("summary bypassed auth", err)
	}
}

// A replayed pre-upgrade binding without an official session keeps the documented
// body-free projection: relay status --brief still explains the rebind step, and
// no inbox, peer or sequence state becomes readable before association.
func TestNativeSummaryKeepsUnassociatedBindingProjection(t *testing.T) {
	e, auth, _ := testEngine(t)
	slot := model.ActorSlot1
	generation := e.bindings[slot].Generation + 1
	e.mu.Lock()
	err := e.append(EventBinding, slot, bindingFact{Binding: Binding{Slot: slot, BindID: "bind-unassociated", Generation: generation, Active: true, ParkEnabled: true}, CredentialHash: Digest("unassociated-secret")})
	e.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	unassociated := Auth{Slot: slot, BindID: "bind-unassociated", Generation: generation, Secret: "unassociated-secret"}
	if _, err := e.Send(auth[model.ActorSlot2], SendRequest{ID: "queued", Text: "body"}); err != nil {
		t.Fatal(err)
	}
	summary, err := e.AuthSummary(unassociated)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Sequence != 0 || len(summary.Inboxes) != 0 || len(summary.LastWake) != 0 || summary.Bindings[slot].Associated || !summary.Bindings[slot].Active {
		t.Fatalf("unassociated summary exposed state: %+v", summary)
	}
	if _, peer := summary.Bindings[model.ActorSlot2]; peer {
		t.Fatal("unassociated summary exposed the peer binding")
	}
	if !strings.Contains(summary.Notice, "not associated") {
		t.Fatalf("unassociated summary lost guidance: %q", summary.Notice)
	}
	data, err := json.Marshal(summary)
	if err != nil || strings.Contains(string(data), "body") {
		t.Fatalf("unassociated summary leaked work: %s %v", data, err)
	}
}

func TestNativeSequenceAvoidsSnapshotAllocations(t *testing.T) {
	e, auth, _ := testEngine(t)
	_, err := e.Send(auth["slot1"], SendRequest{ID: "sample", Text: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := e.Sequence(), e.Snapshot().Sequence; got != want {
		t.Fatalf("sequence=%d want=%d", got, want)
	}
	if allocations := testing.AllocsPerRun(100, func() { _ = e.Sequence() }); allocations != 0 {
		t.Fatalf("cursor lookup allocated: %f", allocations)
	}
}

func TestNativeActiveAuthChecksCurrentGenerationDuringDrain(t *testing.T) {
	e, auth, _ := testEngine(t)
	before := e.Sequence()
	e.SetDraining(true)
	if err := e.CheckAuth(auth["slot1"]); err != nil {
		t.Fatal("completion admission blocked by drain", err)
	}
	if e.Sequence() != before {
		t.Fatal("admission appended an event")
	}
	e.SetDraining(false)
	if err := e.Unbind("slot1"); err != nil {
		t.Fatal(err)
	}
	if err := e.CheckAuth(auth["slot1"]); !errors.Is(err, ErrAuth) {
		t.Fatal("revoked credentials accepted", err)
	}
}
