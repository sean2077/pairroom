package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"time"
)

type mutationAuditState struct{ invalidatedApprovals *int64 }
type mutationAuditContextKey struct{}

func contextAuditPlan(ctx context.Context) opPlan {
	key, _ := ctx.Value(mutationAuditContextKey{}).(OpKey)
	return opPlan{key: key}
}

func setAuditInvalidations(plan opPlan, count int64) {
	if plan.auditState != nil {
		plan.auditState.invalidatedApprovals = &count
	}
}

func mutationOperationID(key OpKey) string {
	// The namespace/principal/full typed key disambiguate client correlations;
	// raw caller identifiers are never copied into this audit projection.
	encoding, _ := json.Marshal(key)
	digest := sha256.Sum256(encoding)
	return "operation-" + hex.EncodeToString(digest[:])
}

func (s *Store) writeMutationAudit(ctx context.Context, tx pgx.Tx, plan opPlan, roomID, actor, actorKind, kind, outcome, detail, target string, count *int64, at time.Time) error {
	if target != "" && !validRoomCreateIdentity(target) || count != nil && *count < 0 {
		return errors.New("invalid typed mutation audit")
	}
	var roomValue, actorValue, targetValue, countValue any
	if roomID != "" {
		roomValue = roomID
	}
	if actor != "" {
		actorValue = actor
	}
	if target != "" {
		targetValue = target
	}
	if count != nil {
		countValue = *count
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+s.table("audit")+`(deployment_id,room_id,user_id,actor_kind,kind,outcome,detail,target_id,operation_id,invalidated_approval_count,at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, s.cfg.DeploymentID, roomValue, actorValue, actorKind, kind, outcome, detail, targetValue, mutationOperationID(plan.key), countValue, at)
	return err
}
func auditCount(plan opPlan) *int64 {
	if plan.auditState == nil {
		return nil
	}
	return plan.auditState.invalidatedApprovals
}
func controlAuditTarget(receipt ControlReceipt) string {
	switch receipt.Kind {
	case controlUserStatus, controlAdminSet:
		return receipt.TargetUserID
	case controlLogin:
		return receipt.UserID
	case controlRevoke:
		return receipt.SessionID
	case ControlDeviceComplete, ControlDeviceRotate, ControlDeviceRecover, ControlDeviceRevoke:
		return receipt.DeviceID
	case ControlAgentRegister:
		return receipt.AgentID
	}
	return ""
}
