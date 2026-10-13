package sqlstore

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// Readiness contains only closed metadata suitable for a protected readiness
// projection. It contains no identity, credential, Room data or raw failure.
type Readiness struct {
	SchemaCompatible bool `json:"schema_compatible"`
	OwnershipCurrent bool `json:"ownership_current"`
	RecoveryComplete bool `json:"recovery_complete"`
	RestoreClear     bool `json:"restore_clear"`
}

func (s *Store) Readiness(ctx context.Context) (Readiness, error) {
	result, err := s.resolveWithFallback(ctx, opPlan{restore: &restoreMutation{kind: "health"}})
	if err != nil {
		return Readiness{}, err
	}
	if result.readiness == nil {
		return Readiness{}, refusedError(ClassStateInvalid, nil)
	}
	return *result.readiness, nil
}
func (s *Store) resolveReadiness(ctx context.Context, tx pgx.Tx, pending bool) (Result, error) {
	if err := verifySchemaVersion(ctx, tx, s.cfg.Schema); err != nil {
		return resolveIncomplete(ctx, err)
	}
	var recovered bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("startup_recovery_runs")+` WHERE ownership_epoch=$1)`, s.lease.epoch).Scan(&recovered); err != nil {
		return resolveIncomplete(ctx, err)
	}
	metadata := Readiness{SchemaCompatible: true, OwnershipCurrent: true, RecoveryComplete: recovered && s.recoveryCompleted.Load(), RestoreClear: !pending}
	return Result{Outcome: OutcomeReplayed, readiness: &metadata}, nil
}
