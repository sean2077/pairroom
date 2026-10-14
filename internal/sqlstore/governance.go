package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
)

const (
	governancePrepare  = "prepare"
	governanceFinish   = "finish"
	governanceTransfer = "transfer"
	governanceRead     = "read"
	governanceList     = "list"
	governanceTTL      = 5 * time.Minute
)

type governanceMutation struct {
	kind, actionID string
	intent         GovernanceIntent
	completion     enterpriseauth.Completion
}

type governanceActionState struct {
	missing                                       bool
	action                                        GovernanceAction
	actor, session                                string
	createdAt, readyAt, consumedAt, invalidatedAt time.Time
}

type governanceDependencies struct {
	actorMissing, actorDisabled, actorAdmin                                            bool
	ownerMissing, ownerDisabled, targetMissing, targetDisabled                         bool
	actorMemberMissing, actorMemberDisabled, targetMemberMissing, targetMemberDisabled bool
	actorRole, targetRole                                                              string
	credential                                                                         sessionDependency
	action                                                                             governanceActionState
	identityMatches                                                                    bool
	roomMissing                                                                        bool
	owner, orgID                                                                       string
	seq, revision                                                                      uint64
	approvalDigest                                                                     [32]byte
}

func (s *Store) governancePlan(auth AuthContext, clientID string, mutation governanceMutation) (opPlan, error) {
	if auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(auth.UserID) || !validRoomCreateIdentity(auth.SessionID) || auth.DeviceGrantID != "" || auth.BindingGrantID != "" || !validRoomCreateIdentity(clientID) {
		return opPlan{}, refusedError(ClassAuthorization, nil)
	}
	if mutation.kind == governancePrepare || mutation.kind == governanceTransfer {
		i := mutation.intent
		if !validRoomCreateIdentity(i.RoomID) || !validRoomCreateIdentity(i.TargetUserID) || !validRoomCreateIdentity(i.ExpectedOwnerUserID) || i.TargetUserID == i.ExpectedOwnerUserID || i.ExpectedRevision > math.MaxInt64 {
			return opPlan{}, refusedError(ClassInvalidRequest, nil)
		}
		if (mutation.kind == governancePrepare || mutation.actionID != "" || i.Reason != "") && !ValidGovernanceReason(i.Reason) {
			return opPlan{}, refusedError(ClassInvalidRequest, nil)
		}
	}
	if (mutation.kind == governanceRead || mutation.kind == governanceFinish) && !validRoomCreateIdentity(mutation.actionID) {
		return opPlan{}, refusedError(ClassInvalidRequest, nil)
	}
	if mutation.kind == governanceFinish {
		completion := mutation.completion
		if completion.Purpose != enterpriseauth.PurposeReauthenticate || !completion.FreshReauthentication || completion.CorrelationID != mutation.actionID || !validIdentity(completion.Identity) || completion.Identity.AuthTime.IsZero() {
			return opPlan{}, refusedError(ClassAuthorization, nil)
		}
	}
	key := OpKey{Namespace: "governance", Deployment: auth.Deployment, OpKind: mutation.kind, Identifier: clientID, Principal: auth.UserID}
	// The exact original browser session is part of this operation's scope.
	hash := controlHash(key, struct {
		Session, Action string
		Intent          GovernanceIntent
		Identity        enterpriseauth.VerifiedIdentity
	}{auth.SessionID, mutation.actionID, mutation.intent, mutation.completion.Identity})
	return opPlan{auth: auth, userID: auth.UserID, key: key, hash: hash, governance: &mutation}, nil
}

func (s *Store) PrepareGovernanceReauthentication(ctx context.Context, auth AuthContext, clientID string, intent GovernanceIntent) (GovernanceAction, error) {
	plan, err := s.governancePlan(auth, clientID, governanceMutation{kind: governancePrepare, intent: intent})
	if err != nil {
		return GovernanceAction{}, err
	}
	result, err := s.commitWithRestarts(ctx, plan, model.Event{})
	return governanceActionResult(result, err)
}

func (s *Store) FinishGovernanceReauthentication(ctx context.Context, auth AuthContext, completion enterpriseauth.Completion) (GovernanceAction, error) {
	plan, err := s.governancePlan(auth, completion.CorrelationID, governanceMutation{kind: governanceFinish, actionID: completion.CorrelationID, completion: completion})
	if err != nil {
		return GovernanceAction{}, err
	}
	result, err := s.commitWithRestarts(ctx, plan, model.Event{})
	return governanceActionResult(result, err)
}

func (s *Store) TransferRoomOwnership(ctx context.Context, cmd OwnershipTransferCommand) (Result, error) {
	plan, err := s.governancePlan(cmd.Auth, cmd.ClientID, governanceMutation{kind: governanceTransfer, actionID: cmd.GovernanceActionID, intent: cmd.Intent})
	if err != nil {
		return refused(ClassAuthorization, err)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) ResolveRoomOwnership(ctx context.Context, cmd OwnershipTransferCommand) (Result, error) {
	plan, err := s.governancePlan(cmd.Auth, cmd.ClientID, governanceMutation{kind: governanceTransfer, actionID: cmd.GovernanceActionID, intent: cmd.Intent})
	if err != nil {
		return refused(ClassAuthorization, err)
	}
	return s.resolveWithFallback(ctx, plan)
}

func (s *Store) ReadGovernanceAction(ctx context.Context, auth AuthContext, actionID string) (GovernanceAction, error) {
	plan, err := s.governancePlan(auth, actionID, governanceMutation{kind: governanceRead, actionID: actionID})
	if err != nil {
		return GovernanceAction{}, err
	}
	result, err := s.resolveWithFallback(ctx, plan)
	return governanceActionResult(result, err)
}

func (s *Store) ListGovernanceActions(ctx context.Context, auth AuthContext) ([]GovernanceAction, error) {
	plan, err := s.governancePlan(auth, "list", governanceMutation{kind: governanceList})
	if err != nil {
		return nil, err
	}
	result, err := s.resolveWithFallback(ctx, plan)
	if err != nil {
		return nil, err
	}
	return result.governanceActions, nil
}

func governanceActionResult(result Result, err error) (GovernanceAction, error) {
	if err != nil {
		return GovernanceAction{}, err
	}
	if result.Governance == nil || result.Governance.Action == nil {
		return GovernanceAction{}, refusedError(ClassStateInvalid, nil)
	}
	return *result.Governance.Action, nil
}

func (s *Store) readGovernanceAction(ctx context.Context, q rowQuerier, id, lock string) (governanceActionState, error) {
	d := governanceActionState{action: GovernanceAction{ID: id}}
	var ready, consumed, invalidated *time.Time
	err := q.QueryRow(ctx, `SELECT actor_user_id,session_id,room_id,target_user_id,expected_owner_user_id,expected_revision,reason,created_at,expires_at,ready_at,consumed_at,invalidated_at FROM `+s.table("governance_actions")+` WHERE action_id=$1`+lock, id).Scan(&d.actor, &d.session, &d.action.Intent.RoomID, &d.action.Intent.TargetUserID, &d.action.Intent.ExpectedOwnerUserID, &d.action.Intent.ExpectedRevision, &d.action.Intent.Reason, &d.createdAt, &d.action.ExpiresAt, &ready, &consumed, &invalidated)
	if errors.Is(err, pgx.ErrNoRows) {
		d.missing = true
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if !ValidGovernanceReason(d.action.Intent.Reason) {
		return d, &storedFactCorruption{err: errors.New("invalid stored governance intent")}
	}
	d.createdAt, d.action.ExpiresAt = d.createdAt.UTC(), d.action.ExpiresAt.UTC()
	if ready != nil {
		d.readyAt, d.action.Ready = ready.UTC(), true
	}
	if invalidated != nil {
		d.invalidatedAt = *invalidated
	}
	if consumed != nil {
		d.consumedAt = consumed.UTC()
	}
	return d, nil
}

func (s *Store) readGovernanceDependencies(ctx context.Context, q rowQuerier, plan opPlan, lock string) (*governanceDependencies, error) {
	g := plan.governance
	d := &governanceDependencies{}
	intent := g.intent
	if g.actionID != "" {
		var err error
		d.action, err = s.readGovernanceAction(ctx, q, g.actionID, lockNone)
		if err != nil {
			return nil, err
		}
		if g.kind == governanceFinish || g.kind == governanceRead {
			intent = d.action.action.Intent
		}
	}
	ids := []string{plan.userID}
	for _, id := range []string{intent.ExpectedOwnerUserID, intent.TargetUserID} {
		if id != "" && id != plan.userID && (len(ids) < 2 || id != ids[1]) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		var disabled, missing bool
		err := q.QueryRow(ctx, `SELECT disabled FROM `+s.table("users")+` WHERE user_id=$1`+lock, id).Scan(&disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = true
		} else if err != nil {
			return nil, err
		}
		if id == plan.userID {
			d.actorMissing, d.actorDisabled = missing, disabled
		}
		if id == intent.ExpectedOwnerUserID {
			d.ownerMissing, d.ownerDisabled = missing, disabled
		}
		if id == intent.TargetUserID {
			d.targetMissing, d.targetDisabled = missing, disabled
		}
	}
	err := q.QueryRow(ctx, `SELECT enabled FROM `+s.table("organization_admins")+` WHERE user_id=$1`+lock, plan.userID).Scan(&d.actorAdmin)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if g.kind == governanceFinish {
		identity := g.completion.Identity
		var uid string
		var retired bool
		err := q.QueryRow(ctx, `SELECT user_id,retired_at IS NOT NULL FROM `+s.table("external_identities")+` WHERE connection_id=$1 AND issuer=$2 AND subject=$3`+lock, identity.ConnectionID, identity.Issuer, identity.Subject).Scan(&uid, &retired)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		d.identityMatches = err == nil && !retired && uid == plan.userID
	}
	d.credential, err = s.readCredential(ctx, q, plan.auth, lock)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if intent.RoomID == "" {
			break
		}
		memberLock := lock
		if g.kind == governanceTransfer && lock != lockNone {
			memberLock = lockExclusive
		}
		var role string
		var disabled, missing bool
		err := q.QueryRow(ctx, `SELECT role,disabled FROM `+s.table("room_members")+` WHERE room_id=$1 AND user_id=$2`+memberLock, intent.RoomID, id).Scan(&role, &disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = true
		} else if err != nil {
			return nil, err
		}
		if id == plan.userID {
			d.actorRole, d.actorMemberMissing, d.actorMemberDisabled = role, missing, disabled
		}
		if id == intent.TargetUserID {
			d.targetRole, d.targetMemberMissing, d.targetMemberDisabled = role, missing, disabled
		}
	}
	if g.actionID != "" && lock != lockNone {
		actionLock := lock
		if g.kind == governanceFinish || g.kind == governanceTransfer {
			actionLock = lockExclusive
		}
		d.action, err = s.readGovernanceAction(ctx, q, g.actionID, actionLock)
		if err != nil {
			return nil, err
		}
	}
	if g.kind == governanceTransfer {
		// Ownership UPDATE is the first guard for all admission writers. Lock
		// pending approvals before the Room consistency row, so transferring
		// away and back can never revive an old Owner's unconsumed approval.
		d.approvalDigest, err = s.readCatalogArchiveApprovals(ctx, q, intent.RoomID, lock)
		if err != nil {
			return nil, err
		}
	}
	if intent.RoomID != "" {
		roomLock := lock
		if g.kind == governanceTransfer && lock != lockNone {
			roomLock = lockExclusive
		}
		err := q.QueryRow(ctx, `SELECT owner_user_id,org_id,current_seq,revision FROM `+s.table("rooms")+` WHERE room_id=$1`+roomLock, intent.RoomID).Scan(&d.owner, &d.orgID, &d.seq, &d.revision)
		if errors.Is(err, pgx.ErrNoRows) {
			d.roomMissing = true
		} else if err != nil {
			return nil, err
		}
	}
	return d, nil
}

func governanceReadAllowed(plan opPlan, d *governanceDependencies) bool {
	if d == nil || d.actorMissing || d.actorDisabled || !d.credential.allowed() {
		return false
	}
	if plan.governance.kind == governanceTransfer && plan.governance.actionID == "" {
		return !d.roomMissing && !d.actorMemberMissing && !d.actorMemberDisabled
	}
	if !d.actorAdmin {
		return false
	}
	if plan.governance.actionID != "" {
		return !d.action.missing && d.action.actor == plan.userID && d.action.session == plan.auth.SessionID
	}
	return true
}

func eligibleGovernanceTarget(intent GovernanceIntent, d *governanceDependencies) bool {
	return !d.roomMissing && d.owner == intent.ExpectedOwnerUserID && !d.ownerMissing && !d.targetMissing && !d.targetDisabled && !d.targetMemberMissing && !d.targetMemberDisabled && d.targetRole == "member"
}

func (s *Store) lookupGovernance(ctx context.Context, tx pgx.Tx, plan opPlan, d *governanceDependencies) (Result, bool, error) {
	var hash, resultHash, encoding []byte
	var version int
	err := tx.QueryRow(ctx, `SELECT intent_hash,result_hash,result_version,result_encoding FROM `+s.table("governance_operations")+` WHERE deployment_id=$1 AND actor_user_id=$2 AND op_kind=$3 AND identifier=$4`, plan.key.Deployment, plan.userID, plan.governance.kind, plan.key.Identifier).Scan(&hash, &resultHash, &version, &encoding)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(hash, plan.hash[:]) {
		result, err := controlConflict()
		return result, true, err
	}
	var receipt GovernanceReceipt
	decoder := json.NewDecoder(bytes.NewReader(encoding))
	decoder.DisallowUnknownFields()
	digest := sha256.Sum256(encoding)
	if version != 1 || !bytes.Equal(digest[:], resultHash) || decoder.Decode(&receipt) != nil || receipt.Version != 1 || receipt.Kind != plan.governance.kind {
		return Result{}, true, &storedFactCorruption{err: errors.New("invalid stored governance receipt")}
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, encoding) {
		return Result{}, true, &storedFactCorruption{err: errors.New("noncanonical governance receipt")}
	}
	var fact model.Event
	if plan.governance.kind == governanceTransfer {
		if receipt.Action != nil || receipt.RoomID != plan.governance.intent.RoomID || receipt.OwnerUserID != plan.governance.intent.TargetUserID || receipt.Revision != plan.governance.intent.ExpectedRevision+1 || receipt.Seq < 2 {
			return Result{}, true, &storedFactCorruption{err: errors.New("governance transfer receipt disagrees with its operation")}
		}
		// Read this operation's metadata-only ownership fact, never a Room
		// content snapshot. A receipt pointing to any other kind fails closed.
		fact, err = s.loadFact(ctx, tx, receipt.RoomID, receipt.Seq)
		if err != nil {
			return Result{}, true, err
		}
		change, err := model.DecodeEnterpriseOwnershipChange(fact)
		if err != nil || change.FromUserID != plan.governance.intent.ExpectedOwnerUserID || change.ToUserID != plan.governance.intent.TargetUserID || change.Governance != (plan.governance.actionID != "") {
			return Result{}, true, &storedFactCorruption{err: errors.New("stored governance fact disagrees with its operation")}
		}
	} else if receipt.Action == nil || !validRoomCreateIdentity(receipt.Action.ID) || !ValidGovernanceReason(receipt.Action.Intent.Reason) || receipt.Action.ExpiresAt.IsZero() || receipt.RoomID != "" || receipt.OwnerUserID != "" || receipt.Seq != 0 || receipt.Revision != 0 {
		return Result{}, true, &storedFactCorruption{err: errors.New("invalid governance action receipt")}
	} else if plan.governance.kind == governancePrepare && (receipt.Action.Intent != plan.governance.intent || receipt.Action.Ready) || plan.governance.kind == governanceFinish && (receipt.Action.ID != plan.governance.actionID || !receipt.Action.Ready) {
		return Result{}, true, &storedFactCorruption{err: errors.New("stored governance action disagrees with its operation")}
	}
	if plan.governance.kind == governanceFinish && (d == nil || d.action.missing || receipt.Action.Intent != d.action.action.Intent || !receipt.Action.ExpiresAt.Equal(d.action.action.ExpiresAt) || receipt.Action.Ready != d.action.action.Ready) {
		// The finish hash references this immutable original action. Digest
		// integrity alone cannot prove the returned target/rationale is that
		// action's intent. Consumption and expiry do not erase accepted results.
		return Result{}, true, &storedFactCorruption{err: errors.New("stored governance finish disagrees with its original action")}
	}
	return Result{Outcome: OutcomeReplayed, Seq: receipt.Seq, Revision: receipt.Revision, Fact: fact, Governance: &receipt}, true, nil
}

func (s *Store) resolveGovernancePlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *governanceDependencies) (Result, error) {
	if !governanceReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	if plan.governance.kind == governanceRead {
		if !d.action.action.ExpiresAt.After(time.Now()) || !d.action.consumedAt.IsZero() || !d.action.invalidatedAt.IsZero() {
			return refused(ClassAuthorization, nil)
		}
		return Result{Outcome: OutcomeReplayed, Governance: &GovernanceReceipt{Version: 1, Kind: governanceRead, Action: &d.action.action}}, nil
	}
	if plan.governance.kind == governanceList {
		rows, err := tx.Query(ctx, `SELECT action_id,room_id,target_user_id,expected_owner_user_id,expected_revision,reason,expires_at,ready_at IS NOT NULL FROM `+s.table("governance_actions")+` WHERE actor_user_id=$1 AND session_id=$2 AND consumed_at IS NULL AND invalidated_at IS NULL AND expires_at>pg_catalog.clock_timestamp() ORDER BY created_at DESC,action_id LIMIT 32`, plan.userID, plan.auth.SessionID)
		if err != nil {
			return resolveIncomplete(ctx, err)
		}
		defer rows.Close()
		actions := make([]GovernanceAction, 0)
		for rows.Next() {
			var action GovernanceAction
			if err := rows.Scan(&action.ID, &action.Intent.RoomID, &action.Intent.TargetUserID, &action.Intent.ExpectedOwnerUserID, &action.Intent.ExpectedRevision, &action.Intent.Reason, &action.ExpiresAt, &action.Ready); err != nil {
				return resolveIncomplete(ctx, err)
			}
			if !ValidGovernanceReason(action.Intent.Reason) {
				return refused(ClassStoredFactUnreadable, nil)
			}
			action.ExpiresAt = action.ExpiresAt.UTC()
			actions = append(actions, action)
		}
		if err := rows.Err(); err != nil {
			return resolveIncomplete(ctx, err)
		}
		return Result{Outcome: OutcomeReplayed, governanceActions: actions}, nil
	}
	result, found, err := s.lookupGovernance(ctx, tx, plan, d)
	if err != nil {
		if isStoredFactCorruption(err) {
			return refused(ClassStoredFactUnreadable, err)
		}
		var kernel *KernelError
		if errors.As(err, &kernel) {
			return result, err
		}
		return resolveIncomplete(ctx, err)
	}
	if !found {
		return Result{Outcome: OutcomeNotCommitted}, nil
	}
	return result, nil
}

func (s *Store) applyGovernancePlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *governanceDependencies) (Result, error) {
	if !governanceReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	result, found, err := s.lookupGovernance(ctx, tx, plan, d)
	if err != nil {
		if isStoredFactCorruption(err) {
			return refused(ClassStoredFactUnreadable, err)
		}
		var kernel *KernelError
		if errors.As(err, &kernel) {
			return result, err
		}
		return controlStatementFailure(s, ctx, err)
	}
	if found {
		return result, nil
	}
	g := plan.governance
	intent := g.intent
	if g.kind == governanceFinish {
		intent = d.action.action.Intent
	}
	if !eligibleGovernanceTarget(intent, d) {
		return refused(ClassAuthorization, nil)
	}
	if d.revision != intent.ExpectedRevision {
		return refused(ClassRevisionConflict, nil)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := GovernanceReceipt{Version: 1, Kind: g.kind}
	var fact model.Event
	switch g.kind {
	case governancePrepare:
		if !d.ownerDisabled {
			return refused(ClassAuthorization, nil)
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+s.table("governance_actions")+` WHERE actor_user_id=$1 AND session_id=$2 AND consumed_at IS NULL AND invalidated_at IS NULL AND expires_at>pg_catalog.clock_timestamp()`, plan.userID, plan.auth.SessionID).Scan(&pending); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		if pending >= 32 {
			return refused(ClassRangeExceeded, nil)
		}
		id, err := controlID("action")
		if err != nil {
			return refused(ClassStateInvalid, err)
		}
		action := GovernanceAction{ID: id, Intent: intent, ExpiresAt: now.Add(governanceTTL)}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("governance_actions")+`(action_id,actor_user_id,session_id,room_id,target_user_id,expected_owner_user_id,expected_revision,reason,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, id, plan.userID, plan.auth.SessionID, intent.RoomID, intent.TargetUserID, intent.ExpectedOwnerUserID, int64(intent.ExpectedRevision), intent.Reason, now, action.ExpiresAt); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		receipt.Action = &action
	case governanceFinish:
		if !d.ownerDisabled || !d.identityMatches || !d.action.readyAt.IsZero() || !d.action.consumedAt.IsZero() || !d.action.invalidatedAt.IsZero() || !d.action.action.ExpiresAt.After(now) || g.completion.Identity.AuthTime.Before(d.action.createdAt.Truncate(time.Second)) || g.completion.Identity.AuthTime.After(now.Add(time.Minute)) {
			return refused(ClassAuthorization, nil)
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("governance_actions")+` SET ready_at=$2 WHERE action_id=$1`, g.actionID, now); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		action := d.action.action
		action.Ready = true
		receipt.Action = &action
	case governanceTransfer:
		if g.actionID != "" {
			if !d.ownerDisabled || d.action.action.Intent != intent || !d.action.action.Ready || !d.action.action.ExpiresAt.After(now) || !d.action.consumedAt.IsZero() || !d.action.invalidatedAt.IsZero() {
				return refused(ClassAuthorization, nil)
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.table("governance_actions")+` SET consumed_at=$2 WHERE action_id=$1`, g.actionID, now); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
		} else if d.ownerDisabled || d.owner != plan.userID || d.actorRole != "owner" {
			return refused(ClassAuthorization, nil)
		}
		if d.seq >= math.MaxInt64 || d.revision >= math.MaxInt64 {
			return refused(ClassRangeExceeded, nil)
		}
		cutoff, e := approvalCutoff(ctx)
		if e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		tag, e := tx.Exec(ctx, `UPDATE `+s.table("binding_requests")+` SET status='denied' WHERE room_id=$1 AND status='approved' AND expires_at>$2`, intent.RoomID, cutoff)
		if e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		setAuditInvalidations(plan, tag.RowsAffected())
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("room_members")+` SET role='member' WHERE room_id=$1 AND user_id=$2 AND role='owner'`, intent.RoomID, intent.ExpectedOwnerUserID); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET owner_user_id=$2,current_seq=$3,revision=$4 WHERE room_id=$1`, intent.RoomID, intent.TargetUserID, int64(d.seq+1), int64(d.revision+1)); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("room_members")+` SET role='owner' WHERE room_id=$1 AND user_id=$2`, intent.RoomID, intent.TargetUserID); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		fact, err = model.NewEnterpriseOwnershipChange(intent.RoomID, d.seq+1, model.EnterpriseOwnershipChange{Version: 1, FromUserID: intent.ExpectedOwnerUserID, ToUserID: intent.TargetUserID, Governance: g.actionID != ""}, now)
		if err != nil {
			return refused(ClassInvalidFact, err)
		}
		if err := s.writeStoredRoomEvent(ctx, tx, fact, userFactOrigin(plan.userID)); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		receipt.RoomID, receipt.OwnerUserID, receipt.Seq, receipt.Revision = intent.RoomID, intent.TargetUserID, d.seq+1, d.revision+1
	default:
		return refused(ClassUnsupportedOp, nil)
	}
	encoding, err := json.Marshal(receipt)
	if err != nil || len(encoding) > maxEncodingBytes {
		return refused(ClassInvalidFact, err)
	}
	digest := sha256.Sum256(encoding)
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("governance_operations")+`(deployment_id,actor_user_id,op_kind,identifier,intent_hash,result_hash,result_version,result_encoding,committed_at) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8)`, plan.key.Deployment, plan.userID, g.kind, plan.key.Identifier, plan.hash[:], digest[:], encoding, now); err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	if err := s.writeMutationAudit(ctx, tx, plan, intent.RoomID, plan.userID, "user", "governance."+g.kind, "accepted", intent.Reason, intent.RoomID, auditCount(plan), now); err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	return Result{Outcome: OutcomeCommitted, Fact: fact, Seq: receipt.Seq, Revision: receipt.Revision, Governance: &receipt}, nil
}
