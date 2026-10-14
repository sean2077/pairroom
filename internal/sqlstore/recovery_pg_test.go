package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func recoveryPGPair(t *testing.T) (*bindingPGState, BindingCredential, BindingCredential) {
	t.Helper()
	b, first, second := relayPGPair(t, nil)
	// The low-level fixture deliberately has an empty primitive Room. This
	// recovery-specific seed gives it a strict real creation fact, rather than
	// weakening startup replay to accept missing enterprise origins.
	intent, e := model.PrepareEnterpriseRoomIntent("Primitive fixture domain origin", model.Collaboration{})
	if e != nil {
		t.Fatal(e)
	}
	fact, e := model.NewEnterpriseRoomCreation("room-1", "dep-test", "org-test", "u-owner", intent, time.Now().UTC())
	if e != nil {
		t.Fatal(e)
	}
	tx, e := b.f.admin.Begin(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(context.Background())
	if e = b.f.store.writeStoredRoomEvent(context.Background(), tx, fact, userFactOrigin("u-owner")); e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(context.Background(), `UPDATE rooms SET current_seq=1,revision=1 WHERE room_id='room-1'`); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(context.Background()); e != nil {
		t.Fatal(e)
	}
	return b, first, second
}
func recoveryRestart(t *testing.T, b *bindingPGState) {
	t.Helper()
	if e := b.f.store.Close(); e != nil {
		t.Fatal(e)
	}
	s, e := Open(context.Background(), b.f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	b.f.store = s
}
func recoveryMessages(t *testing.T, b *bindingPGState) map[string]relay.Message {
	t.Helper()
	snapshot, e := b.f.store.ReadonlySnapshot(context.Background(), b.owner.Auth, "binding-room")
	if e != nil {
		t.Fatal(e)
	}
	view, e := relay.ProjectEnterpriseSnapshot(toRelaySnapshot(snapshot))
	if e != nil {
		t.Fatal(e)
	}
	out := map[string]relay.Message{}
	for _, m := range view.Messages {
		out[m.ID] = m
	}
	return out
}
func recoveryCounts(t *testing.T, b *bindingPGState) [5]int64 {
	t.Helper()
	var result [5]int64
	e := b.f.admin.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM room_events),(SELECT count(*) FROM startup_room_recoveries),(SELECT count(*) FROM startup_recovery_runs),(SELECT count(*) FROM audit),revision FROM rooms WHERE room_id='binding-room'`).Scan(&result[0], &result[1], &result[2], &result[3], &result[4])
	if e != nil {
		t.Fatal(e)
	}
	return result
}

func TestRecoveryNewEpochReapsOnceAndPreservesLateAcknowledgement(t *testing.T) {
	b, owner, member := recoveryPGPair(t)
	ctx := context.Background()
	sent := relayPGCommit(t, b, relayPGCommand(t, b, owner, "first-send", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "old delivering", ExpectedPeerGeneration: 1}))
	claim := relayPGCommit(t, b, relayPGCommand(t, b, member, "first-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
	queued := relayPGCommit(t, b, relayPGCommand(t, b, owner, "queued-send", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "queued remains", ExpectedPeerGeneration: 1}))
	before := recoveryCounts(t, b)
	if state := recoveryMessages(t, b)[sent.Relay.Message.ID].State; state != "delivering" {
		t.Fatal(state)
	}
	if recoveryCounts(t, b) != before {
		t.Fatal("same-epoch snapshot wrote or reaped")
	}
	if _, e := b.f.store.RecoverDeliveries(ctx); e == nil || recoveryCounts(t, b) != before {
		t.Fatal("late first startup call reaped current-epoch work")
	}
	recoveryRestart(t, b)
	report, e := b.f.store.RecoverDeliveries(ctx)
	if e != nil || !report.Complete || report.Rooms != 2 || report.Reaped != 1 {
		t.Fatalf("startup recovery: %+v %v", report, e)
	}
	states := recoveryMessages(t, b)
	if states[sent.Relay.Message.ID].State != "unknown" || states[queued.Relay.Message.ID].State != "queued" {
		t.Fatalf("recovery changed unrelated work: %+v", states)
	}
	var origin string
	var user *string
	var encoding []byte
	if e = b.f.admin.QueryRow(ctx, `SELECT origin_kind,origin_user,event_encoding FROM room_events WHERE room_id='binding-room' ORDER BY seq DESC LIMIT 1`).Scan(&origin, &user, &encoding); e != nil {
		t.Fatal(e)
	}
	var record storedEventRecord
	if e = json.Unmarshal(encoding, &record); e != nil {
		t.Fatal(e)
	}
	var payload struct {
		Receipt                          string `json:"receipt"`
		FromGeneration, TargetGeneration uint64
	}
	if e = json.Unmarshal(record.Event.Data, &payload); e != nil {
		t.Fatal(e)
	}
	if origin != "operator" || user != nil || record.CodecVersion != 2 || record.OriginKind != "operator" || record.OriginUser != nil || record.Event.Actor != model.ActorSystem || payload.Receipt != claim.Relay.Claim.Receipt {
		t.Fatal("recovery invented User provenance or lost private receipt")
	}
	ack := relayPGCommit(t, b, relayPGCommand(t, b, member, "late-original-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: claim.Relay.Claim.ID, Receipt: claim.Relay.Claim.Receipt}))
	if !ack.Relay.HandedOff {
		t.Fatal("original receipt cannot settle recovered unknown")
	}
	fresh := relayPGCommit(t, b, relayPGCommand(t, b, member, "new-current-epoch-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
	if fresh.Relay.Claim.ID != queued.Relay.Message.ID {
		t.Fatal("queued work changed")
	}
	before = recoveryCounts(t, b)
	again, e := b.f.store.RecoverDeliveries(ctx)
	if e != nil || again != report || recoveryCounts(t, b) != before || recoveryMessages(t, b)[queued.Relay.Message.ID].State != "delivering" {
		t.Fatalf("repeated startup reaped current epoch: %+v %v", again, e)
	}
}

func TestRecoveryEventAndAuditFailureRollBackRoomAndMarker(t *testing.T) {
	for _, failure := range []string{"event", "audit"} {
		t.Run(failure, func(t *testing.T) {
			b, owner, member := recoveryPGPair(t)
			ctx := context.Background()
			sent := relayPGCommit(t, b, relayPGCommand(t, b, owner, "send", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "must stay delivering", ExpectedPeerGeneration: 1}))
			relayPGCommit(t, b, relayPGCommand(t, b, member, "claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
			recoveryRestart(t, b)
			if failure == "event" {
				mustExec(t, b.f.admin, `ALTER TABLE room_events ADD CONSTRAINT test_no_operator_event CHECK(origin_kind<>'operator') NOT VALID`)
			} else {
				mustExec(t, b.f.admin, `ALTER TABLE audit ADD CONSTRAINT test_no_recovery_audit CHECK(kind<>'delivery.recovery.room') NOT VALID`)
			}
			before := recoveryCounts(t, b)
			_, e := b.f.store.RecoverDeliveries(ctx)
			var k *KernelError
			if !errors.As(e, &k) || k.Outcome != OutcomeNotCommitted || k.Class != ClassConstraintViolation || recoveryCounts(t, b) != before || recoveryMessages(t, b)[sent.Relay.Message.ID].State != "delivering" {
				t.Fatalf("startup failure left partial Room effect: %v", e)
			}
			r, e := b.f.store.CommitRelayOperation(ctx, relayPGCommand(t, b, owner, "forbidden-after-failure", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "must not run", ExpectedPeerGeneration: 1}))
			if e == nil || r.Class != ClassStartupRecovery {
				t.Fatal("failed startup restored its writer")
			}
		})
	}
}
func TestRecoveryCorruptRoomRefusesStartupAndNextEpochContinuesWithoutDuplicateFact(t *testing.T) {
	b, owner, member := recoveryPGPair(t)
	ctx := context.Background()
	sent := relayPGCommit(t, b, relayPGCommand(t, b, owner, "send", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "recover once", ExpectedPeerGeneration: 1}))
	relayPGCommit(t, b, relayPGCommand(t, b, member, "claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
	intent, _ := model.PrepareEnterpriseRoomIntent("Corrupt later page", model.Collaboration{})
	if _, e := b.f.store.CommitRoomCreate(ctx, RoomCreateCommand{Auth: b.owner.Auth, RoomID: "zz-corrupt", ClientID: "create-corrupt", Intent: intent}); e != nil {
		t.Fatal(e)
	}
	var original []byte
	if e := b.f.admin.QueryRow(ctx, `SELECT event_encoding FROM room_events WHERE room_id='zz-corrupt' AND seq=1`).Scan(&original); e != nil {
		t.Fatal(e)
	}
	mustExec(t, b.f.admin, `UPDATE room_events SET event_encoding='corrupt' WHERE room_id='zz-corrupt' AND seq=1`)
	recoveryRestart(t, b)
	if _, e := b.f.store.RecoverDeliveries(ctx); e == nil {
		t.Fatal("corrupt Room allowed startup")
	}
	if recoveryMessages(t, b)[sent.Relay.Message.ID].State != "unknown" {
		t.Fatal("earlier committed Room recovery missing")
	}
	before := recoveryCounts(t, b)
	if _, e := b.f.store.RecoverDeliveries(ctx); e == nil || recoveryCounts(t, b) != before {
		t.Fatal("failed epoch retried effects")
	}
	if _, e := b.f.admin.Exec(ctx, `UPDATE room_events SET event_encoding=$1 WHERE room_id='zz-corrupt' AND seq=1`, original); e != nil {
		t.Fatal(e)
	}
	recoveryRestart(t, b)
	report, e := b.f.store.RecoverDeliveries(ctx)
	if e != nil || !report.Complete || report.Reaped != 0 || report.Rooms != 3 {
		t.Fatalf("safe next epoch continuation: %+v %v", report, e)
	}
	after := recoveryCounts(t, b)
	if after[0] != before[0] {
		t.Fatal("next epoch repeated recovered unknown fact")
	}
}

func TestRecoveryArchivedRoomChangesOnlyDeliveringState(t *testing.T) {
	b, owner, member := recoveryPGPair(t)
	ctx := context.Background()
	handed := relayPGCommit(t, b, relayPGCommand(t, b, owner, "handed-send", recoverySendIntent("already handed off")))
	first := relayPGCommit(t, b, relayPGCommand(t, b, member, "handed-claim", recoveryClaimIntent()))
	relayPGCommit(t, b, relayPGCommand(t, b, member, "handed-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: first.Relay.Claim.ID, Receipt: first.Relay.Claim.Receipt}))
	unknown := relayPGCommit(t, b, relayPGCommand(t, b, owner, "unknown-send", recoverySendIntent("already unknown")))
	seedAgedRelayClaim(t, b, member)
	delivering := relayPGCommit(t, b, relayPGCommand(t, b, owner, "delivering-send", recoverySendIntent("still delivering")))
	active := relayPGCommit(t, b, relayPGCommand(t, b, member, "delivering-claim", recoveryClaimIntent()))
	queued := relayPGCommit(t, b, relayPGCommand(t, b, owner, "queued-send", recoverySendIntent("still queued")))
	before := recoveryMessages(t, b)
	var operatorExpiry int
	if e := b.f.admin.QueryRow(ctx, `SELECT count(*) FROM room_events WHERE room_id='binding-room' AND actor='system' AND kind='native.message.updated' AND origin_kind='operator' AND origin_user IS NULL`).Scan(&operatorExpiry); e != nil || operatorExpiry != 1 {
		t.Fatalf("automatic expiry copied User provenance: %d %v", operatorExpiry, e)
	}
	var operatorAudit int
	if e := b.f.admin.QueryRow(ctx, `SELECT count(*) FROM audit WHERE room_id='binding-room' AND kind='relay.expiry' AND actor_kind='operator' AND user_id IS NULL`).Scan(&operatorAudit); e != nil || operatorAudit != 1 {
		t.Fatal("automatic expiry missing operator audit")
	}
	mustExec(t, b.f.admin, `UPDATE rooms SET archived_at=now() WHERE room_id='binding-room'`)
	recoveryRestart(t, b)
	report, e := b.f.store.RecoverDeliveries(ctx)
	if e != nil || report.Reaped != 1 {
		t.Fatalf("archived recovery: %+v %v", report, e)
	}
	after := recoveryMessages(t, b)
	for _, id := range []string{handed.Relay.Message.ID, unknown.Relay.Message.ID, queued.Relay.Message.ID} {
		if !reflect.DeepEqual(before[id], after[id]) {
			t.Fatalf("recovery altered non-delivering message %s", id)
		}
	}
	if after[delivering.Relay.Message.ID].State != "unknown" || after[delivering.Relay.Message.ID].FromGeneration != before[delivering.Relay.Message.ID].FromGeneration || after[delivering.Relay.Message.ID].TargetGeneration != before[delivering.Relay.Message.ID].TargetGeneration {
		t.Fatal("recovery altered generations or did not reap delivering")
	}
	ack := relayPGCommit(t, b, relayPGCommand(t, b, member, "archived-late-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: active.Relay.Claim.ID, Receipt: active.Relay.Claim.Receipt}))
	if !ack.Relay.HandedOff {
		t.Fatal("archive prevented original receipt settlement")
	}
	r, e := b.f.store.CommitRelayOperation(ctx, relayPGCommand(t, b, member, "archive-new-claim", recoveryClaimIntent()))
	if e == nil || r.Class != ClassInvalidControlState || r.Relay != nil {
		t.Fatal("archive released new work")
	}
}
