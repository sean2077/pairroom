package sqlstore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"math"
	"time"
)

func (s *Store) resolveDeviceControl(ctx context.Context, tx pgx.Tx, p opPlan, d *controlDependencies) (Result, error) {
	c := d.credential
	switch p.control.kind {
	case ControlDeviceRevoke:
		r, found, e := s.lookupControl(ctx, tx, p)
		if e != nil {
			if isStoredFactCorruption(e) {
				return refused(ClassStoredFactUnreadable, e)
			}
			var k *KernelError
			if errors.As(e, &k) {
				return r, e
			}
			return resolveIncomplete(ctx, e)
		}
		if !found {
			return Result{Outcome: OutcomeNotCommitted}, nil
		}
		if r.Control == nil || !validDeviceReceipt(p, d, *r.Control) {
			return refused(ClassStoredFactUnreadable, nil)
		}
		return r, nil
	case controlEnrollmentRead:
		x := d.device
		return Result{Outcome: OutcomeReplayed, enrollment: &DeviceEnrollmentInfo{RequestID: x.requestID, DeviceID: x.deviceID, Label: x.label, Origin: x.origin, CallbackURI: x.callbackURI, ClientState: x.clientState, Status: x.status, ExpiresAt: x.expiresAt}}, nil
	case controlDeviceAuthenticate:
		return Result{Outcome: OutcomeReplayed, device: &DeviceCredential{Auth: AuthContext{Deployment: s.cfg.DeploymentID, UserID: c.userID, DeviceGrantID: c.grantID}, DeviceID: c.deviceID, GrantID: c.grantID, ExpiresAt: c.expiresAt, RefreshExpiresAt: c.refreshExpiresAt}}, nil
	case controlBindingAuthenticate:
		return Result{Outcome: OutcomeReplayed, binding: &BindingCredential{Auth: AuthContext{Deployment: s.cfg.DeploymentID, UserID: c.userID, BindingGrantID: c.grantID}, DeviceID: c.deviceID, AgentID: c.agentID, RoomID: c.roomID, BindingID: c.bindingID, GrantID: c.grantID, Slot: string(c.slot), Generation: c.generation, ExpiresAt: c.expiresAt}}, nil
	}
	return refused(ClassUnsupportedOp, nil)
}
func (s *Store) applyDeviceControl(ctx context.Context, tx pgx.Tx, p opPlan, d *controlDependencies) (Result, error) {
	m, x, kind := p.control.device, d.device, p.control.kind
	if kind == ControlAgentRegister {
		// Credential rotation changes a verifier, not the endpoint's identity.
		// Freeze the live Device authority instead of the particular access grant.
		p.hash = controlHash(p.key, struct {
			DeviceID, SourceKey string
			Runtime             string
		}{d.credential.deviceID, m.sourceKey, string(m.runtime)})
	}
	// A predecessor never reaches ordinary idempotent replay. Recovery is an
	// explicit bounded, once-only effect with its own audit and transaction.
	if kind == ControlDeviceRotate && x.currentGrantID != m.rotation.CurrentGrantID {
		return refused("rotation_recovery_required", nil)
	}
	if kind == ControlDeviceRecover {
		if x.currentGrantID == m.rotation.CurrentGrantID {
			original := p
			original.key.OpKind = ControlDeviceRotate
			copyControl := *p.control
			copyControl.kind = ControlDeviceRotate
			original.control = &copyControl
			_, found, e := s.lookupControl(ctx, tx, original)
			if e != nil {
				return controlStatementFailure(s, ctx, e)
			}
			if !found {
				return Result{Outcome: OutcomeNotCommitted}, nil
			}
			return refused(ClassStateInvalid, nil)
		}
		if x.previousGrantID != m.rotation.CurrentGrantID || x.currentGrantID != m.rotation.Next.GrantID || x.recoveryRotationID != m.rotation.RotationID || x.recoveryHash != p.hash || x.recoveryExpired || x.recoveryUsed {
			return refused("rotation_recovery_unavailable", nil)
		}
	}
	if kind != ControlDeviceRecover {
		r, found, e := s.lookupControl(ctx, tx, p)
		if e != nil {
			if isStoredFactCorruption(e) {
				return refused(ClassStoredFactUnreadable, e)
			}
			var k *KernelError
			if errors.As(e, &k) {
				return r, e
			}
			return controlStatementFailure(s, ctx, e)
		}
		if found {
			if r.Control == nil || !validDeviceReceipt(p, d, *r.Control) {
				return refused(ClassStoredFactUnreadable, nil)
			}
			if kind == ControlDeviceComplete && !d.candidate.allowed() {
				return refused(ClassAuthorization, nil)
			}
			if kind == ControlDeviceDecision && (m.decision.Approve && (x.status != "approved" || x.codeHash != m.decision.CodeHash) || !m.decision.Approve && x.status != "denied") {
				return refused(ClassInvalidControlState, nil)
			}
			return r, nil
		}
	}
	if d.revision >= math.MaxInt64 {
		return refused(ClassRangeExceeded, nil)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	r := ControlReceipt{Version: 1, Kind: kind, Seq: d.revision + 1}
	actor := p.auth.UserID
	switch kind {
	case ControlDeviceEnrollment:
		if !x.missing {
			return refused(ClassInvalidControlState, nil)
		}
		c := m.enrollment
		expiry := now.Add(10 * time.Minute)
		_, e := tx.Exec(ctx, `INSERT INTO `+s.table("device_enrollments")+`(request_id,device_id,label,origin,callback_uri,client_state,proof_hash,grant_id,access_hash,refresh_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, c.RequestID, c.DeviceID, c.Label, c.Origin, c.CallbackURI, c.ClientState, c.ProofHash[:], c.Credential.GrantID, c.Credential.AccessHash[:], c.Credential.RefreshHash[:], now, expiry)
		if e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		r.RequestID, r.DeviceID, r.Status, r.ExpiresAt = c.RequestID, c.DeviceID, "pending", expiry
	case ControlDeviceDecision:
		if x.status != "pending" && x.status != "approved" {
			return refused(ClassInvalidControlState, nil)
		}
		status := "denied"
		var code any
		count := x.approvalCount
		if m.decision.Approve {
			if count >= 3 {
				return refused("enrollment_approval_limit", nil)
			}
			count++
			status = "approved"
			code = m.decision.CodeHash[:]
		}
		_, e := tx.Exec(ctx, `UPDATE `+s.table("device_enrollments")+` SET status=$2,approved_user_id=$3,code_hash=$4,approval_count=$5 WHERE request_id=$1`, m.requestID, status, actor, code, count)
		if e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		r.UserID, r.RequestID, r.DeviceID, r.Status, r.ExpiresAt = actor, m.requestID, x.deviceID, status, x.expiresAt
	case ControlDeviceComplete:
		if x.status != "approved" {
			return refused(ClassInvalidControlState, nil)
		}
		actor = x.ownerID
		if x.deviceMissing {
			if _, e := tx.Exec(ctx, `INSERT INTO `+s.table("devices")+`(device_id,org_id,user_id) VALUES($1,$2,$3)`, x.deviceID, d.orgID, actor); e != nil {
				return controlStatementFailure(s, ctx, e)
			}
		}
		// A deliberate approval can establish a new family, but old credentials are
		// never restored. Device identity and owner stay immutable.
		accessExpiry, refreshExpiry := now.Add(15*time.Minute), now.Add(30*24*time.Hour)
		if _, e := tx.Exec(ctx, `INSERT INTO `+s.table("device_families")+`(family_id,device_id,user_id,origin,proof_hash,restore_epoch,current_grant_id,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, x.requestID, x.deviceID, actor, x.origin, x.proofHash[:], d.restoreEpoch, x.grantID, now, refreshExpiry); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		if _, e := tx.Exec(ctx, `INSERT INTO `+s.table("device_grants")+`(grant_id,family_id,access_hash,refresh_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, x.grantID, x.requestID, x.accessHash[:], x.refreshHash[:], now, accessExpiry); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		if _, e := tx.Exec(ctx, `UPDATE `+s.table("device_enrollments")+` SET status='completed' WHERE request_id=$1`, x.requestID); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		r.UserID, r.RequestID, r.DeviceID, r.GrantID, r.Status, r.ExpiresAt, r.RefreshExpiresAt = actor, x.requestID, x.deviceID, x.grantID, "completed", accessExpiry, refreshExpiry
	case ControlDeviceRotate:
		c := m.rotation
		actor = x.ownerID
		expiry := now.Add(15 * time.Minute)
		if expiry.After(x.refreshExpiresAt) {
			expiry = x.refreshExpiresAt
		}
		if _, e := tx.Exec(ctx, `INSERT INTO `+s.table("device_grants")+`(grant_id,family_id,access_hash,refresh_hash,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, c.Next.GrantID, x.familyID, c.Next.AccessHash[:], c.Next.RefreshHash[:], now, expiry); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		if _, e := tx.Exec(ctx, `UPDATE `+s.table("device_grants")+` SET revoked_at=$2 WHERE grant_id=$1 AND revoked_at IS NULL`, c.CurrentGrantID, now); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		if _, e := tx.Exec(ctx, `UPDATE `+s.table("device_families")+` SET current_grant_id=$2,previous_grant_id=$3,recovery_rotation_id=$4,recovery_hash=$5,recovery_until=$6,recovery_used_at=NULL WHERE family_id=$1`, x.familyID, c.Next.GrantID, c.CurrentGrantID, c.RotationID, p.hash[:], now.Add(2*time.Minute)); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		r.UserID, r.DeviceID, r.GrantID, r.ExpiresAt, r.RefreshExpiresAt = actor, x.deviceID, c.Next.GrantID, expiry, x.refreshExpiresAt
	case ControlDeviceRecover:
		actor = x.ownerID
		if _, e := tx.Exec(ctx, `UPDATE `+s.table("device_families")+` SET recovery_used_at=$2 WHERE family_id=$1`, x.familyID, now); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		var expiry time.Time
		if e := tx.QueryRow(ctx, `SELECT expires_at FROM `+s.table("device_grants")+` WHERE grant_id=$1`, x.currentGrantID).Scan(&expiry); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		r.UserID, r.DeviceID, r.GrantID, r.ExpiresAt, r.RefreshExpiresAt = actor, x.deviceID, x.currentGrantID, expiry.UTC(), x.refreshExpiresAt
	case ControlAgentRegister:
		var id, runtime string
		var retired bool
		e := tx.QueryRow(ctx, `SELECT agent_id,COALESCE(runtime_kind,''),retired_at IS NOT NULL FROM `+s.table("agents")+` WHERE device_id=$1 AND stable_source_key=$2`, d.credential.deviceID, m.sourceKey).Scan(&id, &runtime, &retired)
		if errors.Is(e, pgx.ErrNoRows) {
			id = m.agentID
			if _, e = tx.Exec(ctx, `INSERT INTO `+s.table("agents")+`(agent_id,org_id,device_id,stable_source_key,runtime_kind,runtime_recorded_at) VALUES($1,$2,$3,$4,$5,$6)`, id, d.orgID, d.credential.deviceID, m.sourceKey, string(m.runtime), now); e != nil {
				return controlStatementFailure(s, ctx, e)
			}
		} else if e != nil {
			return controlStatementFailure(s, ctx, e)
		} else if retired || runtime != string(m.runtime) {
			return refused(ClassInvalidControlState, nil)
		}
		r.UserID, r.DeviceID, r.AgentID = actor, d.credential.deviceID, id
	case ControlDeviceRevoke:
		scope := authorityRevocationScope{DeviceID: x.deviceID}
		if e := s.retireAuthorityBindings(ctx, tx, scope, actor); e != nil {
			if isStoredFactCorruption(e) {
				return refused(ClassStoredFactUnreadable, e)
			}
			return controlStatementFailure(s, ctx, e)
		}
		if e := s.revokeNativeCredentials(ctx, tx, scope); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		if _, e := tx.Exec(ctx, `UPDATE `+s.table("devices")+` SET disabled=true,retired_at=COALESCE(retired_at,$2),retired_reason=COALESCE(retired_reason,'device.revoked') WHERE device_id=$1`, x.deviceID, now); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		r.UserID, r.DeviceID, r.Status = actor, x.deviceID, "revoked"
	default:
		return refused(ClassUnsupportedOp, nil)
	}
	if e := s.appendControlResult(ctx, tx, p, r, actor); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	return Result{Outcome: OutcomeCommitted, Control: &r}, nil
}

func validDeviceReceipt(p opPlan, d *controlDependencies, r ControlReceipt) bool {
	if r.Version != 1 || r.Kind != p.control.kind || r.Seq == 0 {
		return false
	}
	expected := ControlReceipt{Version: r.Version, Kind: r.Kind, Seq: r.Seq}
	switch p.control.kind {
	case ControlDeviceEnrollment:
		expected.RequestID, expected.DeviceID, expected.Status, expected.ExpiresAt = p.control.device.requestID, p.control.device.enrollment.DeviceID, "pending", d.device.expiresAt
	case ControlDeviceDecision:
		status := "denied"
		if p.control.device.decision.Approve {
			status = "approved"
		}
		expected.RequestID, expected.DeviceID, expected.UserID, expected.Status, expected.ExpiresAt = p.control.device.requestID, d.device.deviceID, p.auth.UserID, status, d.device.expiresAt
	case ControlDeviceComplete:
		expected.RequestID, expected.DeviceID, expected.UserID, expected.GrantID, expected.Status = p.control.device.requestID, d.device.deviceID, d.device.ownerID, d.device.grantID, "completed"
		expected.ExpiresAt, expected.RefreshExpiresAt = d.candidate.expiresAt, d.candidate.refreshExpiresAt
	case ControlDeviceRotate:
		expected.DeviceID, expected.UserID, expected.GrantID = p.control.device.rotation.DeviceID, d.device.ownerID, p.control.device.rotation.Next.GrantID
		expected.ExpiresAt, expected.RefreshExpiresAt = r.ExpiresAt, d.device.refreshExpiresAt
		if r.ExpiresAt.IsZero() {
			return false
		}
	case ControlAgentRegister:
		if !validRoomCreateIdentity(r.AgentID) {
			return false
		}
		expected.DeviceID, expected.UserID, expected.AgentID = d.credential.deviceID, p.auth.UserID, r.AgentID
	case ControlDeviceRevoke:
		expected.DeviceID, expected.UserID, expected.Status = p.control.device.deviceID, p.auth.UserID, "revoked"
	default:
		return false
	}
	// time.Time's JSON decoder uses UTC; compare instant values separately so a
	// driver location representation cannot change canonical semantic checking.
	if !expected.ExpiresAt.Equal(r.ExpiresAt) || !expected.RefreshExpiresAt.Equal(r.RefreshExpiresAt) {
		return false
	}
	expected.ExpiresAt, expected.RefreshExpiresAt = r.ExpiresAt, r.RefreshExpiresAt
	return expected == r
}
