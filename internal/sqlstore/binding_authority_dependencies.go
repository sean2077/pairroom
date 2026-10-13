package sqlstore

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
)

type bindingRequestState struct {
	missing, expired                       bool
	info                                   BindingRequestInfo
	orgID, familyID, restoreEpoch, grantID string
	proofHash, tokenHash                   [32]byte
}
type bindingDependencies struct {
	actorMissing, actorDisabled, actorMemberMissing, actorMemberDisabled                 bool
	requesterMissing, requesterDisabled, requesterMemberMissing, requesterMemberDisabled bool
	ownerMissing, ownerDisabled, ownerMemberMissing, ownerMemberDisabled                 bool
	actorRole, requesterRole, ownerRole                                                  string
	credential                                                                           sessionDependency
	request                                                                              bindingRequestState
	renewTarget                                                                          bindingRenewalTarget
	roomMissing, archived                                                                bool
	owner, orgID, hostMode, restoreEpoch                                                 string
	seq, revision                                                                        uint64
	agentMissing, agentRetired, requesterScopeInvalid                                    bool
	agentDevice, agentOrg, activeAgentBinding                                            string
	runtime                                                                              model.RuntimeKind
	headMissing, slotHasHistory                                                          bool
	head                                                                                 uint64
	activeBinding                                                                        string
	requesterFamilyExpiry                                                                time.Time
}

type bindingRenewalTarget struct {
	missing, retired                 bool
	agentID, deviceID, userID, orgID string
	slot                             model.ActorID
	generation                       uint64
}

func (s *Store) readBindingRequestState(ctx context.Context, q rowQuerier, requestID string) (bindingRequestState, error) {
	d := bindingRequestState{}
	var proof, token []byte
	err := q.QueryRow(ctx, `SELECT request_id,org_id,room_id,slot,observed_generation,replacement,agent_id,binding_id,grant_id,token_hash,proof_hash,park_enabled,user_id,device_id,family_id,restore_epoch,status,requested_at,expires_at,COALESCE(approved_by,''),expires_at<=pg_catalog.clock_timestamp() FROM `+s.table("binding_requests")+` WHERE request_id=$1`, requestID).Scan(&d.info.RequestID, &d.orgID, &d.info.RoomID, &d.info.Slot, &d.info.ObservedGeneration, &d.info.Replace, &d.info.AgentID, &d.info.BindingID, &d.grantID, &token, &proof, &d.info.ParkEnabled, &d.info.UserID, &d.info.DeviceID, &d.familyID, &d.restoreEpoch, &d.info.Status, &d.info.RequestedAt, &d.info.ExpiresAt, &d.info.ApprovedBy, &d.expired)
	if errors.Is(err, pgx.ErrNoRows) {
		d.missing = true
		return d, nil
	}
	if err != nil {
		return d, err
	}
	if len(proof) != 32 || len(token) != 32 {
		return d, &storedFactCorruption{err: errors.New("invalid Binding request verifier length")}
	}
	copy(d.proofHash[:], proof)
	copy(d.tokenHash[:], token)
	d.info.RequestedAt, d.info.ExpiresAt = d.info.RequestedAt.UTC(), d.info.ExpiresAt.UTC()
	return d, nil
}

// Lifecycle commands preselect ownership UPDATE; reads preselect SHARE. That
// guard stabilizes all scope joins before rows are visited in sorted User and
// membership order, then Room order. No shared guard is upgraded mid-command.
func (s *Store) readBindingDependencies(ctx context.Context, q rowQuerier, plan opPlan, lock string) (*bindingDependencies, error) {
	d := &bindingDependencies{}
	var err error
	if plan.binding.kind != bindingList && plan.binding.kind != bindingContextRead && plan.binding.kind != bindingRenew {
		d.request, err = s.readBindingRequestState(ctx, q, plan.binding.requestID)
		if err != nil {
			return nil, err
		}
	}
	if plan.binding.kind == bindingRenew {
		target := &d.renewTarget
		err = q.QueryRow(ctx, `SELECT b.agent_id,a.device_id,dev.user_id,b.org_id,b.slot,b.generation,b.retired_at IS NOT NULL FROM `+s.table("bindings")+` b JOIN `+s.table("agents")+` a USING(agent_id) JOIN `+s.table("devices")+` dev USING(device_id) WHERE b.binding_id=$1 AND b.room_id=$2`, plan.binding.renewal.BindingID, plan.key.RoomID).Scan(&target.agentID, &target.deviceID, &target.userID, &target.orgID, &target.slot, &target.generation, &target.retired)
		if errors.Is(err, pgx.ErrNoRows) {
			target.missing = true
		} else if err != nil {
			return nil, err
		}
	}
	// This initial metadata read locates the sorted authority set; it takes no
	// row lock and is rechecked by the common discovery/locked comparison.
	err = q.QueryRow(ctx, `SELECT owner_user_id FROM `+s.table("rooms")+` WHERE room_id=$1`, plan.key.RoomID).Scan(&d.owner)
	if errors.Is(err, pgx.ErrNoRows) {
		d.roomMissing = true
	} else if err != nil {
		return nil, err
	}
	requester := d.request.info.UserID
	agentID, slot := d.request.info.AgentID, d.request.info.Slot
	if plan.binding.kind == bindingRequest {
		requester = plan.userID
		agentID = plan.binding.request.AgentID
		slot = plan.binding.request.Slot
	}
	if plan.binding.kind == bindingRenew {
		requester = plan.userID
		agentID = d.renewTarget.agentID
		slot = d.renewTarget.slot
	}
	ids := []string{plan.userID}
	for _, id := range []string{requester, d.owner} {
		if id != "" && !containsBindingID(ids, id) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		var disabled, missing bool
		err = q.QueryRow(ctx, `SELECT disabled FROM `+s.table("users")+` WHERE user_id=$1`+lock, id).Scan(&disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = true
		} else if err != nil {
			return nil, err
		}
		if id == plan.userID {
			d.actorMissing, d.actorDisabled = missing, disabled
		}
		if id == requester {
			d.requesterMissing, d.requesterDisabled = missing, disabled
		}
		if id == d.owner {
			d.ownerMissing, d.ownerDisabled = missing, disabled
		}
	}
	d.credential, err = s.readCredential(ctx, q, plan.auth, lock)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		var role string
		var disabled, missing bool
		err = q.QueryRow(ctx, `SELECT role,disabled FROM `+s.table("room_members")+` WHERE room_id=$1 AND user_id=$2`+lock, plan.key.RoomID, id).Scan(&role, &disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = true
		} else if err != nil {
			return nil, err
		}
		if id == plan.userID {
			d.actorRole, d.actorMemberMissing, d.actorMemberDisabled = role, missing, disabled
		}
		if id == requester {
			d.requesterRole, d.requesterMemberMissing, d.requesterMemberDisabled = role, missing, disabled
		}
		if id == d.owner {
			d.ownerRole, d.ownerMemberMissing, d.ownerMemberDisabled = role, missing, disabled
		}
	}
	roomLock := lock
	if lock != "" && !bindingReadOnly(plan.binding.kind) {
		roomLock = lockExclusive
	}
	err = q.QueryRow(ctx, `SELECT owner_user_id,org_id,host_mode,archived_at IS NOT NULL,current_seq,revision FROM `+s.table("rooms")+` WHERE room_id=$1`+roomLock, plan.key.RoomID).Scan(&d.owner, &d.orgID, &d.hostMode, &d.archived, &d.seq, &d.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		d.roomMissing = true
	} else if err != nil {
		return nil, err
	}
	err = q.QueryRow(ctx, `SELECT restore_epoch FROM `+s.table("identity_policy")+` WHERE id`).Scan(&d.restoreEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		d.requesterScopeInvalid = true
	} else if err != nil {
		return nil, err
	}
	if agentID != "" {
		err = q.QueryRow(ctx, `SELECT device_id,org_id,COALESCE(runtime_kind,''),retired_at IS NOT NULL FROM `+s.table("agents")+` WHERE agent_id=$1`, agentID).Scan(&d.agentDevice, &d.agentOrg, &d.runtime, &d.agentRetired)
		if errors.Is(err, pgx.ErrNoRows) {
			d.agentMissing = true
		} else if err != nil {
			return nil, err
		}
		d.request.info.Runtime = d.runtime
		err = q.QueryRow(ctx, `SELECT binding_id FROM `+s.table("bindings")+` WHERE agent_id=$1 AND retired_at IS NULL`, agentID).Scan(&d.activeAgentBinding)
		if !errors.Is(err, pgx.ErrNoRows) && err != nil {
			return nil, err
		}
	}
	if slot != "" && !d.roomMissing {
		err = q.QueryRow(ctx, `SELECT current_generation,EXISTS(SELECT 1 FROM `+s.table("bindings")+` b WHERE b.room_id=h.room_id AND b.slot=h.slot) FROM `+s.table("room_slots")+` h WHERE room_id=$1 AND slot=$2`, plan.key.RoomID, slot).Scan(&d.head, &d.slotHasHistory)
		if errors.Is(err, pgx.ErrNoRows) {
			d.headMissing = true
		} else if err != nil {
			return nil, err
		}
		err = q.QueryRow(ctx, `SELECT binding_id FROM `+s.table("bindings")+` WHERE room_id=$1 AND slot=$2 AND retired_at IS NULL`, plan.key.RoomID, slot).Scan(&d.activeBinding)
		if !errors.Is(err, pgx.ErrNoRows) && err != nil {
			return nil, err
		}
	}
	if !d.request.missing && d.request.info.RequestID != "" {
		var user, device, epoch string
		var invalid bool
		err = q.QueryRow(ctx, `SELECT f.user_id,f.device_id,f.restore_epoch,f.expires_at,f.revoked_at IS NOT NULL OR f.expires_at<=pg_catalog.clock_timestamp() OR dev.disabled OR dev.retired_at IS NOT NULL OR dev.user_id<>f.user_id FROM `+s.table("device_families")+` f JOIN `+s.table("devices")+` dev USING(device_id) WHERE f.family_id=$1`, d.request.familyID).Scan(&user, &device, &epoch, &d.requesterFamilyExpiry, &invalid)
		if errors.Is(err, pgx.ErrNoRows) {
			invalid = true
		} else if err != nil {
			return nil, err
		}
		d.requesterScopeInvalid = d.requesterScopeInvalid || invalid || user != d.request.info.UserID || device != d.request.info.DeviceID || epoch != d.request.restoreEpoch || epoch != d.restoreEpoch
	}
	return d, nil
}

func containsBindingID(ids []string, id string) bool {
	for _, current := range ids {
		if current == id {
			return true
		}
	}
	return false
}

func bindingOwnerLive(plan opPlan, d *bindingDependencies) bool {
	return d.owner == plan.userID && !d.ownerMissing && !d.ownerDisabled && !d.ownerMemberMissing && !d.ownerMemberDisabled && d.ownerRole == "owner"
}
func bindingReadAllowed(plan opPlan, d *bindingDependencies) bool {
	if d == nil || d.roomMissing || d.actorMissing || d.actorDisabled || d.actorMemberMissing || d.actorMemberDisabled || !d.credential.allowed() {
		return false
	}
	if plan.binding.kind == bindingContextRead {
		return (d.credential.kind == CredentialBrowser || d.credential.kind == CredentialDevice) && (d.actorRole == "owner" || d.actorRole == "member")
	}
	if plan.auth.SessionID != "" {
		return d.credential.kind == CredentialBrowser && bindingOwnerLive(plan, d)
	}
	if d.credential.kind != CredentialDevice {
		return false
	}
	if plan.binding.kind == bindingRequest || plan.binding.kind == bindingList {
		return true
	}
	if plan.binding.kind == bindingRenew {
		target := d.renewTarget
		return !target.missing && target.userID == plan.userID && target.deviceID == d.credential.deviceID && target.generation == plan.binding.renewal.Generation && target.orgID == d.orgID
	}
	return !d.request.missing && d.request.info.RoomID == plan.key.RoomID && d.request.info.UserID == plan.userID && d.request.info.DeviceID == d.credential.deviceID && d.request.familyID == d.credential.familyID && !d.requesterScopeInvalid
}
func bindingRequesterEligible(d *bindingDependencies) bool {
	return !d.requesterMissing && !d.requesterDisabled && !d.requesterMemberMissing && !d.requesterMemberDisabled && (d.requesterRole == "owner" || d.requesterRole == "member") && !d.requesterScopeInvalid
}
