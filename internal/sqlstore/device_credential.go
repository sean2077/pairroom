package sqlstore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

// Ownership SHARE is held before this read; all lifecycle mutations take its
// UPDATE guard up front. Scope rows cannot change underneath these predicates.
// Only the credential row is locked here, avoiding a later slot-lock upgrade.
func (s *Store) readNativeCredential(ctx context.Context, q rowQuerier, auth AuthContext, lock string) (sessionDependency, error) {
	d := sessionDependency{required: true, provided: true, kind: CredentialDevice, grantID: auth.DeviceGrantID}
	grantLock := ""
	if lock != "" {
		grantLock = lock + " OF g"
	}
	var epoch, currentEpoch string
	var err error
	if auth.DeviceGrantID != "" {
		err = q.QueryRow(ctx, `SELECT f.user_id,f.device_id,f.family_id,f.origin,g.expires_at,f.expires_at,g.revoked_at IS NOT NULL OR f.revoked_at IS NOT NULL OR d.disabled OR d.retired_at IS NOT NULL OR f.current_grant_id<>g.grant_id OR d.user_id<>f.user_id,g.expires_at<=pg_catalog.clock_timestamp() OR f.expires_at<=pg_catalog.clock_timestamp(),f.restore_epoch,p.restore_epoch,o.disabled FROM `+s.table("device_grants")+` g JOIN `+s.table("device_families")+` f USING(family_id) JOIN `+s.table("devices")+` d USING(device_id) CROSS JOIN `+s.table("identity_policy")+` p JOIN `+s.table("configured_org")+` c ON c.id JOIN `+s.table("organizations")+` o ON o.org_id=c.org_id WHERE g.grant_id=$1`+grantLock, auth.DeviceGrantID).Scan(&d.userID, &d.deviceID, &d.familyID, &d.origin, &d.expiresAt, &d.refreshExpiresAt, &d.revoked, &d.expired, &epoch, &currentEpoch, &d.orgDisabled)
	} else {
		d.kind, d.grantID = CredentialBinding, auth.BindingGrantID
		err = q.QueryRow(ctx, `SELECT f.user_id,f.device_id,f.family_id,f.origin,b.agent_id,b.room_id,b.binding_id,b.slot,b.generation,g.expires_at,f.expires_at,g.revoked_at IS NOT NULL OR f.revoked_at IS NOT NULL OR dev.disabled OR dev.retired_at IS NOT NULL OR a.retired_at IS NOT NULL OR b.retired_at IS NOT NULL OR h.current_generation<>b.generation OR a.device_id<>f.device_id OR dev.user_id<>f.user_id OR NOT EXISTS(SELECT 1 FROM `+s.table("room_members")+` member WHERE member.room_id=b.room_id AND member.user_id=f.user_id AND NOT member.disabled AND member.role IN ('owner','member')),g.expires_at<=pg_catalog.clock_timestamp() OR f.expires_at<=pg_catalog.clock_timestamp(),g.restore_epoch,p.restore_epoch,o.disabled FROM `+s.table("binding_grants")+` g JOIN `+s.table("device_families")+` f USING(family_id) JOIN `+s.table("devices")+` dev ON dev.device_id=f.device_id JOIN `+s.table("bindings")+` b USING(binding_id) JOIN `+s.table("agents")+` a USING(agent_id) JOIN `+s.table("room_slots")+` h ON h.room_id=b.room_id AND h.slot=b.slot CROSS JOIN `+s.table("identity_policy")+` p JOIN `+s.table("configured_org")+` c ON c.id JOIN `+s.table("organizations")+` o ON o.org_id=c.org_id WHERE g.grant_id=$1 AND f.restore_epoch=p.restore_epoch`+grantLock, auth.BindingGrantID).Scan(&d.userID, &d.deviceID, &d.familyID, &d.origin, &d.agentID, &d.roomID, &d.bindingID, &d.slot, &d.generation, &d.expiresAt, &d.refreshExpiresAt, &d.revoked, &d.expired, &epoch, &currentEpoch, &d.orgDisabled)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		d.missing = true
		return d, nil
	}
	if err != nil {
		return sessionDependency{}, err
	}
	d.userMismatch = auth.UserID != d.userID
	d.epochMismatch = epoch != currentEpoch
	d.expiresAt = d.expiresAt.UTC()
	d.refreshExpiresAt = d.refreshExpiresAt.UTC()
	return d, nil
}

func (s *Store) AuthenticateDevice(ctx context.Context, tokenHash [32]byte, origin string) (DeviceCredential, error) {
	if tokenHash == ([32]byte{}) || !validDeviceOrigin(origin) {
		return DeviceCredential{}, refusedError(ClassAuthorization, nil)
	}
	p := opPlan{control: &controlMutation{kind: controlDeviceAuthenticate, device: &deviceMutation{tokenHash: tokenHash, origin: origin}}}
	r, e := s.resolveWithFallback(ctx, p)
	if e != nil {
		return DeviceCredential{}, e
	}
	if r.device == nil {
		return DeviceCredential{}, refusedError(ClassStateInvalid, nil)
	}
	return *r.device, nil
}
func (s *Store) AuthenticateBinding(ctx context.Context, tokenHash [32]byte, origin string) (BindingCredential, error) {
	if tokenHash == ([32]byte{}) || !validDeviceOrigin(origin) {
		return BindingCredential{}, refusedError(ClassAuthorization, nil)
	}
	p := opPlan{control: &controlMutation{kind: controlBindingAuthenticate, device: &deviceMutation{tokenHash: tokenHash, origin: origin}}}
	r, e := s.resolveWithFallback(ctx, p)
	if e != nil {
		return BindingCredential{}, e
	}
	if r.binding == nil {
		return BindingCredential{}, refusedError(ClassStateInvalid, nil)
	}
	return *r.binding, nil
}
