package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func bindingIdentityAt(events []model.Event, seq uint64, bindingID string) (model.EnterpriseBinding, error) {
	var found model.EnterpriseBinding
	for _, event := range events {
		if event.Seq > seq {
			break
		}
		if event.Kind != model.EnterpriseBindingUpdatedKind {
			continue
		}
		binding, err := model.DecodeEnterpriseBindingUpdate(event)
		if err != nil {
			return found, &storedFactCorruption{err: err}
		}
		if binding.BindID == bindingID {
			found = binding
		}
	}
	if found.BindID == "" {
		return found, &storedFactCorruption{err: errors.New("binding authority is absent from durable history")}
	}
	return found, nil
}

func (s *Store) bindingAuthoritySnapshot(ctx context.Context, tx pgx.Tx, plan opPlan, d *bindingDependencies) (RoomSnapshot, error) {
	return s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: plan.key.RoomID, DeploymentID: s.cfg.DeploymentID, OrgID: d.orgID, HostMode: d.hostMode, Seq: d.seq, Revision: d.revision})
}

func (s *Store) renewBindingAuthority(ctx context.Context, tx pgx.Tx, plan opPlan, d *bindingDependencies, at time.Time, receipt *BindingAuthorityReceipt) error {
	c := plan.binding.renewal
	if !bindingRequesterEligible(d) || d.renewTarget.retired || d.agentMissing || d.agentRetired || d.activeBinding != c.BindingID || d.activeAgentBinding != c.BindingID || d.headMissing || d.head != c.Generation || d.agentDevice != d.credential.deviceID || d.agentOrg != d.orgID {
		return refusedError(ClassAuthorization, nil)
	}
	if d.revision >= math.MaxInt64 {
		return refusedError(ClassRangeExceeded, nil)
	}
	snapshot, err := s.bindingAuthoritySnapshot(ctx, tx, plan, d)
	if err != nil {
		return err
	}
	if _, err := relay.ProjectEnterpriseSnapshot(toRelaySnapshot(snapshot)); err != nil {
		return &storedFactCorruption{err: err}
	}
	identity, err := bindingIdentityAt(snapshot.Events, d.seq, c.BindingID)
	if err != nil {
		return err
	}
	if !identity.Active || identity.RoomID != plan.key.RoomID || identity.Slot != d.renewTarget.slot || identity.Generation != c.Generation || identity.AgentID != d.renewTarget.agentID || identity.Runtime != d.runtime {
		return &storedFactCorruption{err: errors.New("renewal authority disagrees with original Binding identity")}
	}
	var used bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("binding_grants")+` WHERE grant_id=$1 OR token_hash=$2) OR EXISTS(SELECT 1 FROM `+s.table("binding_requests")+` WHERE grant_id=$1 OR token_hash=$2)`, c.Grant.GrantID, c.Grant.TokenHash[:]).Scan(&used); err != nil {
		return err
	}
	if used {
		return refusedError(ClassInvalidControlState, nil)
	}
	expires := at.Add(bindingGrantTTL)
	if expires.After(d.credential.refreshExpiresAt) {
		expires = d.credential.refreshExpiresAt
	}
	if !expires.After(at) {
		return refusedError(ClassAuthorization, nil)
	}
	// Grant replacement is operational authority, not a Binding transition:
	// no Room event and no replay-induced cancellation/unknown state changes.
	if _, err := tx.Exec(ctx, `UPDATE `+s.table("binding_grants")+` SET revoked_at=COALESCE(revoked_at,$2) WHERE binding_id=$1`, c.BindingID, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("binding_grants")+`(grant_id,binding_id,family_id,token_hash,restore_epoch,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, c.Grant.GrantID, c.BindingID, d.credential.familyID, c.Grant.TokenHash[:], d.restoreEpoch, at, expires); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET revision=$2 WHERE room_id=$1`, plan.key.RoomID, int64(d.revision+1)); err != nil {
		return err
	}
	receipt.Binding, receipt.GrantID, receipt.ExpiresAt, receipt.Seq, receipt.Revision = &identity, c.Grant.GrantID, expires, d.seq, d.revision+1
	return nil
}

func validateBindingRenewalReceipt(r BindingAuthorityReceipt) error {
	if r.Version != 1 || r.Kind != bindingRenew || r.Request != nil || r.Binding == nil || !r.Binding.Active || !validRoomCreateIdentity(r.GrantID) || r.ExpiresAt.IsZero() || r.Seq < 2 || r.Seq > math.MaxInt64 || r.Revision == 0 || r.Revision > math.MaxInt64 {
		return errors.New("invalid Binding credential renewal result")
	}
	data, _ := json.Marshal(r.Binding)
	_, err := model.DecodeEnterpriseBindingUpdate(model.Event{ID: "validate-binding-renewal", RoomID: r.Binding.RoomID, Kind: model.EnterpriseBindingUpdatedKind, Actor: r.Binding.Slot, CreatedAt: r.ExpiresAt, Data: data})
	return err
}

func (s *Store) validateBindingRenewalReplay(ctx context.Context, tx pgx.Tx, plan opPlan, d *bindingDependencies, r BindingAuthorityReceipt) error {
	c, b := plan.binding.renewal, r.Binding
	if b.RoomID != plan.key.RoomID || b.BindID != c.BindingID || b.Generation != c.Generation || b.AgentID != d.renewTarget.agentID || b.Runtime != d.runtime || b.Slot != d.renewTarget.slot || r.GrantID != c.Grant.GrantID || r.Seq > d.seq {
		return &storedFactCorruption{err: errors.New("renewal receipt changed its immutable Binding scope")}
	}
	var hash []byte
	var family, epoch, bindingID string
	var created, expires time.Time
	err := tx.QueryRow(ctx, `SELECT binding_id,family_id,token_hash,restore_epoch,created_at,expires_at FROM `+s.table("binding_grants")+` WHERE grant_id=$1`, r.GrantID).Scan(&bindingID, &family, &hash, &epoch, &created, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return &storedFactCorruption{err: errors.New("renewal receipt grant is missing")}
	}
	if err != nil {
		return err
	}
	if bindingID != c.BindingID || family != d.credential.familyID || epoch != d.restoreEpoch || !bytes.Equal(hash, c.Grant.TokenHash[:]) || !expires.Equal(r.ExpiresAt) || !expires.After(created) || expires.After(created.Add(bindingGrantTTL)) {
		return &storedFactCorruption{err: errors.New("renewal receipt changed its frozen grant candidate")}
	}
	snapshot, err := s.bindingAuthoritySnapshot(ctx, tx, plan, d)
	if err != nil {
		return err
	}
	identity, err := bindingIdentityAt(snapshot.Events, r.Seq, c.BindingID)
	if err != nil {
		return err
	}
	expected, _ := json.Marshal(b)
	actual, _ := json.Marshal(identity)
	if !bytes.Equal(expected, actual) {
		return &storedFactCorruption{err: errors.New("renewal receipt changed original bound metadata")}
	}
	return nil
}
