package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

const (
	// discoveryRestartLimit bounds the side-effect-free restart of the
	// discovery phase when locked dependencies differ from the discovery
	// reads. The kernel performs no effect retries; this is the only loop and
	// it is classified.
	discoveryRestartLimit = 3
	// maxOperationBudget bounds every Commit, Resolve and ReadonlySnapshot —
	// including waiting for the work connection — even when the caller passes
	// an unbounded context. The earlier of this budget and the caller's
	// deadline applies.
	maxOperationBudget = 60 * time.Second
	// openBudget bounds Open (and its ownership transaction) when the caller
	// passes an unbounded context.
	openBudget = 30 * time.Second
)

// Config binds one Store to exactly one immutable deployment inside one
// schema. Every application table is addressed schema-qualified; pinning
// search_path alone is not relied on to exclude cross-schema or pg_temp
// shadow resolution.
type Config struct {
	DSN          string
	Schema       string
	DeploymentID string
	// RequireSessionAuth forbids User-only authorization in public adapters.
	// False is retained solely for existing trusted internal callers/fixtures.
	RequireSessionAuth bool
	AttachmentLimits   AttachmentLimits
	ResourceLimits     ResourceLimits
}

// Store is the single-writer transaction kernel for one deployment. The lease
// connection is dedicated to the deployment's advisory lock; the work
// connection serializes Commit and Resolve because pgx.Conn is not safe for
// concurrent use. This slice deliberately provides no pool.
type Store struct {
	cfg                                                Config
	lease                                              *leaseState
	work                                               *pgx.Conn
	sem                                                chan struct{}
	poisoned                                           error
	closed                                             bool
	recoverySealed, recoveryStarted                    bool
	recoveryRunning, recoveryFailed, recoveryCompleted atomic.Bool
}

// Open connects the lease and work connections, verifies the schema version
// and the bound deployment, and acquires the deployment's ownership lease.
// The whole open is bounded even for an unbounded caller context.
func Open(ctx context.Context, cfg Config) (*Store, error) {
	if cfg.DSN == "" || cfg.DeploymentID == "" || !schemaNamePattern.MatchString(cfg.Schema) {
		return nil, fmt.Errorf("sqlstore: DSN, schema and deployment id are required")
	}
	if !cfg.AttachmentLimits.valid() {
		return nil, fmt.Errorf("sqlstore: invalid attachment limits")
	}
	if !cfg.ResourceLimits.valid() {
		return nil, fmt.Errorf("sqlstore: invalid resource limits")
	}
	ctx, cancel := context.WithTimeout(ctx, openBudget)
	defer cancel()
	lease, err := openLease(ctx, cfg)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			cctx, ccancel := cleanupCtx()
			defer ccancel()
			_ = lease.conn.Close(cctx)
		}
	}()
	// openLease verified the schema version and the bound deployment on the
	// lease connection before taking the lock.
	work, err := pgx.Connect(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("sqlstore: connect work: %w", err)
	}
	if err := configureSession(ctx, work, cfg.Schema); err != nil {
		cctx, ccancel := cleanupCtx()
		defer ccancel()
		_ = work.Close(cctx)
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
	ownerUserID    string
	orgID          string
	hostMode       string
	archived       bool
	target         membershipTargetState
	credential     sessionDependency
	control        *controlDependencies
	governance     *governanceDependencies
	binding        *bindingDependencies
	recovery       *recoveryDependencies
	catalog        *catalogDependencies
	attachment     *attachmentDependencies
	restore        *restoreDependencies
	attachmentRefs [8]attachmentState
}

func (d dependencyState) equal(other dependencyState) bool {
	if d.restore != nil || other.restore != nil {
		return d.restore != nil && other.restore != nil && *d.restore == *other.restore
	}
	if d.attachment != nil || other.attachment != nil {
		return d.attachment != nil && other.attachment != nil && *d.attachment == *other.attachment
	}
	if d.catalog != nil || other.catalog != nil {
		return d.catalog != nil && other.catalog != nil && *d.catalog == *other.catalog
	}
	if d.recovery != nil || other.recovery != nil {
		return d.recovery != nil && other.recovery != nil && *d.recovery == *other.recovery
	}
	if d.binding != nil || other.binding != nil {
		return d.binding != nil && other.binding != nil && *d.binding == *other.binding
	}
	if d.governance != nil || other.governance != nil {
		return d.governance != nil && other.governance != nil && *d.governance == *other.governance
	}
	if d.control != nil || other.control != nil {
		return equalControlDependencies(d.control, other.control)
	}
	return d.userMissing == other.userMissing && d.userDisabled == other.userDisabled &&
		d.memberMissing == other.memberMissing && d.memberDisabled == other.memberDisabled &&
		d.memberRole == other.memberRole && d.roomMissing == other.roomMissing &&
		d.ownerUserID == other.ownerUserID && d.orgID == other.orgID && d.hostMode == other.hostMode && d.archived == other.archived && d.target == other.target && d.credential == other.credential && d.attachmentRefs == other.attachmentRefs
}

// readAllowed reports live read authorization: the user and the membership
// must exist and be enabled, at any role.
func (d dependencyState) readAllowed() bool {
	return !d.userMissing && !d.userDisabled && !d.memberMissing && !d.memberDisabled && d.credential.allowed()
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
func (s *Store) readDependencies(ctx context.Context, q rowQuerier, roomID string, auth AuthContext, d *dependencyState, lock string) error {
	var pending bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("restore_runs")+` WHERE completed_at IS NULL)`).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return refusedError(ClassRestorePending, nil)
	}
	userID := auth.UserID
	err := q.QueryRow(ctx, `SELECT disabled FROM `+s.table("users")+` WHERE user_id = $1`+lock, userID).Scan(&d.userDisabled)
	if errors.Is(err, pgx.ErrNoRows) {
		d.userMissing = true
	} else if err != nil {
		return err
	}
	d.credential, err = s.readCredential(ctx, q, auth, lock)
	if err != nil {
		return err
	}
	err = q.QueryRow(ctx, `SELECT role, disabled FROM `+s.table("room_members")+` WHERE room_id = $1 AND user_id = $2`+lock, roomID, userID).Scan(&d.memberRole, &d.memberDisabled)
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
func (s *Store) readRoom(ctx context.Context, q rowQuerier, roomID string, d *dependencyState, lock string) error {
	err := q.QueryRow(ctx, `SELECT current_seq, revision, owner_user_id, org_id, host_mode, archived_at IS NOT NULL FROM `+s.table("rooms")+` WHERE room_id = $1`+lock, roomID).Scan(&d.currentSeq, &d.revision, &d.ownerUserID, &d.orgID, &d.hostMode, &d.archived)
	if errors.Is(err, pgx.ErrNoRows) {
		d.roomMissing = true
	} else if err != nil {
		return err
	}
	return nil
}

// opPlan is the operation-specific half of the shared transaction state
// machine: append_fact, human_send, room_create and room_membership each supply
// one, and the machine below is the only transition implementation. The encoding budget is
// per operation: append_fact keeps its original 262144-byte gate unchanged,
// while human_send carries an explicitly bounded larger budget for its
// worst-case-escaped text.
type opPlan struct {
	key                   OpKey
	userID                string
	audit                 AuditRecord
	hash                  [32]byte
	expectedRevision      uint64
	encodingBudget        int
	roomCreate            *model.EnterpriseRoomIntent
	roomMembership        *model.EnterpriseMembershipIntent
	auth                  AuthContext
	control               *controlMutation
	relay                 *relayMutation
	governance            *governanceMutation
	binding               *bindingMutation
	recovery              *recoveryMutation
	catalog               *catalogMutation
	attachment            *attachmentMutation
	restore               *restoreMutation
	human                 *HumanSendIntent
	produceHuman          bool
	roomReadAuthorization bool
	auditState            *mutationAuditState
}

// ownershipLock is fixed before any row is locked. Room creation and its
// resolution must exclude each other even when the target Room does not yet
// exist. Membership controls use the same exclusive guard while their target
// row can be absent; ordinary appends retain their shared ownership guard.
func (p opPlan) ownershipLock() string {
	if p.restore != nil {
		return lockExclusive
	}
	if p.attachment != nil {
		if attachmentReadOnly(p.attachment.kind) {
			return lockShare
		}
		return lockExclusive
	}
	if p.catalog != nil {
		if catalogReadOnly(p.catalog.kind) {
			return lockShare
		}
		return lockExclusive
	}
	if p.recovery != nil {
		if p.recovery.kind == recoveryPage {
			return lockShare
		}
		return lockExclusive
	}
	if p.binding != nil {
		if bindingReadOnly(p.binding.kind) {
			return lockShare
		}
		return lockExclusive
	}
	if p.governance != nil {
		if p.governance.kind == governanceRead || p.governance.kind == governanceList {
			return lockShare
		}
		return lockExclusive
	}
	if p.control != nil {
		if controlReadOnly(p.control.kind) {
			return lockShare
		}
		return lockExclusive
	}
	if p.roomCreate != nil || p.roomMembership != nil {
		return lockExclusive
	}
	return lockShare
}

// Commit runs the transaction state machine for one append_fact command and
// never mutates or notifies anything outside the transaction it reports.
func (s *Store) Commit(ctx context.Context, cmd Command) (Result, error) {
	if class := validateCommand(cmd); class != "" {
		return refused(class, nil)
	}
	if cmd.Auth.Deployment != s.cfg.DeploymentID {
		return refused(ClassInvalidRequest, nil)
	}
	plan := opPlan{
		auth:             cmd.Auth,
		key:              cmd.Key,
		userID:           cmd.Auth.UserID,
		audit:            cmd.Mutation.Audit,
		hash:             canonicalHash(cmd.Key, cmd.Mutation),
		expectedRevision: cmd.ExpectedRevision,
		encodingBudget:   maxEncodingBytes,
	}
	return s.commitWithRestarts(ctx, plan, cmd.Mutation.Fact)
}

// commitWithRestarts serializes one operation and runs the bounded,
// side-effect-free discovery restart loop.
func (s *Store) commitWithRestarts(ctx context.Context, plan opPlan, fact model.Event) (Result, error) {
	opCtx, release, err := s.beginOperation(ctx)
	if err != nil {
		return s.acquireFailure(ctx, err)
	}
	defer release()
	if plan.recovery == nil && plan.restore == nil {
		if s.recoveryRunning.Load() || s.recoveryFailed.Load() {
			return refused(ClassStartupRecovery, nil)
		}
		s.recoverySealed = true
	}
	if s.closed {
		return refused(ClassClosed, nil)
	}
	if s.poisoned != nil {
		return refused(ClassLeaseLost, s.poisoned)
	}
	for restarts := 0; ; restarts++ {
		result, err, restart := s.commitPlanned(opCtx, plan, fact)
		if !restart {
			return result, err
		}
		if restarts >= discoveryRestartLimit {
			return notCommitted(ClassDiscoveryRestart, nil)
		}
	}
}

// refused3 adapts the two-value refusal constructor to commitPlanned's
// three-value contract (no restart requested).
func refused3(class string, cause error) (Result, error, bool) {
	result, err := refused(class, cause)
	return result, err, false
}

// commitPlanned performs one discovery/lock/verify/commit attempt for one
// operation plan. The third return requests a side-effect-free restart of the
// discovery phase.
func (s *Store) commitPlanned(ctx context.Context, plan opPlan, fact model.Event) (Result, error, bool) {
	// A discovery restart cannot retain a previous attempt's measured effects.
	plan.auditState = &mutationAuditState{}
	ctx = context.WithValue(ctx, mutationAuditContextKey{}, plan.key)
	cutoffContext, cutoffErr := captureApprovalCutoff(ctx, s.work, plan)
	if cutoffErr != nil {
		r, e := s.preEffectFailure(ctx, cutoffErr)
		return r, e, false
	}
	ctx = cutoffContext
	var discovered dependencyState
	// No transaction of this operation has started while reading, so a failure
	// here is a definite non-commit.
	if err := s.readPlanDependencies(ctx, s.work, plan, &discovered, lockNone); err != nil {
		result, err := s.preEffectFailure(ctx, err)
		return result, err, false
	}
	if err := s.readPlannedRoom(ctx, s.work, plan, &discovered, lockNone); err != nil {
		result, err := s.preEffectFailure(ctx, err)
		return result, err, false
	}
	tx, err := s.work.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		result, err := s.preEffectFailure(ctx, err)
		return result, err, false
	}
	defer func() {
		rctx, cancel := cleanupCtx()
		defer cancel()
		_ = tx.Rollback(rctx)
	}()

	// Lock the full set once, in the documented global order and at the
	// strongest mode any later step needs. Nothing is locked after this.
	var ownerEpoch int64
	var ownerPID int32
	var ownerStart time.Time
	if err := tx.QueryRow(ctx, `SELECT epoch, lease_pid, lease_backend_start FROM `+s.table("ownership")+` WHERE id`+plan.ownershipLock()).Scan(&ownerEpoch, &ownerPID, &ownerStart); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	var locked dependencyState
	if err := s.readPlanDependencies(ctx, tx, plan, &locked, lockShare); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	if err := s.readPlannedRoom(ctx, tx, plan, &locked, lockExclusive); err != nil {
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

	var result Result
	if plan.restore != nil {
		result, err = s.applyRestorePlan(ctx, tx, plan, locked.restore)
	} else if plan.attachment != nil {
		result, err = s.applyAttachmentPlan(ctx, tx, plan, locked.attachment)
	} else if plan.recovery != nil {
		result, err = s.applyRecoveryPlan(ctx, tx, plan, locked.recovery)
	} else if plan.catalog != nil {
		result, err = s.applyCatalogPlan(ctx, tx, plan, locked.catalog)
	} else if plan.control != nil {
		result, err = s.applyControlPlan(ctx, tx, plan, locked.control)
	} else if plan.governance != nil {
		result, err = s.applyGovernancePlan(ctx, tx, plan, locked.governance)
	} else if plan.binding != nil {
		result, err = s.applyBindingPlan(ctx, tx, plan, locked.binding)
	} else if plan.relay != nil {
		result, err = s.applyRelayPlan(ctx, tx, plan, locked)
	} else {
		var unusedRestart bool
		result, err, unusedRestart = s.applyRoomPlan(ctx, tx, plan, fact, locked)
		if unusedRestart {
			return Result{}, nil, true
		}
	}
	if err != nil || result.Outcome != OutcomeCommitted {
		return result, err, false
	}
	// All operation scopes share this only COMMIT and its uncertainty
	// classification. Scope-specific handlers prepare/write database rows;
	// they never begin, commit, retry or publish an effect themselves.
	if err := tx.Commit(ctx); err != nil {
		return s.commitFailure(err)
	}
	return result, nil, false
}

func (s *Store) readPlannedRoom(ctx context.Context, q rowQuerier, plan opPlan, d *dependencyState, lock string) error {
	if plan.control != nil || plan.governance != nil || plan.binding != nil || plan.recovery != nil || plan.catalog != nil || plan.attachment != nil || plan.restore != nil {
		return nil
	}
	return s.readRoom(ctx, q, plan.key.RoomID, d, lock)
}

// applyRoomPlan is the existing Room-domain decision/write body. The common
// runner owns its transaction, discovery restart, fence and final COMMIT.
func (s *Store) applyRoomPlan(ctx context.Context, tx pgx.Tx, plan opPlan, fact model.Event, locked dependencyState) (Result, error, bool) {

	// Live read authorization gates every disclosure, including a replay of
	// the caller's own accepted operation. A disabled or removed principal is
	// refused before any stored result is shown.
	if plan.roomCreate != nil {
		// A new Room has no membership yet. Every creation still requires an
		// admitted, enabled User before even a body-free conflict is shown.
		if locked.userMissing || locked.userDisabled || !locked.credential.allowed() {
			return refused3(ClassAuthorization, nil)
		}
	} else {
		switch {
		case locked.roomMissing:
			return refused3(ClassRoomNotFound, nil)
		case !locked.readAllowed():
			return refused3(ClassAuthorization, nil)
		}
	}

	// Idempotency: same key and identical stable mutation replays the original
	// stored fact; same key with a different mutation is a definite conflict —
	// the same OutcomeConflict that Resolve reports for this situation.
	var storedHash []byte
	var resultSeq int64
	var resultRevision int64
	err := tx.QueryRow(ctx, `
		SELECT intent_hash, result_seq, result_revision FROM `+s.table("operations")+`
		 WHERE op_namespace = $1 AND deployment_id = $2 AND room_id = $3
		   AND binding_id = $4 AND generation = $5 AND op_kind = $6 AND identifier = $7 AND principal = $8`,
		plan.key.Namespace, plan.key.Deployment, plan.key.RoomID, plan.key.BindingID, int64(plan.key.Generation), plan.key.OpKind, plan.key.Identifier, plan.key.Principal).Scan(&storedHash, &resultSeq, &resultRevision)
	switch {
	case err == nil:
		if plan.roomCreate != nil && !locked.readAllowed() {
			// A receipt belongs to the principal, but current Room read
			// permission is still required before hash or result disclosure.
			return refused3(ClassAuthorization, nil)
		}
		if !bytes.Equal(storedHash, plan.hash[:]) {
			kerr := &KernelError{Outcome: OutcomeConflict, Class: ClassPayloadConflict}
			return Result{Outcome: OutcomeConflict, Class: ClassPayloadConflict, Err: kerr, Revision: uint64(resultRevision)}, kerr, false
		}
		original, err := s.loadFact(ctx, tx, plan.key.RoomID, uint64(resultSeq))
		if err != nil {
			return refused3(ClassStoredFactUnreadable, err)
		}
		if plan.roomCreate != nil {
			if err := s.validateRoomCreateReplay(ctx, tx, plan, original); err != nil {
				return refused3(ClassStoredFactUnreadable, err)
			}
		}
		if plan.roomMembership != nil {
			if err := validateRoomMembershipReplay(plan, original); err != nil {
				return refused3(ClassStoredFactUnreadable, err)
			}
		}
		if plan.human != nil {
			if err := s.validateHumanMessageReplay(ctx, tx, plan, original); err != nil {
				if isStoredFactCorruption(err) {
					return refused3(ClassStoredFactUnreadable, err)
				}
				return s.postBeginFailure(ctx, err)
			}
		}
		// Replayed returns the original stored result, including the Room
		// revision that original commit defined — never the current revision,
		// which may have advanced past it.
		return Result{Outcome: OutcomeReplayed, Seq: uint64(resultSeq), Revision: uint64(resultRevision), Fact: original}, nil, false
	case errors.Is(err, pgx.ErrNoRows):
		// A new operation.
	default:
		return s.postBeginFailure(ctx, err)
	}

	if plan.roomCreate != nil {
		if !locked.roomMissing {
			// No matching operation exists: an existing ID is never adopted,
			// upserted or disclosed, including for its current owner.
			return refused3(ClassRoomTaken, nil)
		}
	} else if plan.roomMembership != nil {
		if !locked.readAllowed() || locked.memberRole != "owner" || locked.ownerUserID != plan.userID {
			return refused3(ClassAuthorization, nil)
		}
	} else if !locked.writeAllowed() {
		// A new operation on an existing Room requires a writer role.
		return refused3(ClassAuthorization, nil)
	}

	// Strict compare-and-swap against the locked revision. Zero is a legal
	// initial revision; there is no bypass. A mismatch never appends the stale
	// fact — the caller rebuilds its plan.
	if plan.expectedRevision != locked.revision {
		kerr := &KernelError{Outcome: OutcomeRefused, Class: ClassRevisionConflict}
		return Result{Outcome: OutcomeRefused, Class: ClassRevisionConflict, Err: kerr, Revision: locked.revision}, kerr, false
	}
	// A commit defines the new revision as expected+1 and the new sequence as
	// current+1; both must fit the bigint range — overflow is refused, never
	// truncated.
	if locked.currentSeq >= math.MaxInt64 || locked.revision >= math.MaxInt64 {
		return refused3(ClassRangeExceeded, nil)
	}

	var createOrg string
	if plan.human != nil {
		if locked.archived {
			return refused3(ClassInvalidControlState, nil)
		}
		metadata, e := attachmentReferenceMetadata(plan, locked)
		if e != nil {
			return refused3(ClassAuthorization, e)
		}
		if plan.produceHuman {
			fact, err = s.prepareHumanMessage(ctx, tx, plan, locked, metadata)
			if err != nil {
				if errors.Is(err, relay.ErrEnterpriseQueueLimit) {
					return refused3(ClassResourceLimit, err)
				}
				return refused3(ClassInvalidControlState, err)
			}
		}
		if e := s.validateHumanMessageCandidate(ctx, tx, plan, locked, fact, metadata); e != nil {
			return refused3(ClassInvalidFact, e)
		}
	}
	if plan.roomCreate != nil {
		fact, createOrg, err = s.creationFact(ctx, tx, plan)
		if err != nil {
			return s.postBeginFailure(ctx, err)
		}
	}
	if plan.roomMembership != nil {
		var class string
		fact, class, err = s.prepareMembership(ctx, tx, plan, locked)
		if class != "" {
			return refused3(class, err)
		}
		if err != nil {
			return s.postBeginFailure(ctx, err)
		}
		if membershipRevokes(*plan.roomMembership) {
			scope := authorityRevocationScope{UserID: plan.roomMembership.TargetUserID, RoomID: plan.key.RoomID}
			if err := s.retireAuthorityBindings(ctx, tx, scope, plan.userID); err != nil {
				if isStoredFactCorruption(err) {
					return refused3(ClassStoredFactUnreadable, err)
				}
				return s.postBeginFailure(ctx, err)
			}
			count, err := s.revokeNativeCredentials(ctx, tx, scope)
			if err != nil {
				return s.postBeginFailure(ctx, err)
			}
			setAuditInvalidations(plan, count)
			// Retirement facts and the membership fact share this Room's one
			// revision change. The exclusive guard and prelocked scope remain
			// held; only the sequence advanced within this same transaction.
			if err := tx.QueryRow(ctx, `SELECT current_seq FROM `+s.table("rooms")+` WHERE room_id=$1`, plan.key.RoomID).Scan(&locked.currentSeq); err != nil {
				return s.postBeginFailure(ctx, err)
			}
			if locked.currentSeq >= math.MaxInt64 {
				return refused3(ClassRangeExceeded, nil)
			}
			fact.CreatedAt = time.Now().UTC()
		}
	}
	stored := fact
	stored.Seq = locked.currentSeq + 1
	encoding, err := json.Marshal(stored)
	if err != nil || len(encoding) > plan.encodingBudget {
		return refused3(ClassInvalidFact, err)
	}
	if plan.roomMembership == nil || !membershipRevokes(*plan.roomMembership) {
		var budgetErr error
		if !s.cfg.RequireSessionAuth {
			privateEncoding, e := encodeStoredFact(stored, userFactOrigin(plan.userID))
			if e != nil {
				return refused3(ClassInvalidFact, e)
			}
			budgetErr = s.checkRoomNewWorkBudget(ctx, tx, plan.key.RoomID, 1, int64(len(privateEncoding)), 0, 0)
		} else {
			snap := RoomSnapshot{RoomID: plan.key.RoomID, DeploymentID: plan.key.Deployment, OrgID: locked.orgID, HostMode: locked.hostMode, Seq: locked.currentSeq, Revision: locked.revision}
			if plan.roomCreate != nil {
				snap.OrgID = createOrg
				snap.CreationUserID = plan.userID
				snap.HostMode = string(model.HostNative)
			} else {
				var e error
				snap, e = s.readRoomEvents(ctx, tx, snap)
				if e != nil {
					r, e := resourceAdmissionFailure(s, ctx, e)
					return r, e, false
				}
			}
			budgetErr = s.checkRoomFactAdmission(ctx, tx, plan, []model.Event{stored}, snap)
		}
		if budgetErr != nil {
			r, e := resourceAdmissionFailure(s, ctx, budgetErr)
			return r, e, false
		}
	}
	if plan.roomCreate != nil {
		if e := s.checkPersistentAdmission(ctx, tx, "rooms", 1); e != nil {
			r, e := resourceAdmissionFailure(s, ctx, e)
			return r, e, false
		}
		if e := s.checkMembershipAdmission(ctx, tx, "room", plan.key.RoomID, 1); e != nil {
			r, e := resourceAdmissionFailure(s, ctx, e)
			return r, e, false
		}
		if err := s.insertCreatedRoom(ctx, tx, plan, createOrg, stored.CreatedAt); err != nil {
			return s.postBeginFailure(ctx, err)
		}
	}
	if plan.roomMembership != nil {
		if plan.roomMembership.Action == MembershipAdmit && locked.target.memberMissing {
			if e := s.checkMembershipAdmission(ctx, tx, "room", plan.key.RoomID, 1); e != nil {
				r, e := resourceAdmissionFailure(s, ctx, e)
				return r, e, false
			}
		}
		if err := s.writeMembership(ctx, tx, plan, locked); err != nil {
			return s.postBeginFailure(ctx, err)
		}
	}
	now := time.Now().UTC()
	if err := s.writeStoredRoomEvent(ctx, tx, stored, userFactOrigin(plan.userID)); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	if plan.human != nil {
		if e := s.writeHumanMessageReferences(ctx, tx, plan, stored, locked); e != nil {
			return s.postBeginFailure(ctx, e)
		}
	}

	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("operations")+` (op_namespace, deployment_id, room_id, binding_id, generation, op_kind, identifier, principal, intent_hash, expected_revision, result_seq, result_revision, committed_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		plan.key.Namespace, plan.key.Deployment, plan.key.RoomID, plan.key.BindingID, int64(plan.key.Generation), plan.key.OpKind, plan.key.Identifier, plan.key.Principal, plan.hash[:], int64(plan.expectedRevision), int64(stored.Seq), int64(locked.revision+1), now); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	target := plan.key.RoomID
	if plan.roomMembership != nil {
		target = plan.roomMembership.TargetUserID
	}
	if err := s.writeMutationAudit(ctx, tx, plan, plan.key.RoomID, plan.userID, "user", plan.audit.Kind, plan.audit.Outcome, plan.audit.Detail, target, auditCount(plan), now); err != nil {
		return s.postBeginFailure(ctx, err)
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET current_seq = $2, revision = $3 WHERE room_id = $1`,
		plan.key.RoomID, int64(stored.Seq), int64(locked.revision+1)); err != nil {
		return s.postBeginFailure(ctx, err)
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

// loadFact decodes the authoritative stored encoding for one sequence and
// cross-checks the index columns; the index columns are a derived projection
// and are never the replay source.
func (s *Store) loadFact(ctx context.Context, q rowQuerier, roomID string, seq uint64) (model.Event, error) {
	var eventID, kind, actor, originKind string
	var originUser *string
	var factVersion int
	var encoding []byte
	err := q.QueryRow(ctx, `SELECT event_id, fact_version, kind, actor, event_encoding, origin_kind, origin_user FROM `+s.table("room_events")+` WHERE room_id = $1 AND seq = $2`, roomID, int64(seq)).
		Scan(&eventID, &factVersion, &kind, &actor, &encoding, &originKind, &originUser)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Event{}, &storedFactCorruption{err: err}
		}
		return model.Event{}, err
	}
	event, err := decodeStoredFact(roomID, seq, eventID, factVersion, kind, actor, encoding, originKind, originUser)
	if err != nil {
		return model.Event{}, &storedFactCorruption{err: err}
	}
	return event, nil
}

// storedFactCorruption identifies a completed read whose bytes or invariants
// are invalid. It is deliberately distinct from an interrupted SQL read,
// which proves neither corruption nor an operation's final outcome.
type storedFactCorruption struct{ err error }

func (e *storedFactCorruption) Error() string { return e.err.Error() }
func (e *storedFactCorruption) Unwrap() error { return e.err }

func isStoredFactCorruption(err error) bool {
	var corrupt *storedFactCorruption
	return errors.As(err, &corrupt)
}
