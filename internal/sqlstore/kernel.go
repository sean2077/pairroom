package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	// discoveryRestartLimit bounds the side-effect-free restart of the
	// discovery phase when locked dependencies differ from the discovery
	// reads. The kernel performs no effect retries; this is the only loop and
	// it is classified.
	discoveryRestartLimit = 3
	// maxOperationBudget bounds every Commit and Resolve — including waiting
	// for the work connection — even when the caller passes an unbounded
	// context. The earlier of this budget and the caller's deadline applies.
	maxOperationBudget = 60 * time.Second
)

// Config binds one Store to exactly one immutable deployment inside one
// schema. Every application table is addressed schema-qualified; pinning
// search_path alone is not relied on to exclude cross-schema or pg_temp
// shadow resolution.
type Config struct {
	DSN          string
	Schema       string
	DeploymentID string
}

// Store is the single-writer transaction kernel for one deployment. The lease
// connection is dedicated to the deployment's advisory lock; the work
// connection serializes Commit and Resolve because pgx.Conn is not safe for
// concurrent use. This slice deliberately provides no pool.
type Store struct {
	cfg      Config
	lease    *leaseState
	work     *pgx.Conn
	sem      chan struct{}
	poisoned error
	closed   bool

	// Test seams (nil outside tests): observe or perturb the narrow windows
	// between discovery and locking, right before COMMIT, and before the
	// Resolve lookup.
	afterDiscovery      func()
	beforeCommit        func()
	beforeResolveLookup func()
}

// Open connects the lease and work connections, verifies the schema version
// and the bound deployment, and acquires the deployment's ownership lease.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DSN == "" || cfg.DeploymentID == "" || !schemaNamePattern.MatchString(cfg.Schema) {
		return nil, fmt.Errorf("sqlstore: DSN, schema and deployment id are required")
	}
	lease, err := openLease(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = lease.conn.Close(context.WithoutCancel(ctx))
		}
	}()
	// openLease verified the schema version and the bound deployment on the
	// lease connection before taking the lock.
	work, err := pgx.Connect(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: connect work: %w", err)
	}
	if err := pinSearchPath(ctx, work, cfg.Schema); err != nil {
		_ = work.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	ok = true
	return &Store{cfg: cfg, lease: lease, work: work, sem: make(chan struct{}, 1)}, nil
}

// Close serializes with in-flight operations, then releases the work
// connection and the lease connection, which releases the advisory lock; the
// ownership row keeps this incarnation's epoch so a later Open advances it.
func (s *Store) Close() error {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	if s.closed {
		return nil
	}
	s.closed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(s.work.Close(ctx), s.lease.conn.Close(ctx))
}

// beginOperation applies the earlier of the caller's deadline and the
// internal budget, then serializes on the work connection.
func (s *Store) beginOperation(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	opCtx, cancel := context.WithTimeout(ctx, maxOperationBudget)
	select {
	case s.sem <- struct{}{}:
		return opCtx, func() { <-s.sem; cancel() }, nil
	case <-opCtx.Done():
		cancel()
		return nil, nil, opCtx.Err()
	}
}

// acquireFailure classifies a failure that happened before the operation
// touched anything: the outcome is a definite non-commit.
func (s *Store) acquireFailure(parent context.Context, err error) (Result, error) {
	if parent.Err() != nil {
		return notCommitted(ClassCanceled, err)
	}
	return notCommitted(ClassOperationBudget, err)
}

// dependencyState is one reading of the command's lock set.
type dependencyState struct {
	userMissing    bool
	userDisabled   bool
	memberMissing  bool
	memberDisabled bool
	memberRole     string
	roomMissing    bool
	currentSeq     uint64
	revision       uint64
}

func (d dependencyState) equal(other dependencyState) bool {
	return d.userMissing == other.userMissing && d.userDisabled == other.userDisabled &&
		d.memberMissing == other.memberMissing && d.memberDisabled == other.memberDisabled &&
		d.memberRole == other.memberRole && d.roomMissing == other.roomMissing
}

// readAllowed reports live read authorization: the user and the membership
// must exist and be enabled, at any role.
func (d dependencyState) readAllowed() bool {
	return !d.userMissing && !d.userDisabled && !d.memberMissing && !d.memberDisabled
}

// writeAllowed reports live write authorization for a new append.
func (d dependencyState) writeAllowed() bool {
	return d.readAllowed() && (d.memberRole == "owner" || d.memberRole == "member")
}

const (
	lockNone      = ""
	lockShare     = " FOR SHARE"
	lockExclusive = " FOR UPDATE"
)

// readDependencies reads the command's authorization dependencies in the
// documented global order (users, then room_members) with the requested lock
// clause.
func (s *Store) readDependencies(ctx context.Context, q rowQuerier, cmd Command, d *dependencyState, lock string) error {
	err := q.QueryRow(ctx, `SELECT disabled FROM `+s.table("users")+` WHERE user_id = $1`+lock, cmd.Auth.UserID).Scan(&d.userDisabled)
	if errors.Is(err, pgx.ErrNoRows) {
		d.userMissing = true
	} else if err != nil {
		return err
	}
	err = q.QueryRow(ctx, `SELECT role, disabled FROM `+s.table("room_members")+` WHERE room_id = $1 AND user_id = $2`+lock, cmd.Key.RoomID, cmd.Auth.UserID).Scan(&d.memberRole, &d.memberDisabled)
	if errors.Is(err, pgx.ErrNoRows) {
		d.memberMissing = true
	} else if err != nil {
		return err
	}
	return nil
}

// readRoom reads the Room consistency row — the sequence and revision truth —
// with the requested lock clause, in the documented position after the
// authorization rows.
func (s *Store) readRoom(ctx context.Context, q rowQuerier, cmd Command, d *dependencyState, lock string) error {
	err := q.QueryRow(ctx, `SELECT current_seq, revision FROM `+s.table("rooms")+` WHERE room_id = $1`+lock, cmd.Key.RoomID).Scan(&d.currentSeq, &d.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		d.roomMissing = true
	} else if err != nil {
		return err
	}
	return nil
}

// Commit runs the transaction state machine for one command and never mutates
// or notifies anything outside the transaction it reports.
func (s *Store) Commit(ctx context.Context, cmd Command) (Result, error) {
	if class := validateCommand(cmd); class != "" {
		return refused(class, nil)
	}
	if cmd.Auth.Deployment != s.cfg.DeploymentID {
		return refused(ClassInvalidRequest, nil)
	}
	opCtx, release, err := s.beginOperation(ctx)
	if err != nil {
		return s.acquireFailure(ctx, err)
	}
	defer release()
	if s.closed {
		return refused(ClassClosed, nil)
	}
	if s.poisoned != nil {
		return refused(ClassLeaseLost, s.poisoned)
	}
	hash := canonicalHash(cmd.Key, cmd.Mutation)
	for restarts := 0; ; restarts++ {
		result, err, restart := s.commitOnce(opCtx, cmd, hash)
		if !restart {
			return result, err
		}
		if restarts >= discoveryRestartLimit {
			return notCommitted(ClassDiscoveryRestart, nil)
		}
	}
}

// refused3 adapts the two-value refusal constructor to commitOnce's
// three-value contract (no restart requested).
func refused3(class string, cause error) (Result, error, bool) {
	result, err := refused(class, cause)
	return result, err, false
}

// commitOnce performs one discovery/lock/verify/commit attempt. The third
// return requests a side-effect-free restart of the discovery phase.
func (s *Store) commitOnce(ctx context.Context, cmd Command, hash [32]byte) (Result, error, bool) {
	var discovered dependencyState
	// No transaction of this operation has started while reading, so a failure
	// here is a definite non-commit.
	if err := s.readDependencies(ctx, s.work, cmd, &discovered, lockNone); err != nil {
		result, err := s.preEffectFailure(ctx, err)
		return result, err, false
	}
	if err := s.readRoom(ctx, s.work, cmd, &discovered, lockNone); err != nil {
		result, err := s.preEffectFailure(ctx, err)
		return result, err, false
	}
	if s.afterDiscovery != nil {
		s.afterDiscovery()
	}
	tx, err := s.work.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		result, err := s.preEffectFailure(ctx, err)
		return result, err, false
	}
	defer func() {
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	// Lock the full set once, in the documented global order and at the
	// strongest mode any later step needs. Nothing is locked after this.
	var ownerEpoch int64
	var ownerPID int32
	var ownerStart time.Time
	if err := tx.QueryRow(ctx, `SELECT epoch, lease_pid, lease_backend_start FROM `+s.table("ownership")+` WHERE id FOR SHARE`).Scan(&ownerEpoch, &ownerPID, &ownerStart); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	var locked dependencyState
	if err := s.readDependencies(ctx, tx, cmd, &locked, lockShare); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	if err := s.readRoom(ctx, tx, cmd, &locked, lockExclusive); err != nil {
		return s.postBeginFailure(ctx, err)
	}

	// Fence: the registered incarnation must still be this Store and must
	// still hold the exact lock. A loss is sticky — no restart, no revival.
	if err := s.fenceHoldsLocked(ctx, tx, ownerEpoch, ownerPID, ownerStart); err != nil {
		if errors.Is(err, errLeaseLost) {
			s.poisoned = err
			result, err := refused(ClassLeaseLost, err)
			return result, err, false
		}
		return s.postBeginFailure(ctx, err)
	}

	// Re-verify: locked dependencies must agree with the discovery reads;
	// otherwise roll back and restart the pure-discovery phase.
	if !discovered.equal(locked) {
		return Result{}, nil, true
	}

	// Live read authorization gates every disclosure, including a replay of
	// the caller's own accepted operation. A disabled or removed principal is
	// refused before any stored result is shown.
	switch {
	case locked.roomMissing:
		return refused3(ClassRoomNotFound, nil)
	case !locked.readAllowed():
		return refused3(ClassAuthorization, nil)
	}

	// Idempotency: same key and identical stable mutation replays the original
	// stored fact; same key with a different mutation is a definite conflict.
	var storedHash []byte
	var resultSeq int64
	err = tx.QueryRow(ctx, `
		SELECT intent_hash, result_seq FROM `+s.table("operations")+`
		 WHERE op_namespace = $1 AND deployment_id = $2 AND room_id = $3
		   AND binding_id = '' AND generation = 0 AND op_kind = $4 AND identifier = $5 AND principal = $6`,
		cmd.Key.Namespace, cmd.Key.Deployment, cmd.Key.RoomID, cmd.Key.OpKind, cmd.Key.Identifier, cmd.Key.Principal).Scan(&storedHash, &resultSeq)
	switch {
	case err == nil:
		if !bytes.Equal(storedHash, hash[:]) {
			return refused3(ClassPayloadConflict, nil)
		}
		original, err := s.loadFact(ctx, tx, cmd.Key.RoomID, uint64(resultSeq))
		if err != nil {
			return refused3(ClassStoredFactUnreadable, err)
		}
		// Replayed reports the current locked revision as-is; only a commit
		// defines expected+1.
		return Result{Outcome: OutcomeReplayed, Seq: uint64(resultSeq), Revision: locked.revision, Fact: original}, nil, false
	case errors.Is(err, pgx.ErrNoRows):
		// A new operation.
	default:
		return s.postBeginFailure(ctx, err)
	}

	// A new append additionally requires a writer role.
	if !locked.writeAllowed() {
		return refused3(ClassAuthorization, nil)
	}

	// Strict compare-and-swap against the locked revision. Zero is a legal
	// initial revision; there is no bypass. A mismatch never appends the stale
	// fact — the caller rebuilds its plan.
	if cmd.ExpectedRevision != locked.revision {
		kerr := &KernelError{Outcome: OutcomeRefused, Class: ClassRevisionConflict}
		return Result{Outcome: OutcomeRefused, Class: ClassRevisionConflict, Err: kerr, Revision: locked.revision}, kerr, false
	}
	// A commit defines the new revision as expected+1 and the new sequence as
	// current+1; both must fit the bigint range — overflow is refused, never
	// truncated.
	if locked.currentSeq >= math.MaxInt64 || locked.revision >= math.MaxInt64 {
		return refused3(ClassRangeExceeded, nil)
	}

	stored := cmd.Mutation.Fact
	stored.Seq = locked.currentSeq + 1
	encoding, err := json.Marshal(stored)
	if err != nil || len(encoding) > maxEncodingBytes {
		return refused3(ClassInvalidFact, err)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("room_events")+` (room_id, seq, event_id, fact_version, event_encoding, kind, actor, created_at, origin_user) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		cmd.Key.RoomID, int64(stored.Seq), stored.ID, factCodecVersion, encoding, stored.Kind, string(stored.Actor), stored.CreatedAt, cmd.Auth.UserID); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("operations")+` (op_namespace, deployment_id, room_id, binding_id, generation, op_kind, identifier, principal, intent_hash, expected_revision, result_seq, committed_at) VALUES ($1, $2, $3, '', 0, $4, $5, $6, $7, $8, $9, $10)`,
		cmd.Key.Namespace, cmd.Key.Deployment, cmd.Key.RoomID, cmd.Key.OpKind, cmd.Key.Identifier, cmd.Key.Principal, hash[:], int64(cmd.ExpectedRevision), int64(stored.Seq), now); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("audit")+` (deployment_id, room_id, user_id, kind, outcome, detail, at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		cmd.Key.Deployment, cmd.Key.RoomID, cmd.Auth.UserID, cmd.Mutation.Audit.Kind, cmd.Mutation.Audit.Outcome, cmd.Mutation.Audit.Detail, now); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET current_seq = $2, revision = $3 WHERE room_id = $1`,
		cmd.Key.RoomID, int64(stored.Seq), int64(locked.revision+1)); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	if s.beforeCommit != nil {
		s.beforeCommit()
	}
	if err := tx.Commit(ctx); err != nil {
		return s.commitFailure(err)
	}
	return Result{Outcome: OutcomeCommitted, Seq: stored.Seq, Revision: locked.revision + 1, Fact: stored}, nil, false
}

// preEffectFailure classifies a failure before any statement of the
// operation's transaction was sent: the outcome is definite.
func (s *Store) preEffectFailure(parent context.Context, err error) (Result, error) {
	if parent.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return notCommitted(ClassCanceled, err)
	}
	return notCommitted(ClassConnectionFailed, err)
}

// postBeginFailure classifies an error from a statement before COMMIT was
// sent. The transaction cannot have committed, so the outcome is definite;
// only the class varies.
func (s *Store) postBeginFailure(parent context.Context, err error) (Result, error, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001":
			result, err := notCommitted(ClassSerializationFailure, err)
			return result, err, false
		case "40P01":
			result, err := notCommitted(ClassDeadlockDetected, err)
			return result, err, false
		case "23505", "23503", "23514", "23502":
			result, err := notCommitted(ClassConstraintViolation, err)
			return result, err, false
		}
		result, err := notCommitted(ClassStatementFailed, err)
		return result, err, false
	}
	if parent.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result, err := notCommitted(ClassCanceled, err)
		return result, err, false
	}
	result, err := notCommitted(ClassConnectionFailed, err)
	return result, err, false
}

// commitFailure classifies a failed COMMIT. A server-side abort class proves
// no effect; everything else after the COMMIT was sent — deadline, network
// loss, connection exceptions, statement completion unknown, ErrTxClosed — is
// an unknown outcome and is never reported as rolled back.
func (s *Store) commitFailure(err error) (Result, error, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001":
			result, err := notCommitted(ClassSerializationFailure, err)
			return result, err, false
		case "40P01":
			result, err := notCommitted(ClassDeadlockDetected, err)
			return result, err, false
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result, err := unknown(ClassCommitUncertain, err)
		return result, err, false
	}
	result, err := unknown(ClassConnectionException, err)
	return result, err, false
}

// loadFact decodes the authoritative stored encoding and cross-checks the
// index columns (event id, version, kind, actor, room, sequence) against it;
// the index columns are a derived projection and are never the replay source.
func (s *Store) loadFact(ctx context.Context, q rowQuerier, roomID string, seq uint64) (model.Event, error) {
	var eventID, kind, actor string
	var factVersion int
	var encoding []byte
	err := q.QueryRow(ctx, `SELECT event_id, fact_version, kind, actor, event_encoding FROM `+s.table("room_events")+` WHERE room_id = $1 AND seq = $2`, roomID, int64(seq)).
		Scan(&eventID, &factVersion, &kind, &actor, &encoding)
	if err != nil {
		return model.Event{}, err
	}
	if factVersion != factCodecVersion {
		return model.Event{}, fmt.Errorf("stored fact version %d is not supported by this build", factVersion)
	}
	var event model.Event
	if err := json.Unmarshal(encoding, &event); err != nil {
		return model.Event{}, err
	}
	if event.ID != eventID || event.Seq != seq || event.RoomID != roomID || event.Kind != kind || string(event.Actor) != actor {
		return model.Event{}, fmt.Errorf("stored fact index columns disagree with the authoritative encoding")
	}
	return event, nil
}
