package sqlstore

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

const recoveryBudget = 5 * time.Minute
const recoveryPageSize = 64
const recoveryPage = "page"
const recoveryRoom = "room"
const recoveryFinish = "finish"
const ClassStartupRecovery = "startup_recovery_required"

type RecoveryReport struct {
	Epoch    int64  `json:"epoch"`
	Rooms    uint64 `json:"rooms"`
	Reaped   uint64 `json:"reaped"`
	Complete bool   `json:"complete"`
}
type recoveryMutation struct {
	kind, after string
	epoch       int64
}
type recoveryDependencies struct {
	complete, roomMissing, marked bool
	roomID, orgID, hostMode       string
	seq, revision, reaped, rooms  uint64
}

// RecoverDeliveries is an operator-only startup barrier, never a public HTTP
// command. It must precede business mutations. Failure closes this Store's
// startup boundary; a fresh Open/epoch is required rather than hot repair.
func (s *Store) RecoverDeliveries(parent context.Context) (report RecoveryReport, err error) {
	ctx, cancel := context.WithTimeout(parent, recoveryBudget)
	defer cancel()
	_, release, e := s.beginOperation(ctx)
	if e != nil {
		return report, e
	}
	if s.closed || s.poisoned != nil || s.recoveryRunning.Load() || s.recoveryFailed.Load() || s.recoverySealed && !s.recoveryStarted {
		release()
		return report, refusedError(ClassStartupRecovery, nil)
	}
	s.recoverySealed, s.recoveryStarted = true, true
	s.recoveryRunning.Store(true)
	release()
	defer func() {
		if err != nil {
			s.recoveryFailed.Store(true)
		}
		s.recoveryRunning.Store(false)
	}()
	report.Epoch = s.lease.epoch
	after := ""
	for {
		p := opPlan{recovery: &recoveryMutation{kind: recoveryPage, after: after, epoch: s.lease.epoch}}
		result, e := s.resolveWithFallback(ctx, p)
		if e != nil {
			return report, e
		}
		if result.Recovery == nil {
			return report, refusedError(ClassStateInvalid, nil)
		}
		if result.Recovery.Complete {
			s.recoveryCompleted.Store(true)
			return *result.Recovery, nil
		}
		if s.recoveryCompleted.Load() {
			return report, refusedError(ClassStoredFactUnreadable, errors.New("completed recovery marker is missing"))
		}
		if len(result.recoveryRooms) == 0 {
			break
		}
		for _, roomID := range result.recoveryRooms {
			p := opPlan{key: OpKey{RoomID: roomID}, recovery: &recoveryMutation{kind: recoveryRoom, epoch: s.lease.epoch}}
			result, e := s.commitWithRestarts(ctx, p, model.Event{})
			if e != nil {
				return report, e
			}
			if result.Recovery == nil {
				return report, refusedError(ClassStateInvalid, nil)
			}
			report.Rooms++
			report.Reaped += result.Recovery.Reaped
			after = roomID
		}
	}
	result, e := s.commitWithRestarts(ctx, opPlan{recovery: &recoveryMutation{kind: recoveryFinish, epoch: s.lease.epoch}}, model.Event{})
	if e != nil {
		return report, e
	}
	if result.Recovery == nil || !result.Recovery.Complete {
		return report, refusedError(ClassStateInvalid, nil)
	}
	s.recoveryCompleted.Store(true)
	return *result.Recovery, nil
}

func (s *Store) readRecoveryDependencies(ctx context.Context, q rowQuerier, plan opPlan, lock string) (*recoveryDependencies, error) {
	d := &recoveryDependencies{roomID: plan.key.RoomID}
	if plan.recovery.epoch != s.lease.epoch {
		return nil, errLeaseLost
	}
	e := q.QueryRow(ctx, `SELECT room_count,reaped_count FROM `+s.table("startup_recovery_runs")+` WHERE ownership_epoch=$1`, plan.recovery.epoch).Scan(&d.rooms, &d.reaped)
	if e == nil {
		d.complete = true
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return nil, e
	}
	if plan.recovery.kind != recoveryRoom {
		return d, nil
	}
	roomLock := lockNone
	if lock != "" {
		roomLock = lockExclusive
	}
	e = q.QueryRow(ctx, `SELECT org_id,host_mode,current_seq,revision FROM `+s.table("rooms")+` WHERE room_id=$1`+roomLock, plan.key.RoomID).Scan(&d.orgID, &d.hostMode, &d.seq, &d.revision)
	if errors.Is(e, pgx.ErrNoRows) {
		d.roomMissing = true
	} else if e != nil {
		return nil, e
	}
	e = q.QueryRow(ctx, `SELECT reaped_count FROM `+s.table("startup_room_recoveries")+` WHERE ownership_epoch=$1 AND room_id=$2`, plan.recovery.epoch, plan.key.RoomID).Scan(&d.reaped)
	if e == nil {
		d.marked = true
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return nil, e
	}
	return d, nil
}
func (s *Store) resolveRecoveryPlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *recoveryDependencies) (Result, error) {
	if plan.recovery.kind != recoveryPage {
		return refused(ClassUnsupportedOp, nil)
	}
	report := &RecoveryReport{Epoch: plan.recovery.epoch, Rooms: d.rooms, Reaped: d.reaped, Complete: d.complete}
	if d.complete {
		return Result{Outcome: OutcomeReplayed, Recovery: report}, nil
	}
	var rooms []string
	e := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(room_id ORDER BY room_id COLLATE "C"),ARRAY[]::text[]) FROM (SELECT room_id FROM `+s.table("rooms")+` WHERE room_id COLLATE "C">$1 ORDER BY room_id COLLATE "C" LIMIT $2) page`, plan.recovery.after, recoveryPageSize).Scan(&rooms)
	if e != nil {
		return resolveIncomplete(ctx, e)
	}
	return Result{Outcome: OutcomeReplayed, Recovery: report, recoveryRooms: rooms}, nil
}
func (s *Store) applyRecoveryPlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *recoveryDependencies) (Result, error) {
	if !s.recoveryRunning.Load() || plan.recovery.epoch != s.lease.epoch {
		return refused(ClassStartupRecovery, nil)
	}
	report := &RecoveryReport{Epoch: plan.recovery.epoch, Rooms: d.rooms, Reaped: d.reaped, Complete: d.complete}
	if d.complete {
		return Result{Outcome: OutcomeReplayed, Recovery: report}, nil
	}
	now := time.Now().UTC()
	if plan.recovery.kind == recoveryFinish {
		var missing bool
		var rooms, reaped uint64
		e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("rooms")+` r WHERE NOT EXISTS(SELECT 1 FROM `+s.table("startup_room_recoveries")+` m WHERE m.ownership_epoch=$1 AND m.room_id=r.room_id)),(SELECT count(*) FROM `+s.table("startup_room_recoveries")+` WHERE ownership_epoch=$1),(SELECT COALESCE(sum(reaped_count),0) FROM `+s.table("startup_room_recoveries")+` WHERE ownership_epoch=$1)`, plan.recovery.epoch).Scan(&missing, &rooms, &reaped)
		if e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		if missing {
			return refused(ClassStateInvalid, nil)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO `+s.table("startup_recovery_runs")+`(ownership_epoch,room_count,reaped_count,completed_at) VALUES($1,$2,$3,$4)`, plan.recovery.epoch, int64(rooms), int64(reaped), now); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		if e = s.appendRecoveryAudit(ctx, tx, "", "delivery.recovery.complete", now); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		return Result{Outcome: OutcomeCommitted, Recovery: &RecoveryReport{Epoch: plan.recovery.epoch, Rooms: rooms, Reaped: reaped, Complete: true}}, nil
	}
	if plan.recovery.kind != recoveryRoom || d.roomMissing {
		return refused(ClassStateInvalid, nil)
	}
	if d.marked {
		return Result{Outcome: OutcomeReplayed, Recovery: &RecoveryReport{Epoch: plan.recovery.epoch, Rooms: 1, Reaped: d.reaped}}, nil
	}
	snapshot, e := s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: plan.key.RoomID, DeploymentID: s.cfg.DeploymentID, OrgID: d.orgID, HostMode: d.hostMode, Seq: d.seq, Revision: d.revision})
	if e != nil {
		return recoveryFailure(s, ctx, e)
	}
	prepare, e := relay.PrepareEnterpriseRecovery(toRelaySnapshot(snapshot))
	if e != nil {
		return refused(ClassStoredFactUnreadable, e)
	}
	facts, e := prepare(now)
	if e != nil {
		return refused(ClassStoredFactUnreadable, e)
	}
	if len(facts) > maxRelayBatchFacts || uint64(len(facts)) > math.MaxInt64-d.seq || len(facts) > 0 && d.revision >= math.MaxInt64 {
		return refused(ClassRangeExceeded, nil)
	}
	for i := range facts {
		fact := &facts[i]
		fact.Seq = d.seq + uint64(i) + 1
		fact.ID, e = controlID("recovery")
		if e != nil {
			return refused(ClassStateInvalid, e)
		}
		if e = s.writeStoredRoomEvent(ctx, tx, *fact, operatorFactOrigin()); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
	}
	seq, revision := d.seq+uint64(len(facts)), d.revision
	if len(facts) > 0 {
		revision++
		if _, e = tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET current_seq=$2,revision=$3 WHERE room_id=$1`, plan.key.RoomID, int64(seq), int64(revision)); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
	}
	if _, e = tx.Exec(ctx, `INSERT INTO `+s.table("startup_room_recoveries")+`(ownership_epoch,room_id,result_seq,result_revision,reaped_count,recovered_at) VALUES($1,$2,$3,$4,$5,$6)`, plan.recovery.epoch, plan.key.RoomID, int64(seq), int64(revision), len(facts), now); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	if e = s.appendRecoveryAudit(ctx, tx, plan.key.RoomID, "delivery.recovery.room", now); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	return Result{Outcome: OutcomeCommitted, Recovery: &RecoveryReport{Epoch: plan.recovery.epoch, Rooms: 1, Reaped: uint64(len(facts))}}, nil
}
func recoveryFailure(s *Store, ctx context.Context, e error) (Result, error) {
	var k *KernelError
	if errors.As(e, &k) && k.Outcome == OutcomeRefused {
		return refused(k.Class, k.Err)
	}
	return controlStatementFailure(s, ctx, e)
}
func (s *Store) appendRecoveryAudit(ctx context.Context, tx pgx.Tx, roomID, kind string, at time.Time) error {
	var room any
	if roomID != "" {
		room = roomID
	}
	_, e := tx.Exec(ctx, `INSERT INTO `+s.table("audit")+`(deployment_id,room_id,user_id,actor_kind,kind,outcome,at) VALUES($1,$2,NULL,'operator',$3,'accepted',$4)`, s.cfg.DeploymentID, room, kind, at)
	return e
}
