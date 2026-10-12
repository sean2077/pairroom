package sqlstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// rowQuerier is the guarded-read subset of pgx; both pgx.Tx and *pgx.Conn
// implement it.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// leaseKey derives the deployment's advisory-lock key in the bigint form. The
// recorded tag therefore splits across classid (high 32 bits) and objid (low
// 32 bits) with objsubid = 1, verified on PostgreSQL 17.11.
func leaseKey(deploymentID string) uint64 {
	sum := sha256.Sum256([]byte("pairroom-sqlstore-lease\x00" + deploymentID))
	return binary.BigEndian.Uint64(sum[:8])
}

// errLeaseLost marks the sticky fence failure: the ownership record no longer
// belongs to this incarnation, or the registered lease backend no longer holds
// the deployment's advisory lock. The incarnation is retired permanently.
var errLeaseLost = errors.New("sqlstore: ownership lease lost")

type leaseState struct {
	conn    *pgx.Conn
	pid     uint32
	start   time.Time
	epoch   int64
	classid uint32
	objid   uint32
}

func newInstanceID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "inst-" + hex.EncodeToString(raw[:]), nil
}

// openLease connects the dedicated lease connection, pins the search path and
// takes the deployment's session advisory lock. The lock lives on this
// connection for the Store's lifetime and is never handed to a transaction or
// taken via a one-shot statement. A live holder makes Open fail closed; on
// success the ownership row advances to this incarnation's epoch.
func openLease(ctx context.Context, cfg Config) (*leaseState, error) {
	conn, err := pgx.Connect(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: connect lease: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close(context.WithoutCancel(ctx))
		}
	}()
	if err := pinSearchPath(ctx, conn, cfg.Schema); err != nil {
		return nil, err
	}
	if err := verifySchemaVersion(ctx, conn, cfg.Schema); err != nil {
		return nil, err
	}
	if err := verifyDeployment(ctx, conn, cfg.Schema, cfg.DeploymentID); err != nil {
		return nil, err
	}
	key := leaseKey(cfg.DeploymentID)
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_lock($1)`, int64(key)).Scan(&held); err != nil {
		return nil, fmt.Errorf("sqlstore: acquire lease: %w", err)
	}
	if !held {
		return nil, fmt.Errorf("sqlstore: deployment %q already has a live lease holder", cfg.DeploymentID)
	}
	lease := &leaseState{conn: conn, pid: conn.PgConn().PID(), classid: uint32(key >> 32), objid: uint32(key & 0xffffffff)}
	if err := conn.QueryRow(ctx, `SELECT backend_start FROM pg_catalog.pg_stat_activity WHERE pid = $1`, int32(lease.pid)).Scan(&lease.start); err != nil {
		return nil, fmt.Errorf("sqlstore: read lease backend start: %w", err)
	}
	instance, err := newInstanceID()
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("sqlstore: open ownership transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var current int64
	err = tx.QueryRow(ctx, `SELECT epoch FROM `+qualified(cfg.Schema, "ownership")+` WHERE id FOR UPDATE`).Scan(&current)
	switch {
	case err == nil:
		lease.epoch = current + 1
		if _, err := tx.Exec(ctx, `UPDATE `+qualified(cfg.Schema, "ownership")+` SET instance_id = $1, epoch = $2, lease_pid = $3, lease_backend_start = $4, updated_at = pg_catalog.now() WHERE id`,
			instance, lease.epoch, int32(lease.pid), lease.start); err != nil {
			return nil, fmt.Errorf("sqlstore: advance ownership: %w", err)
		}
	case errors.Is(err, pgx.ErrNoRows):
		lease.epoch = 1
		if _, err := tx.Exec(ctx, `INSERT INTO `+qualified(cfg.Schema, "ownership")+` (instance_id, epoch, lease_pid, lease_backend_start) VALUES ($1, $2, $3, $4)`,
			instance, lease.epoch, int32(lease.pid), lease.start); err != nil {
			return nil, fmt.Errorf("sqlstore: register ownership: %w", err)
		}
	default:
		return nil, fmt.Errorf("sqlstore: read ownership: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("sqlstore: record ownership: %w", err)
	}
	ok = true
	return lease, nil
}

// fenceHoldsLocked verifies inside a transaction that has already taken the
// ownership row FOR SHARE that the registered lease incarnation is still this
// Store and that its backend currently holds the exact advisory lock: the
// recorded pid and start identity, the current database OID, the complete
// bigint-form tag, granted, and an exclusive mode. An application name is a
// diagnostic label, never the identity. Any mismatch is a sticky lease loss.
func (s *Store) fenceHoldsLocked(ctx context.Context, q rowQuerier, ownerEpoch int64, ownerPID int32, ownerStart time.Time) error {
	if ownerEpoch != s.lease.epoch || uint32(ownerPID) != s.lease.pid || !ownerStart.Equal(s.lease.start) {
		return errLeaseLost
	}
	var held bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_locks l JOIN pg_catalog.pg_stat_activity a ON a.pid = l.pid
			 WHERE l.locktype = 'advisory'
			   AND l.database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())
			   AND a.pid = $1 AND a.backend_start = $2
			   AND l.classid::bigint = $3 AND l.objid::bigint = $4 AND l.objsubid = $5
			   AND l.mode = 'ExclusiveLock' AND l.granted)`,
		int32(s.lease.pid), s.lease.start, int64(s.lease.classid), int64(s.lease.objid), int16(1)).Scan(&held)
	if err != nil {
		return fmt.Errorf("check lease holder: %w", err)
	}
	if !held {
		return errLeaseLost
	}
	return nil
}
