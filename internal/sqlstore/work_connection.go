package sqlstore

import (
	"context"
	"github.com/jackc/pgx/v5"
)

// replaceClosedWork is called once on entry to a new explicit command while
// sem is held. It repairs transport only: it neither retries a prior command
// nor acquires ownership, advances the epoch or clears a retired incarnation.
// The ordinary transaction still verifies the original lease and live ACL.
func (s *Store) replaceClosedWork(ctx context.Context) error {
	if !s.work.IsClosed() {
		return nil
	}
	conn, err := pgx.Connect(ctx, s.cfg.DSN)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		if !accepted {
			closeCtx, cancel := cleanupCtx()
			defer cancel()
			_ = conn.Close(closeCtx)
		}
	}()
	if err := configureSession(ctx, conn, s.cfg.Schema); err != nil {
		return err
	}
	if err := verifySchemaVersion(ctx, conn, s.cfg.Schema); err != nil {
		return err
	}
	if err := verifyDeployment(ctx, conn, s.cfg.Schema, s.cfg.DeploymentID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.work = conn
	accepted = true
	return nil
}
