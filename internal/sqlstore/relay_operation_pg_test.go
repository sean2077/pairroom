package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func relayPGPair(t *testing.T, transform func(string) string) (*bindingPGState, BindingCredential, BindingCredential) {
	t.Helper()
	b := bindingPGFixture(t, transform)
	enrollment, device := devicePGEnroll(t, b.f, b.owner, "relay-owner")
	source := sha256.Sum256([]byte("relay-owner-source"))
	registered, err := b.f.store.RegisterAgent(context.Background(), device.Auth, "register-relay-owner", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeClaude)
	if err != nil {
		t.Fatal(err)
	}
	owner := *b
	owner.device, owner.enrollment, owner.agentID = device, enrollment, registered.Control.AgentID
	_, first := bindingPGComplete(t, &owner, bindingPGCommand(&owner, "relay-owner", model.ActorSlot1, 1, false))
	_, second := bindingPGComplete(t, b, bindingPGCommand(b, "relay-member", model.ActorSlot2, 1, false))
	return b, first, second
}

func relayPGCommand(t *testing.T, b *bindingPGState, credential BindingCredential, id string, intent relay.EnterpriseOperationIntent) RelayCommand {
	t.Helper()
	var revision uint64
	if err := b.f.admin.QueryRow(context.Background(), `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return RelayCommand{Auth: credential.Auth, RoomID: "binding-room", ClientID: id, BindingID: credential.BindingID, Generation: credential.Generation, ExpectedRevision: revision, Intent: intent}
}

func relayPGCommit(t *testing.T, b *bindingPGState, command RelayCommand) Result {
	t.Helper()
	result, err := b.f.store.CommitRelayOperation(context.Background(), command)
	if err != nil || result.Outcome != OutcomeCommitted || result.Relay == nil {
		t.Fatalf("Relay commit %s: %+v %v", command.Intent.Kind, result, err)
	}
	return result
}

func TestRelayBatchZeroFactsRecoverOriginalCursorAndDoNotInventHistory(t *testing.T) {
	b, _, member := relayPGPair(t, nil)
	command := relayPGCommand(t, b, member, "empty-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim})
	empty := relayPGCommit(t, b, command)
	if !empty.Relay.Empty || empty.Relay.Claim != nil || len(empty.Facts) != 0 || empty.Seq != 4 || empty.Revision != 5 {
		t.Fatalf("empty claim fabricated history: %+v", empty)
	}
	park := relayPGCommit(t, b, relayPGCommand(t, b, member, "same-park", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationPark, ParkEnabled: true}))
	if len(park.Facts) != 0 || park.Seq != 4 || park.Revision != 6 {
		t.Fatal("same policy fabricated an event")
	}
	replayed, err := b.f.store.ResolveRelayOperation(context.Background(), command)
	if err != nil || replayed.Outcome != OutcomeReplayed || replayed.Seq != empty.Seq || replayed.Revision != empty.Revision || !reflect.DeepEqual(replayed.Relay, empty.Relay) {
		t.Fatalf("no-fact receipt drifted: %+v %v", replayed, err)
	}
}

func TestRelayBatchReportSequenceRecoversBeforePeerReplacementCheck(t *testing.T) {
	b, owner, _ := relayPGPair(t, nil)
	command := relayPGCommand(t, b, owner, "transport-one", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationReport, ReportSeq: 1, Text: "@agent2 original publication", ExpectedPeerGeneration: 1})
	original := relayPGCommit(t, b, command)
	bindingPGComplete(t, b, bindingPGCommand(b, "replaced-peer", model.ActorSlot2, 1, true))
	command.ClientID = "new-transport-same-report"
	replayed, err := b.f.store.CommitRelayOperation(context.Background(), command)
	if err != nil || replayed.Outcome != OutcomeReplayed || !reflect.DeepEqual(replayed.Relay, original.Relay) || replayed.Revision != original.Revision || !reflect.DeepEqual(replayed.Facts, original.Facts) {
		t.Fatalf("accepted report retagged or blocked: %+v %v", replayed, err)
	}
	command.Intent.Text = "different payload"
	result, err := b.f.store.CommitRelayOperation(context.Background(), command)
	if kerr := kernelErr(t, result, err); kerr.Class != ClassPayloadConflict || result.Relay != nil || len(result.Facts) != 0 {
		t.Fatal("report payload conflict was bypassed by changed peer")
	}
	command = relayPGCommand(t, b, owner, "late-report", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationReport, ReportSeq: 2, Text: "@agent2 private late output", ExpectedPeerGeneration: 1})
	late := relayPGCommit(t, b, command)
	if late.Relay.Publication == nil || late.Relay.Publication.Outcome != relay.PeerChangedCode || late.Relay.Publication.Message != nil {
		t.Fatal("late output reached successor")
	}
	for _, fact := range late.Facts {
		if strings.Contains(string(fact.Data), "private late output") {
			t.Fatal("terminal receipt persisted private late body")
		}
	}
}

func TestRelayBatchAckIsPrivateProofAndAcceptedNoop(t *testing.T) {
	b, owner, member := relayPGPair(t, nil)
	sent := relayPGCommit(t, b, relayPGCommand(t, b, owner, "send", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "claimed once", ExpectedPeerGeneration: 1}))
	claim := relayPGCommit(t, b, relayPGCommand(t, b, member, "claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
	if claim.Relay.Claim.ID != sent.Relay.Message.ID {
		t.Fatal("FIFO claim selected wrong message")
	}
	ackCommand := relayPGCommand(t, b, member, "ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: claim.Relay.Claim.ID, Receipt: claim.Relay.Claim.Receipt})
	ack := relayPGCommit(t, b, ackCommand)
	secondCommand := relayPGCommand(t, b, member, "already-acknowledged", ackCommand.Intent)
	second := relayPGCommit(t, b, secondCommand)
	if !second.Relay.HandedOff || len(second.Facts) != 0 || second.Seq != ack.Seq {
		t.Fatal("settled acknowledgement created another transition")
	}
	ackCommand.Intent.Receipt = "different-private-proof"
	result, err := b.f.store.CommitRelayOperation(context.Background(), ackCommand)
	if kerr := kernelErr(t, result, err); kerr.Class != ClassPayloadConflict || result.Relay != nil {
		t.Fatal("receipt was excluded from operation identity")
	}
}

func TestRelayArchiveAllowsOnlyOriginalReceiptSettlement(t *testing.T) {
	b, owner, member := relayPGPair(t, nil)
	ctx := context.Background()
	relayPGCommit(t, b, relayPGCommand(t, b, owner, "before-archive", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "accepted before archive", ExpectedPeerGeneration: 1}))
	claim := relayPGCommit(t, b, relayPGCommand(t, b, member, "before-archive-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
	if _, err := b.f.admin.Exec(ctx, `UPDATE rooms SET archived_at=now() WHERE room_id='binding-room'`); err != nil {
		t.Fatal(err)
	}
	// Archiving stops new delivery, not the evidence that a prior envelope
	// reached stdout. The original live narrow credential and receipt are
	// still required; this path never unarchives or changes a Binding.
	for _, intent := range []relay.EnterpriseOperationIntent{{Kind: relay.EnterpriseOperationClaim}, {Kind: relay.EnterpriseOperationSend, Text: "new archived publication", ExpectedPeerGeneration: 1}} {
		command := relayPGCommand(t, b, member, "refused-"+string(intent.Kind), intent)
		result, err := b.f.store.CommitRelayOperation(ctx, command)
		if got := kernelErr(t, result, err); got.Class != ClassInvalidControlState || result.Relay != nil {
			t.Fatal("archive allowed new work")
		}
	}
	ack := relayPGCommit(t, b, relayPGCommand(t, b, member, "archive-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: claim.Relay.Claim.ID, Receipt: claim.Relay.Claim.Receipt}))
	if !ack.Relay.HandedOff {
		t.Fatal("original delivery could not settle")
	}
	var archived, active bool
	if err := b.f.admin.QueryRow(ctx, `SELECT archived_at IS NOT NULL,(SELECT retired_at IS NULL FROM bindings WHERE binding_id=$1) FROM rooms WHERE room_id='binding-room'`, member.BindingID).Scan(&archived, &active); err != nil || !archived || !active {
		t.Fatal("ack changed archive or reservation")
	}
}

// This fixture uses the real shared planner with an explicitly aged timestamp
// to exercise expiry without sleeping. It does not claim wall-clock/vendor
// acceptance; the tested operation and its rollback still run through SQL.
func seedAgedRelayClaim(t *testing.T, b *bindingPGState, member BindingCredential) {
	t.Helper()
	ctx := context.Background()
	snapshot, err := b.f.store.ReadonlySnapshot(ctx, member.Auth, "binding-room")
	if err != nil {
		t.Fatal(err)
	}
	prepare, err := relay.PrepareEnterpriseOperation(toRelaySnapshot(snapshot), relay.EnterpriseOperationCaller{Actor: model.ActorSlot2, UserID: member.Auth.UserID, BindingID: member.BindingID, Generation: member.Generation, ClientID: "aged-claim-fixture"}, relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}, relay.EnterpriseOperationIDs{Receipt: "aged-original-receipt"})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := prepare(time.Now().UTC().Add(-time.Minute))
	if err != nil || len(batch.Facts) != 1 {
		t.Fatalf("aged claim fixture: %v", err)
	}
	fact := batch.Facts[0]
	fact.ID, fact.Seq = "aged-claim-event", snapshot.Seq+1

	tx, err := b.f.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := b.f.store.writeStoredRoomEvent(ctx, tx, fact, userFactOrigin(member.Auth.UserID)); err != nil {
		t.Fatal(err)
	}

	if _, err := tx.Exec(ctx, `UPDATE rooms SET current_seq=$2,revision=$3 WHERE room_id=$1`, fact.RoomID, int64(fact.Seq), int64(snapshot.Revision+1)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRelayBatchLaterEventAndAuditFailuresRollBackWholeExpiryClaim(t *testing.T) {
	b, owner, member := relayPGPair(t, nil)
	first := relayPGCommit(t, b, relayPGCommand(t, b, owner, "first", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "old delivery", ExpectedPeerGeneration: 1}))
	seedAgedRelayClaim(t, b, member)
	second := relayPGCommit(t, b, relayPGCommand(t, b, owner, "second", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "next delivery", ExpectedPeerGeneration: 1}))
	command := relayPGCommand(t, b, member, "expiry-and-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim})
	ctx := context.Background()
	for _, failure := range []struct{ table, constraint, condition string }{
		{"room_events", "reject_later_batch_event", fmt.Sprintf("seq <= %d", second.Seq+1)},
		{"audit", "reject_batch_audit", "kind <> 'relay.claim'"},
	} {
		if _, err := b.f.admin.Exec(ctx, "ALTER TABLE "+failure.table+" ADD CONSTRAINT "+failure.constraint+" CHECK("+failure.condition+") NOT VALID"); err != nil {
			t.Fatal(err)
		}
		result, err := b.f.store.CommitRelayOperation(ctx, command)
		if kerr := kernelErr(t, result, err); kerr.Outcome != OutcomeNotCommitted || kerr.Class != ClassConstraintViolation {
			t.Fatalf("batch failure not atomic rollback: %+v %v", result, err)
		}
		var seq, revision uint64
		if err := b.f.admin.QueryRow(ctx, `SELECT current_seq,revision FROM rooms WHERE room_id='binding-room'`).Scan(&seq, &revision); err != nil || seq != second.Seq || revision != second.Revision {
			t.Fatal("partial batch advanced Room")
		}
		resolved, err := b.f.store.ResolveRelayOperation(ctx, command)
		if err != nil || resolved.Outcome != OutcomeNotCommitted || resolved.Relay != nil {
			t.Fatal("rolled-back batch retained receipt")
		}
		if _, err := b.f.admin.Exec(ctx, "ALTER TABLE "+failure.table+" DROP CONSTRAINT "+failure.constraint); err != nil {
			t.Fatal(err)
		}
	}
	result := relayPGCommit(t, b, command)
	if len(result.Facts) != 2 || result.Relay.Claim == nil || result.Relay.Claim.ID != second.Relay.Message.ID || result.Seq != second.Seq+2 || result.Revision != second.Revision+1 {
		t.Fatalf("expiry+claim was not one transaction: %+v", result)
	}
	snapshot, err := b.f.store.ReadonlySnapshot(ctx, b.owner.Auth, "binding-room")
	if err != nil {
		t.Fatal(err)
	}
	view, err := relay.ProjectEnterpriseSnapshot(toRelaySnapshot(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, m := range view.Messages {
		states[m.ID] = m.State
	}
	if states[first.Relay.Message.ID] != "unknown" || states[second.Relay.Message.ID] != "delivering" {
		t.Fatalf("wrong batch projection: %+v", states)
	}
}

func TestRelayBatchUnknownCommitResolvesAcrossOwnershipRestart(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			b, owner, _ := relayPGPair(t, func(dsn string) string { return proxy.dsn(dsn) })
			command := relayPGCommand(t, b, owner, "uncertain-send", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "original uncertain payload", ExpectedPeerGeneration: 1})
			proxy.arm()
			result, err := b.f.store.CommitRelayOperation(context.Background(), command)
			if kerr := kernelErr(t, result, err); kerr.Outcome != OutcomeUnknown {
				t.Fatalf("proxy did not interrupt COMMIT: %+v", result)
			}
			_ = b.f.store.Close()
			restarted, err := Open(context.Background(), b.f.cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Close()
			resolved, err := restarted.ResolveRelayOperation(context.Background(), command)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "drop-response" && (resolved.Outcome != OutcomeReplayed || resolved.Relay == nil || resolved.Relay.Message.Text != command.Intent.Text) {
				t.Fatalf("accepted result lost: %+v", resolved)
			}
			if mode == "break-on-commit" && (resolved.Outcome != OutcomeNotCommitted || resolved.Relay != nil || len(resolved.Facts) != 0) {
				t.Fatalf("absence not proven: %+v", resolved)
			}
			explicit, err := restarted.CommitRelayOperation(context.Background(), command)
			if err != nil || explicit.Relay == nil {
				t.Fatalf("explicit same-key recovery: %+v %v", explicit, err)
			}
			again, err := restarted.CommitRelayOperation(context.Background(), command)
			if err != nil || again.Outcome != OutcomeReplayed || !reflect.DeepEqual(again.Relay, explicit.Relay) {
				t.Fatal("same key repeated an uncertain effect")
			}
		})
	}
}
