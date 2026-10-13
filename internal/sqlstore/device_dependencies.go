package sqlstore

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type deviceDependencies struct {
	missing, expired, deviceMissing, deviceDisabled, deviceRetired, revoked, epochMismatch, recoveryUsed, recoveryExpired                                 bool
	requestID, deviceID, label, origin, callbackURI, clientState, status, ownerID, grantID, familyID, currentGrantID, previousGrantID, recoveryRotationID string
	proofHash, codeHash, accessHash, refreshHash, recoveryHash                                                                                            [32]byte
	approvalCount                                                                                                                                         int
	expiresAt, accessExpiresAt, refreshExpiresAt                                                                                                          time.Time
}

func hashFromDB(dst *[32]byte, src []byte) error {
	if len(src) != 32 {
		return &storedFactCorruption{err: errors.New("stored native verifier length is invalid")}
	}
	copy(dst[:], src)
	return nil
}
func (s *Store) readEnrollment(ctx context.Context, q rowQuerier, id, lock string) (deviceDependencies, error) {
	d := deviceDependencies{requestID: id}
	var proof, code, access, refresh []byte
	e := q.QueryRow(ctx, `SELECT device_id,label,origin,callback_uri,client_state,status,COALESCE(approved_user_id,''),grant_id,proof_hash,code_hash,access_hash,refresh_hash,approval_count,expires_at,expires_at<=pg_catalog.clock_timestamp() FROM `+s.table("device_enrollments")+` WHERE request_id=$1`+lock, id).Scan(&d.deviceID, &d.label, &d.origin, &d.callbackURI, &d.clientState, &d.status, &d.ownerID, &d.grantID, &proof, &code, &access, &refresh, &d.approvalCount, &d.expiresAt, &d.expired)
	if errors.Is(e, pgx.ErrNoRows) {
		d.missing = true
		return d, nil
	}
	if e != nil {
		return d, e
	}
	for _, v := range []struct {
		dst *[32]byte
		src []byte
	}{{&d.proofHash, proof}, {&d.accessHash, access}, {&d.refreshHash, refresh}} {
		if e := hashFromDB(v.dst, v.src); e != nil {
			return d, e
		}
	}
	if code != nil {
		if e := hashFromDB(&d.codeHash, code); e != nil {
			return d, e
		}
	}
	d.expiresAt = d.expiresAt.UTC()
	return d, nil
}
func (s *Store) readRotation(ctx context.Context, q rowQuerier, cmd *DeviceRotationCommand, lock string) (deviceDependencies, error) {
	d := deviceDependencies{}
	var proof, refresh, recovery []byte
	clause := ""
	if lock != "" {
		clause = lock + " OF f,g,d"
	}
	e := q.QueryRow(ctx, `SELECT f.device_id,f.user_id,f.family_id,f.origin,f.current_grant_id,COALESCE(f.previous_grant_id,''),COALESCE(f.recovery_rotation_id,''),f.proof_hash,g.refresh_hash,f.recovery_hash,f.expires_at,g.expires_at,f.expires_at<=pg_catalog.clock_timestamp(),f.revoked_at IS NOT NULL OR (g.revoked_at IS NOT NULL AND f.current_grant_id=g.grant_id),d.disabled,d.retired_at IS NOT NULL,f.restore_epoch<>p.restore_epoch,f.recovery_used_at IS NOT NULL,COALESCE(f.recovery_until<=pg_catalog.clock_timestamp(),true) FROM `+s.table("device_grants")+` g JOIN `+s.table("device_families")+` f USING(family_id) JOIN `+s.table("devices")+` d USING(device_id) CROSS JOIN `+s.table("identity_policy")+` p WHERE g.grant_id=$1 AND d.user_id=f.user_id`+clause, cmd.CurrentGrantID).Scan(&d.deviceID, &d.ownerID, &d.familyID, &d.origin, &d.currentGrantID, &d.previousGrantID, &d.recoveryRotationID, &proof, &refresh, &recovery, &d.refreshExpiresAt, &d.accessExpiresAt, &d.expired, &d.revoked, &d.deviceDisabled, &d.deviceRetired, &d.epochMismatch, &d.recoveryUsed, &d.recoveryExpired)
	if errors.Is(e, pgx.ErrNoRows) {
		d.missing = true
		return d, nil
	}
	if e != nil {
		return d, e
	}
	if e = hashFromDB(&d.proofHash, proof); e != nil {
		return d, e
	}
	if e = hashFromDB(&d.refreshHash, refresh); e != nil {
		return d, e
	}
	if recovery != nil {
		if e = hashFromDB(&d.recoveryHash, recovery); e != nil {
			return d, e
		}
	}
	d.accessExpiresAt = d.accessExpiresAt.UTC()
	d.refreshExpiresAt = d.refreshExpiresAt.UTC()
	return d, nil
}

func (s *Store) readDeviceControlDependencies(ctx context.Context, q rowQuerier, plan opPlan, lock string, d *controlDependencies) (*controlDependencies, error) {
	m := plan.control.device
	kind := plan.control.kind
	actor := plan.auth.UserID
	var err error
	if m.requestID != "" {
		d.device, err = s.readEnrollment(ctx, q, m.requestID, lockNone)
		if err != nil {
			return nil, err
		}
		if kind == ControlDeviceComplete {
			actor = d.device.ownerID
		}
	}
	if m.rotation != nil {
		d.device, err = s.readRotation(ctx, q, m.rotation, lockNone)
		if err != nil {
			return nil, err
		}
		actor = d.device.ownerID
	}
	lookupAuth := plan.auth
	if kind == controlDeviceAuthenticate || kind == controlBindingAuthenticate {
		var id string
		if kind == controlDeviceAuthenticate {
			err = q.QueryRow(ctx, `SELECT g.grant_id,f.user_id FROM `+s.table("device_grants")+` g JOIN `+s.table("device_families")+` f USING(family_id) WHERE g.access_hash=$1`, m.tokenHash[:]).Scan(&id, &actor)
			lookupAuth.DeviceGrantID = id
		} else {
			err = q.QueryRow(ctx, `SELECT g.grant_id,f.user_id FROM `+s.table("binding_grants")+` g JOIN `+s.table("device_families")+` f USING(family_id) WHERE g.token_hash=$1`, m.tokenHash[:]).Scan(&id, &actor)
			lookupAuth.BindingGrantID = id
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		lookupAuth.UserID = actor
		lookupAuth.Deployment = s.cfg.DeploymentID
	}
	if actor != "" {
		err = q.QueryRow(ctx, `SELECT disabled FROM `+s.table("users")+` WHERE user_id=$1`+lock, actor).Scan(&d.actorDisabled)
		d.actorMissing = errors.Is(err, pgx.ErrNoRows)
		if err != nil && !d.actorMissing {
			return nil, err
		}
		err = q.QueryRow(ctx, `SELECT enabled FROM `+s.table("organization_admins")+` WHERE user_id=$1`+lock, actor).Scan(&d.actorAdmin)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	} else {
		d.actorMissing = true
	}
	if plan.auth.UserID != "" || kind == controlDeviceAuthenticate || kind == controlBindingAuthenticate {
		d.credential, err = s.readCredential(ctx, q, lookupAuth, lock)
		if err != nil {
			return nil, err
		}
		d.credential.required = true
	}
	exclusive := lockNone
	if lock != "" {
		exclusive = lockExclusive
		if controlReadOnly(kind) {
			exclusive = lockShare
		}
	}
	if m.requestID != "" && lock != "" {
		d.device, err = s.readEnrollment(ctx, q, m.requestID, exclusive)
		if err != nil {
			return nil, err
		}
	}
	if m.rotation != nil && lock != "" {
		d.device, err = s.readRotation(ctx, q, m.rotation, exclusive)
		if err != nil {
			return nil, err
		}
	}
	if kind == ControlDeviceComplete && d.device.deviceID != "" {
		var owner string
		err = q.QueryRow(ctx, `SELECT user_id,disabled,retired_at IS NOT NULL FROM `+s.table("devices")+` WHERE device_id=$1`+exclusive, d.device.deviceID).Scan(&owner, &d.device.deviceDisabled, &d.device.deviceRetired)
		d.device.deviceMissing = errors.Is(err, pgx.ErrNoRows)
		if err != nil && !d.device.deviceMissing {
			return nil, err
		}
		if !d.device.deviceMissing && owner != d.device.ownerID {
			d.device.deviceDisabled = true
		}
		if d.device.status == "completed" {
			d.candidate, err = s.readCredential(ctx, q, AuthContext{UserID: d.device.ownerID, DeviceGrantID: d.device.grantID}, lock)
			if err != nil {
				return nil, err
			}
		}
	}
	if kind == ControlDeviceRevoke {
		d.device.deviceID = m.deviceID
		err = q.QueryRow(ctx, `SELECT user_id,disabled,retired_at IS NOT NULL FROM `+s.table("devices")+` WHERE device_id=$1`+exclusive, m.deviceID).Scan(&d.device.ownerID, &d.device.deviceDisabled, &d.device.deviceRetired)
		d.device.deviceMissing = errors.Is(err, pgx.ErrNoRows)
		if err != nil && !d.device.deviceMissing {
			return nil, err
		}
		d.revocationDigest, err = s.readAuthorityRevocation(ctx, q, authorityRevocationScope{DeviceID: m.deviceID}, lock)
		if err != nil {
			return nil, err
		}
	}
	if lock != "" && !d.missing {
		guard := lockExclusive
		if controlReadOnly(kind) {
			guard = lockShare
		}
		if err = q.QueryRow(ctx, `SELECT revision FROM `+s.table("identity_policy")+` WHERE id`+guard).Scan(&d.revision); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (s *Store) deviceReadAllowed(plan opPlan, d *controlDependencies) bool {
	if d.missing || d.orgDisabled {
		return false
	}
	x := d.device
	m := plan.control.device
	liveActor := !d.actorMissing && !d.actorDisabled
	liveBrowser := liveActor && d.credential.allowed() && d.credential.kind == CredentialBrowser
	switch plan.control.kind {
	case ControlDeviceEnrollment:
		return x.missing || !x.expired && x.proofHash == m.enrollment.ProofHash
	case controlEnrollmentRead, ControlDeviceDecision:
		return liveBrowser && !x.missing && !x.expired && (x.ownerID == "" || x.ownerID == plan.auth.UserID)
	case ControlDeviceComplete:
		return liveActor && !x.missing && !x.expired && !x.deviceDisabled && !x.deviceRetired && x.origin == m.completion.Origin && x.proofHash == m.completion.ProofHash && x.codeHash == m.completion.CodeHash && (x.status == "approved" || x.status == "completed")
	case ControlDeviceRotate, ControlDeviceRecover:
		return liveActor && !x.missing && !x.expired && !x.revoked && !x.deviceDisabled && !x.deviceRetired && !x.epochMismatch && x.deviceID == m.rotation.DeviceID && x.origin == m.rotation.Origin && x.proofHash == m.rotation.ClientProofHash && x.refreshHash == m.rotation.CurrentRefreshHash
	case ControlDeviceRevoke:
		return liveBrowser && !x.deviceMissing && (d.actorAdmin || x.ownerID == plan.auth.UserID)
	case ControlAgentRegister:
		return liveActor && d.credential.allowed() && d.credential.kind == CredentialDevice
	case controlDeviceAuthenticate:
		return liveActor && d.credential.allowed() && d.credential.kind == CredentialDevice && d.credential.origin == m.origin
	case controlBindingAuthenticate:
		return liveActor && d.credential.allowed() && d.credential.kind == CredentialBinding && d.credential.origin == m.origin
	}
	return false
}
