package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGovernanceFinishReplayRejectsRehashedChangedOriginalReceipt(t *testing.T) {
	for _, field := range []string{"reason", "target", "revision", "expiry", "version"} {
		t.Run(field, func(t *testing.T) {
			g := newGovernanceFixture(t, true)
			ctx := t.Context()
			_, completion := prepareGovernance(t, g, "finish-integrity")
			action, err := g.f.store.FinishGovernanceReauthentication(ctx, g.admin.Auth, completion)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = g.f.store.TransferRoomOwnership(ctx, governanceTransferCommand(g, action, "consume-before-damage")); err != nil {
				t.Fatal(err)
			}
			var encoding []byte
			if err = g.f.admin.QueryRow(ctx, `SELECT result_encoding FROM governance_operations WHERE op_kind='finish' AND identifier=$1`, action.ID).Scan(&encoding); err != nil {
				t.Fatal(err)
			}
			var receipt GovernanceReceipt
			if err = json.Unmarshal(encoding, &receipt); err != nil {
				t.Fatal(err)
			}
			switch field {
			case "reason":
				receipt.Action.Intent.Reason = "another valid private reason"
			case "target":
				receipt.Action.Intent.TargetUserID = "different-valid-target"
			case "revision":
				receipt.Action.Intent.ExpectedRevision++
			case "expiry":
				receipt.Action.ExpiresAt = receipt.Action.ExpiresAt.Add(time.Minute)
			case "version":
				receipt.Version++
			}
			encoding, err = json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(encoding)
			// Production triggers are the first integrity layer. This test bypasses
			// that layer to prove immutable-action comparison still detects a corrupt
			// receipt, including restore INSERTs which do not run UPDATE triggers.
			if _, err = g.f.admin.Exec(ctx, `ALTER TABLE governance_operations DISABLE TRIGGER governance_operations_immutable`); err != nil {
				t.Fatal(err)
			}
			if _, err = g.f.admin.Exec(ctx, `UPDATE governance_operations SET result_encoding=$2,result_hash=$3 WHERE op_kind='finish' AND identifier=$1`, action.ID, encoding, digest[:]); err != nil {
				t.Fatal(err)
			}
			var reason string
			if err = g.f.admin.QueryRow(ctx, `SELECT reason FROM governance_actions WHERE action_id=$1`, action.ID).Scan(&reason); err != nil || reason != g.intent.Reason {
				t.Fatal("counterexample changed authoritative action", err)
			}
			replayed, err := g.f.store.FinishGovernanceReauthentication(ctx, g.admin.Auth, completion)
			if err == nil || replayed != (GovernanceAction{}) {
				t.Fatalf("damaged %s escaped accepted replay: %+v %v", field, replayed, err)
			}
			var kernel *KernelError
			if !errors.As(err, &kernel) || kernel.Class != ClassStoredFactUnreadable {
				t.Fatalf("damaged receipt misclassified as live eligibility: %v", err)
			}
		})
	}
}

func TestGovernanceReasonExactPersistenceReplayAuditAndPrivacy(t *testing.T) {
	g := newGovernanceFixture(t, true)
	g.intent.Reason = " original\n恢复 disabled Owner\t "
	ctx := t.Context()
	prepared, completion := prepareGovernance(t, g, "private-reason")
	action, err := g.f.store.FinishGovernanceReauthentication(ctx, g.admin.Auth, completion)
	if err != nil || !action.Ready || action.ID != prepared.ID || action.Intent != g.intent {
		t.Fatal("finish did not preserve original action intent", err)
	}
	changedPrepare := g.intent
	changedPrepare.Reason = "different original preparation"
	if _, err := g.f.store.PrepareGovernanceReauthentication(ctx, g.admin.Auth, "private-reason", changedPrepare); err == nil {
		t.Fatal("prepare replay changed original reason")
	}
	for i, reason := range []string{"", " \t\n\u00a0\u3000", strings.Repeat("界", 342), "private\x1b", "private\u0085"} {
		if _, err := g.f.admin.Exec(ctx, `INSERT INTO governance_actions(action_id,actor_user_id,session_id,room_id,target_user_id,expected_owner_user_id,expected_revision,reason,created_at,expires_at) SELECT action_id||$2,actor_user_id,session_id,room_id,target_user_id,expected_owner_user_id,expected_revision,$3,created_at,expires_at FROM governance_actions WHERE action_id=$1`, action.ID, string(rune('a'+i)), reason); err == nil {
			t.Fatal("SQL accepted invalid original rationale")
		}
	}
	listed, err := g.f.store.ListGovernanceActions(ctx, g.admin.Auth)
	if err != nil || len(listed) != 1 || listed[0].Intent != g.intent {
		t.Fatal("list lost original private intent", err)
	}
	read, err := g.f.store.ReadGovernanceAction(ctx, g.admin.Auth, action.ID)
	if err != nil || read.Intent != g.intent {
		t.Fatal("read changed original private intent", err)
	}
	if _, err := g.f.admin.Exec(ctx, `UPDATE governance_actions SET reason='changed' WHERE action_id=$1`, action.ID); err == nil {
		t.Fatal("stored reason was mutable")
	}
	if _, err := g.f.admin.Exec(ctx, `UPDATE governance_actions SET action_id='different-reference' WHERE action_id=$1`, action.ID); err == nil {
		t.Fatal("action reference was replaceable")
	}
	cmd := governanceTransferCommand(g, action, "original-transfer")
	changed := cmd
	changed.Intent.Reason = "different valid reason"
	if _, err := g.f.store.TransferRoomOwnership(ctx, changed); err == nil {
		t.Fatal("fresh proof accepted changed reason")
	}
	r, err := g.f.store.TransferRoomOwnership(ctx, cmd)
	if err != nil || r.Outcome != OutcomeCommitted {
		t.Fatal(r.Outcome, err)
	}
	finished, err := g.f.store.FinishGovernanceReauthentication(ctx, g.admin.Auth, completion)
	if err != nil || finished != action {
		t.Fatal("accepted finish replay lost immutable original after consumption", err)
	}
	changedAction := cmd
	changedAction.GovernanceActionID = "different-reference"
	if _, err := g.f.store.ResolveRoomOwnership(ctx, changedAction); err == nil {
		t.Fatal("Resolve replaced original action reference")
	}
	for _, resolve := range []bool{false, true} {
		var got Result
		if resolve {
			got, err = g.f.store.ResolveRoomOwnership(ctx, cmd)
		} else {
			got, err = g.f.store.TransferRoomOwnership(ctx, cmd)
		}
		if err != nil || got.Outcome != OutcomeReplayed || got.Seq != r.Seq {
			t.Fatal("original accepted result not restored", err)
		}
		if resolve {
			_, err = g.f.store.ResolveRoomOwnership(ctx, changed)
		} else {
			_, err = g.f.store.TransferRoomOwnership(ctx, changed)
		}
		if err == nil {
			t.Fatal("accepted replay changed original reason")
		}
	}
	var count int
	if err := g.f.admin.QueryRow(ctx, `SELECT count(*) FROM audit WHERE kind IN ('governance.prepare','governance.finish','governance.transfer') AND detail=$1 AND operation_id IS NOT NULL AND target_id=$2`, g.intent.Reason, g.intent.RoomID).Scan(&count); err != nil || count != 3 {
		t.Fatal("same-TX private audits lost reason or typed fields", count, err)
	}
	snapshot, err := g.f.store.ReadonlySnapshot(ctx, g.target.Auth, g.intent.RoomID)
	data, _ := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(data), "恢复") || strings.Contains(string(r.Fact.Data), "reason") {
		t.Fatal("private reason leaked into Room facts", err)
	}
	if _, err := g.f.store.ReadGovernanceAction(ctx, g.target.Auth, action.ID); err == nil {
		t.Fatal("ordinary member read private action")
	}
	if _, err := g.f.store.ReadonlySnapshot(ctx, g.admin.Auth, g.intent.RoomID); err == nil {
		t.Fatal("reason granted admin Room content")
	}
}

func TestGovernanceReasonDamagedStoredIntentFailsClosed(t *testing.T) {
	g := newGovernanceFixture(t, true)
	action := readyGovernance(t, g, "damage-proof")
	ctx := t.Context()
	if _, err := g.f.admin.Exec(ctx, `ALTER TABLE governance_actions DROP CONSTRAINT governance_reason_valid; ALTER TABLE governance_actions DISABLE TRIGGER governance_actions_identity_immutable; UPDATE governance_actions SET reason=''`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.f.store.ReadGovernanceAction(ctx, g.admin.Auth, action.ID); err == nil {
		t.Fatal("damaged private intent was disclosed")
	}
	if _, err := g.f.store.ListGovernanceActions(ctx, g.admin.Auth); err == nil {
		t.Fatal("damaged private intent was listed")
	}
	if _, err := g.f.store.TransferRoomOwnership(ctx, governanceTransferCommand(g, action, "damaged-transfer")); err == nil {
		t.Fatal("damaged private intent authorized transfer")
	}
	var owner string
	var consumed bool
	if err := g.f.admin.QueryRow(ctx, `SELECT owner_user_id,(SELECT consumed_at IS NOT NULL FROM governance_actions WHERE action_id=$2) FROM rooms WHERE room_id=$1`, g.intent.RoomID, action.ID).Scan(&owner, &consumed); err != nil || owner != g.owner.Auth.UserID || consumed {
		t.Fatal("damaged intent left effect", err)
	}
}

func TestGovernanceReasonSchemaRejectsOldAlphaWithoutMigration(t *testing.T) {
	g := newGovernanceFixture(t, true)
	ctx := t.Context()
	if _, err := g.f.admin.Exec(ctx, `ALTER TABLE governance_actions DROP COLUMN reason`); err != nil {
		t.Fatal(err)
	}
	if err := ApplySchema(ctx, g.f.admin, g.f.schema); err == nil || !strings.Contains(err.Error(), "governance reason schema shape") {
		t.Fatal("accepted or migrated reason-less alpha", err)
	}
	var exists bool
	if err := g.f.admin.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='governance_actions' AND column_name='reason')`, g.f.schema).Scan(&exists); err != nil || exists {
		t.Fatal("recreated old alpha column", err)
	}
}

func TestGovernanceEnableTransferBothSerialOrdersUseRealLockBarrier(t *testing.T) {
	for _, enableFirst := range []bool{true, false} {
		name := "transfer-first"
		if enableFirst {
			name = "enable-first"
		}
		t.Run(name, func(t *testing.T) {
			g := newGovernanceFixture(t, true)
			action := readyGovernance(t, g, "prepared-before-race")
			ctx := t.Context()
			work := schemaConn(t, g.f.dsn, g.f.schema)
			if err := configureSession(ctx, work, g.f.schema); err != nil {
				t.Fatal(err)
			}
			other := &Store{cfg: g.f.cfg, lease: g.f.store.lease, work: work, sem: make(chan struct{}, 1)}
			blocker := schemaConn(t, g.f.dsn, g.f.schema)
			tx, err := blocker.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err = tx.Exec(ctx, `SELECT epoch FROM ownership WHERE id FOR SHARE`); err != nil {
				t.Fatal(err)
			}
			type outcome struct {
				result Result
				err    error
			}
			firstDone, secondDone := make(chan outcome, 1), make(chan outcome, 1)
			run := func(store *Store, enable bool, done chan outcome) {
				var r Result
				var e error
				if enable {
					r, e = store.SetUserEnabled(ctx, g.admin.Auth, "enable-racing-owner", g.owner.Auth.UserID, true)
				} else {
					r, e = store.TransferRoomOwnership(ctx, governanceTransferCommand(g, action, "transfer-racing-enable"))
				}
				done <- outcome{r, e}
			}
			go run(g.f.store, enableFirst, firstDone)
			waitIdentityControlGuardChain(t, g.f, []uint32{g.f.store.work.PgConn().PID()}, blocker.PgConn().PID())
			go run(other, !enableFirst, secondDone)
			waitIdentityControlGuardChain(t, g.f, []uint32{other.work.PgConn().PID()}, g.f.store.work.PgConn().PID())
			if err = tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			first, second := <-firstDone, <-secondDone
			if first.err != nil || first.result.Outcome != OutcomeCommitted {
				t.Fatal("first lawful serialization failed", first.err)
			}
			if enableFirst {
				assertGovernanceDenied(t, second.err)
			} else if second.err != nil || second.result.Outcome != OutcomeCommitted {
				t.Fatal("re-enable after accepted transfer failed", second.err)
			}
			var owner string
			var disabled, consumed bool
			var transfers int
			if err = g.f.admin.QueryRow(ctx, `SELECT owner_user_id,(SELECT disabled FROM users WHERE user_id=$2),(SELECT consumed_at IS NOT NULL FROM governance_actions WHERE action_id=$3),(SELECT count(*) FROM audit WHERE kind='governance.transfer') FROM rooms WHERE room_id=$1`, g.intent.RoomID, g.owner.Auth.UserID, action.ID).Scan(&owner, &disabled, &consumed, &transfers); err != nil {
				t.Fatal(err)
			}
			wantOwner, wantCount := g.owner.Auth.UserID, 0
			if !enableFirst {
				wantOwner, wantCount = g.target.Auth.UserID, 1
			}
			if owner != wantOwner || disabled || consumed != !enableFirst || transfers != wantCount {
				t.Fatal("illegal mixed enable/transfer result", owner, disabled, consumed, transfers)
			}
		})
	}
}
