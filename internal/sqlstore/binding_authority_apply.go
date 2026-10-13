package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func (s *Store) applyBindingPlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *bindingDependencies) (Result, error) {
	if !bindingReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	plan.hash = bindingIntentHash(plan, d)
	result, found, err := s.lookupBindingOperation(ctx, tx, plan, d)
	if err != nil {
		return bindingOperationFailure(s, ctx, result, err, false)
	}
	if found {
		return result, nil
	}
	if d.archived && plan.binding.kind != bindingRenew || d.hostMode != string(model.HostNative) || d.seq < 1 {
		return refused(ClassInvalidControlState, nil)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := BindingAuthorityReceipt{Version: 1, Kind: plan.binding.kind}
	var facts []model.Event
	switch plan.binding.kind {
	case bindingRequest:
		c := plan.binding.request
		if !d.request.missing {
			return refused(ClassPayloadConflict, nil)
		}
		if !bindingRequesterEligible(d) || d.agentMissing || d.agentRetired || d.agentDevice != d.credential.deviceID || d.agentOrg != d.orgID || !d.runtime.Valid() || d.runtime != d.runtime.Canonical() {
			return refused(ClassAuthorization, nil)
		}
		if !bindingSlotMatches(c.ObservedGeneration, c.Replace, d) {
			return refused(ClassInvalidControlState, nil)
		}
		if d.activeAgentBinding != "" && (!c.Replace || d.activeAgentBinding != d.activeBinding) {
			return refused(ClassInvalidControlState, nil)
		}
		var pending int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+s.table("binding_requests")+` WHERE device_id=$1 AND status IN ('pending','approved') AND expires_at>pg_catalog.clock_timestamp()`, d.credential.deviceID).Scan(&pending); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		if pending >= 32 {
			return refused(ClassRangeExceeded, nil)
		}
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("bindings")+` WHERE binding_id=$1) OR EXISTS(SELECT 1 FROM `+s.table("binding_grants")+` WHERE grant_id=$2 OR token_hash=$3)`, c.BindingID, c.Grant.GrantID, c.Grant.TokenHash[:]).Scan(&used); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		if used {
			return refused(ClassInvalidControlState, nil)
		}
		info := BindingRequestInfo{RequestID: c.RequestID, RoomID: c.RoomID, Slot: c.Slot, ObservedGeneration: c.ObservedGeneration, Replace: c.Replace, AgentID: c.AgentID, Runtime: d.runtime, BindingID: c.BindingID, UserID: plan.userID, DeviceID: d.credential.deviceID, ParkEnabled: c.ParkEnabled, Status: "pending", RequestedAt: now, ExpiresAt: now.Add(bindingRequestTTL)}
		_, err = tx.Exec(ctx, `INSERT INTO `+s.table("binding_requests")+`(request_id,org_id,room_id,slot,observed_generation,replacement,agent_id,binding_id,grant_id,token_hash,proof_hash,park_enabled,user_id,device_id,family_id,restore_epoch,status,requested_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'pending',$17,$18)`, c.RequestID, d.orgID, c.RoomID, string(c.Slot), int64(c.ObservedGeneration), c.Replace, c.AgentID, c.BindingID, c.Grant.GrantID, c.Grant.TokenHash[:], c.ProofHash[:], c.ParkEnabled, plan.userID, d.credential.deviceID, d.credential.familyID, d.restoreEpoch, now, info.ExpiresAt)
		if err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		receipt.Request = &info
	case bindingDecide:
		if d.request.missing || d.request.info.RoomID != plan.key.RoomID || d.request.info.Status != "pending" || d.request.expired {
			return refused(ClassInvalidControlState, nil)
		}
		if !bindingRequesterEligible(d) || d.agentMissing || d.agentRetired || d.agentDevice != d.request.info.DeviceID || d.agentOrg != d.orgID {
			return refused(ClassAuthorization, nil)
		}
		if !bindingSlotMatches(d.request.info.ObservedGeneration, d.request.info.Replace, d) {
			return refused(ClassInvalidControlState, nil)
		}
		info := d.request.info
		info.Status = "denied"
		if plan.binding.approve {
			info.Status = "approved"
		}
		info.ApprovedBy = plan.userID
		if _, err = tx.Exec(ctx, `UPDATE `+s.table("binding_requests")+` SET status=$2,approved_by=$3,decided_at=$4 WHERE request_id=$1`, info.RequestID, info.Status, plan.userID, now); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		receipt.Request = &info
	case bindingComplete:
		if d.request.missing || d.request.info.RoomID != plan.key.RoomID || d.request.info.Status != "approved" || d.request.expired || d.request.proofHash != plan.binding.proofHash {
			return refused(ClassAuthorization, nil)
		}
		// Owner admission and endpoint consent are independent, live predicates.
		// A later transfer/disable invalidates pending approval, never an already
		// completed Member Binding that remains authorized by its own live ACL.
		if !bindingRequesterEligible(d) || d.owner != d.request.info.ApprovedBy || d.ownerMissing || d.ownerDisabled || d.ownerMemberMissing || d.ownerMemberDisabled || d.ownerRole != "owner" || d.agentMissing || d.agentRetired || d.agentDevice != d.credential.deviceID || d.agentOrg != d.orgID {
			return refused(ClassAuthorization, nil)
		}
		if !bindingSlotMatches(d.request.info.ObservedGeneration, d.request.info.Replace, d) || d.activeAgentBinding != "" && (!d.request.info.Replace || d.activeAgentBinding != d.activeBinding) {
			return refused(ClassInvalidControlState, nil)
		}
		facts, err = s.completeBindingAuthority(ctx, tx, plan, d, now, &receipt)
		if err != nil {
			return bindingOperationFailure(s, ctx, Result{}, err, false)
		}
	case bindingRenew:
		if err := s.renewBindingAuthority(ctx, tx, plan, d, now, &receipt); err != nil {
			return bindingOperationFailure(s, ctx, Result{}, err, false)
		}
	default:
		return refused(ClassUnsupportedOp, nil)
	}
	if err := validateBindingAuthorityReceipt(receipt); err != nil {
		return refused(ClassInvalidFact, err)
	}
	encoding, err := json.Marshal(receipt)
	if err != nil {
		return refused(ClassInvalidFact, err)
	}
	digest := sha256.Sum256(encoding)
	if _, err = tx.Exec(ctx, `INSERT INTO `+s.table("binding_operations")+`(deployment_id,room_id,principal,op_kind,identifier,intent_hash,result_version,result_encoding,result_hash,result_seq,result_revision,accepted_at) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9,$10,$11)`, plan.key.Deployment, plan.key.RoomID, plan.userID, plan.binding.kind, plan.key.Identifier, plan.hash[:], encoding, digest[:], int64(receipt.Seq), int64(receipt.Revision), now); err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO `+s.table("audit")+`(deployment_id,room_id,user_id,kind,outcome,at) VALUES($1,$2,$3,$4,'accepted',$5)`, plan.key.Deployment, plan.key.RoomID, plan.userID, "binding."+plan.binding.kind, now); err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	result = Result{Outcome: OutcomeCommitted, BindingAuthority: &receipt, Seq: receipt.Seq, Revision: receipt.Revision, Facts: facts}
	if len(facts) > 0 {
		result.Fact = facts[len(facts)-1]
	}
	return result, nil
}

func bindingSlotMatches(observed uint64, replacement bool, d *bindingDependencies) bool {
	return !d.headMissing && observed == d.head && replacement == (d.activeBinding != "") && (!d.slotHasHistory || d.head < math.MaxInt64)
}

func (s *Store) completeBindingAuthority(ctx context.Context, tx pgx.Tx, plan opPlan, d *bindingDependencies, at time.Time, receipt *BindingAuthorityReceipt) ([]model.Event, error) {
	request := d.request
	snapshot, err := s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: plan.key.RoomID, DeploymentID: s.cfg.DeploymentID, OrgID: d.orgID, HostMode: d.hostMode, Seq: d.seq, Revision: d.revision})
	if err != nil {
		return nil, err
	}
	var facts []model.Event
	if d.activeBinding != "" {
		id, err := controlID("evt")
		if err != nil {
			return nil, err
		}
		prepare, err := relay.PrepareEnterpriseBindingRetirement(toRelaySnapshot(snapshot), d.activeBinding, "binding.replaced", id)
		if err != nil {
			return nil, &storedFactCorruption{err: err}
		}
		fact, err := prepare(at)
		if err != nil {
			return nil, &storedFactCorruption{err: err}
		}
		facts = append(facts, fact)
	}
	generation := d.head
	if d.slotHasHistory {
		generation++
	}
	identity := model.EnterpriseBinding{Version: 1, RoomID: plan.key.RoomID, Slot: request.info.Slot, BindID: request.info.BindingID, Generation: generation, AgentID: request.info.AgentID, Runtime: d.runtime, Active: true, ParkEnabled: request.info.ParkEnabled}
	fact, err := model.NewEnterpriseBindingUpdate(identity, at)
	if err != nil {
		return nil, err
	}
	facts = append(facts, fact)
	if d.seq > math.MaxInt64-uint64(len(facts)) || d.revision >= math.MaxInt64 {
		return nil, refusedError(ClassRangeExceeded, nil)
	}
	for i := range facts {
		facts[i].Seq = d.seq + uint64(i) + 1
	}
	projection := toRelaySnapshot(snapshot)
	projection.Events = append(projection.Events, facts...)
	projection.Seq += uint64(len(facts))
	projection.Revision++
	if _, err := relay.ProjectEnterpriseSnapshot(projection); err != nil {
		return nil, &storedFactCorruption{err: err}
	}
	if d.activeBinding != "" {
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("binding_grants")+` SET revoked_at=COALESCE(revoked_at,$2) WHERE binding_id=$1`, d.activeBinding, at); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("bindings")+` SET retired_at=$2,retired_reason='binding.replaced' WHERE binding_id=$1 AND retired_at IS NULL`, d.activeBinding, at); err != nil {
			return nil, err
		}
	}
	if d.slotHasHistory {
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("slot_generations")+`(org_id,room_id,slot,generation,created_at) VALUES($1,$2,$3,$4,$5)`, d.orgID, plan.key.RoomID, string(identity.Slot), int64(generation), at); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("room_slots")+` SET current_generation=$3 WHERE room_id=$1 AND slot=$2`, plan.key.RoomID, string(identity.Slot), int64(generation)); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("bindings")+`(binding_id,org_id,room_id,slot,generation,agent_id,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, identity.BindID, d.orgID, plan.key.RoomID, string(identity.Slot), int64(generation), identity.AgentID, at); err != nil {
		return nil, err
	}
	expires := at.Add(bindingGrantTTL)
	if expires.After(d.requesterFamilyExpiry) {
		expires = d.requesterFamilyExpiry
	}
	if !expires.After(at) {
		return nil, refusedError(ClassAuthorization, nil)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("binding_grants")+`(grant_id,binding_id,family_id,token_hash,restore_epoch,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, request.grantID, identity.BindID, request.familyID, request.tokenHash[:], request.restoreEpoch, at, expires); err != nil {
		return nil, err
	}
	for _, event := range facts {
		if err := s.writeStoredRoomEvent(ctx, tx, event, userFactOrigin(plan.userID)); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET current_seq=$2,revision=$3 WHERE room_id=$1`, plan.key.RoomID, int64(projection.Seq), int64(projection.Revision)); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.table("binding_requests")+` SET status='consumed',consumed_at=$2 WHERE request_id=$1`, request.info.RequestID, at); err != nil {
		return nil, err
	}
	info := request.info
	info.Status = "consumed"
	receipt.Request, receipt.Binding, receipt.GrantID, receipt.ExpiresAt, receipt.Seq, receipt.Revision = &info, &identity, request.grantID, expires, projection.Seq, projection.Revision
	return facts, nil
}

func bindingOperationFailure(s *Store, ctx context.Context, result Result, err error, resolving bool) (Result, error) {
	if isStoredFactCorruption(err) {
		return refused(ClassStoredFactUnreadable, err)
	}
	var kernel *KernelError
	if errors.As(err, &kernel) {
		if !resolving && kernel.Outcome == OutcomeUnknown {
			// A snapshot read interrupted before COMMIT cannot have committed
			// this operation. Only read-only resolution retains Unknown here.
			return controlStatementFailure(s, ctx, kernel.Err)
		}
		if result.Outcome == "" {
			result = Result{Outcome: kernel.Outcome, Class: kernel.Class, Err: kernel}
		}
		return result, err
	}
	if resolving {
		return resolveIncomplete(ctx, err)
	}
	return controlStatementFailure(s, ctx, err)
}

func (s *Store) lookupBindingOperation(ctx context.Context, tx pgx.Tx, plan opPlan, d *bindingDependencies) (Result, bool, error) {
	var hash, encoding, resultHash []byte
	var version int
	var seq, revision uint64
	err := tx.QueryRow(ctx, `SELECT intent_hash,result_version,result_encoding,result_hash,result_seq,result_revision FROM `+s.table("binding_operations")+` WHERE deployment_id=$1 AND room_id=$2 AND principal=$3 AND op_kind=$4 AND identifier=$5`, plan.key.Deployment, plan.key.RoomID, plan.userID, plan.binding.kind, plan.key.Identifier).Scan(&hash, &version, &encoding, &resultHash, &seq, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(hash, plan.hash[:]) {
		r, e := controlConflict()
		return r, true, e
	}
	var receipt BindingAuthorityReceipt
	decoder := json.NewDecoder(bytes.NewReader(encoding))
	decoder.DisallowUnknownFields()
	digest := sha256.Sum256(encoding)
	if version != 1 || len(encoding) > maxEncodingBytes || !bytes.Equal(digest[:], resultHash) || decoder.Decode(&receipt) != nil || receipt.Kind != plan.binding.kind || receipt.Seq != seq || receipt.Revision != revision || validateBindingAuthorityReceipt(receipt) != nil {
		return Result{}, true, &storedFactCorruption{err: errors.New("invalid stored Binding operation receipt")}
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, encoding) {
		return Result{}, true, &storedFactCorruption{err: errors.New("binding receipt is not canonical")}
	}
	if receipt.Kind == bindingRenew {
		if err := s.validateBindingRenewalReplay(ctx, tx, plan, d, receipt); err != nil {
			return Result{}, true, err
		}
		return Result{Outcome: OutcomeReplayed, BindingAuthority: &receipt, Seq: seq, Revision: revision}, true, nil
	}
	if receipt.Request.RoomID != plan.key.RoomID || receipt.Request.RequestID != plan.binding.requestID {
		return Result{}, true, &storedFactCorruption{err: errors.New("binding receipt disagrees with its frozen operation")}
	}
	if d.request.missing || receipt.Request.AgentID != d.request.info.AgentID || receipt.Request.BindingID != d.request.info.BindingID || receipt.Request.DeviceID != d.request.info.DeviceID || receipt.Request.UserID != d.request.info.UserID || receipt.Request.Slot != d.request.info.Slot || receipt.Request.ObservedGeneration != d.request.info.ObservedGeneration || receipt.Request.Replace != d.request.info.Replace || receipt.Request.Runtime != d.runtime || receipt.Request.ParkEnabled != d.request.info.ParkEnabled || !receipt.Request.RequestedAt.Equal(d.request.info.RequestedAt) || !receipt.Request.ExpiresAt.Equal(d.request.info.ExpiresAt) {
		return Result{}, true, &storedFactCorruption{err: errors.New("binding receipt disagrees with its immutable request")}
	}
	if receipt.Kind == bindingDecide {
		expectedStatus := "denied"
		if plan.binding.approve {
			expectedStatus = "approved"
		}
		if receipt.Request.ApprovedBy != plan.userID || receipt.Request.ApprovedBy != d.request.info.ApprovedBy || receipt.Request.Status != expectedStatus {
			return Result{}, true, &storedFactCorruption{err: errors.New("binding receipt disagrees with its original Owner decision")}
		}
	}
	result := Result{Outcome: OutcomeReplayed, BindingAuthority: &receipt, Seq: seq, Revision: revision}
	if receipt.Kind == bindingComplete {
		if receipt.GrantID != d.request.grantID || receipt.Request.ApprovedBy != d.request.info.ApprovedBy {
			return Result{}, true, &storedFactCorruption{err: errors.New("binding receipt changed grant identity")}
		}
		first := seq
		if receipt.Request.Replace {
			first--
		}
		for current := first; current <= seq; current++ {
			fact, err := s.loadFact(ctx, tx, plan.key.RoomID, current)
			if err != nil {
				return Result{}, true, err
			}
			identity, err := model.DecodeEnterpriseBindingUpdate(fact)
			if err != nil {
				return Result{}, true, &storedFactCorruption{err: err}
			}
			if current == seq {
				expected, _ := json.Marshal(receipt.Binding)
				actual, _ := json.Marshal(identity)
				if !bytes.Equal(expected, actual) {
					return Result{}, true, &storedFactCorruption{err: errors.New("binding receipt differs from original fact")}
				}
			} else if identity.Active || identity.Slot != receipt.Request.Slot || identity.Generation != receipt.Request.ObservedGeneration || identity.RetiredReason != "binding.replaced" {
				return Result{}, true, &storedFactCorruption{err: errors.New("invalid original replacement retirement")}
			}
			result.Facts = append(result.Facts, fact)
		}
		result.Fact = result.Facts[len(result.Facts)-1]
	}
	return result, true, nil
}

func validateBindingAuthorityReceipt(r BindingAuthorityReceipt) error {
	if r.Kind == bindingRenew {
		return validateBindingRenewalReceipt(r)
	}
	i := r.Request
	if r.Version != 1 || i == nil || !validRoomCreateIdentity(i.RequestID) || !validRoomCreateIdentity(i.RoomID) || !i.Slot.ValidParticipant() || i.ObservedGeneration == 0 || i.ObservedGeneration > math.MaxInt64 || !validRoomCreateIdentity(i.AgentID) || !validRoomCreateIdentity(i.BindingID) || !validRoomCreateIdentity(i.UserID) || !validRoomCreateIdentity(i.DeviceID) || !i.Runtime.Valid() || i.Runtime != i.Runtime.Canonical() || i.RequestedAt.IsZero() || !i.ExpiresAt.After(i.RequestedAt) || i.ExpiresAt.After(i.RequestedAt.Add(bindingRequestTTL)) {
		return errors.New("invalid Binding request result")
	}
	if r.Kind == bindingRequest {
		if i.Status != "pending" || i.ApprovedBy != "" {
			return errors.New("invalid pending Binding result")
		}
	} else if r.Kind == bindingDecide {
		if i.Status != "approved" && i.Status != "denied" || !validRoomCreateIdentity(i.ApprovedBy) {
			return errors.New("invalid Binding decision result")
		}
	} else if r.Kind != bindingComplete {
		return errors.New("unsupported Binding result kind")
	}
	if r.Kind != bindingComplete {
		if r.Binding != nil || r.GrantID != "" || !r.ExpiresAt.IsZero() || r.Seq != 0 || r.Revision != 0 {
			return errors.New("binding approval carries unrelated authority")
		}
		return nil
	}
	if i.Status != "consumed" || !validRoomCreateIdentity(i.ApprovedBy) || r.Binding == nil || !r.Binding.Active || !validRoomCreateIdentity(r.GrantID) || r.ExpiresAt.IsZero() || r.Seq < 2 || r.Seq > math.MaxInt64 || r.Revision == 0 || r.Revision > math.MaxInt64 {
		return errors.New("invalid completed Binding result")
	}
	b := r.Binding
	if b.RoomID != i.RoomID || b.Slot != i.Slot || b.BindID != i.BindingID || b.AgentID != i.AgentID || b.Runtime != i.Runtime || b.ParkEnabled != i.ParkEnabled || b.Generation < i.ObservedGeneration || b.Generation > i.ObservedGeneration+1 || i.Replace && b.Generation != i.ObservedGeneration+1 {
		return errors.New("binding result changed frozen request identity")
	}
	data, _ := json.Marshal(b)
	_, err := model.DecodeEnterpriseBindingUpdate(model.Event{ID: "validate-binding-result", RoomID: b.RoomID, Actor: b.Slot, Kind: model.EnterpriseBindingUpdatedKind, CreatedAt: i.RequestedAt, Data: data})
	return err
}
