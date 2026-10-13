package sqlstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
)

func attachmentReceipt(kind string, a attachmentState) *AttachmentReceipt {
	return &AttachmentReceipt{Version: 1, Kind: kind, RoomID: a.roomID, ObjectID: a.meta.ID, State: a.state, Metadata: a.meta, ExpiresAt: a.expiresAt}
}

func (s *Store) lookupAttachment(ctx context.Context, tx pgx.Tx, p opPlan, d *attachmentDependencies) (Result, bool, error) {
	var hash, encoding, digest []byte
	var version int
	err := tx.QueryRow(ctx, `SELECT intent_hash,result_version,result_encoding,result_hash FROM `+s.table("attachment_operations")+` WHERE deployment_id=$1 AND room_id=$2 AND principal=$3 AND op_kind=$4 AND identifier=$5`, p.key.Deployment, p.key.RoomID, p.key.Principal, p.attachment.kind, p.key.Identifier).Scan(&hash, &version, &encoding, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	var receipt AttachmentReceipt
	sum := sha256.Sum256(encoding)
	if version != 1 || len(encoding) > 16384 || !bytes.Equal(sum[:], digest) || json.Unmarshal(encoding, &receipt) != nil {
		return Result{}, true, &storedFactCorruption{err: errors.New("invalid attachment receipt")}
	}
	canonical, e := json.Marshal(receipt)
	if e != nil || !bytes.Equal(canonical, encoding) || receipt.Version != 1 || receipt.Kind != p.attachment.kind || receipt.BarrierID != p.attachment.barrierID {
		return Result{}, true, &storedFactCorruption{err: errors.New("attachment receipt disagrees with operation")}
	}
	expectedState := map[string]string{"reserve": "reserved", "verify": "verified", "cancel": "abandoned", "expire": "abandoned", "mark": "deletion_pending", "deleted": "deletion_pending", "backup.begin": "", "backup.end": ""}[receipt.Kind]
	if receipt.State != expectedState {
		return Result{}, true, &storedFactCorruption{err: errors.New("attachment receipt has invalid lifecycle state")}
	}
	if expectedState != "" && receipt.ObjectID == "" || expectedState == "" && (receipt.ObjectID != "" || receipt.RoomID != "" || receipt.Metadata != (model.Attachment{}) || !receipt.ExpiresAt.IsZero()) {
		return Result{}, true, &storedFactCorruption{err: errors.New("attachment receipt has unrelated payload")}
	}
	if receipt.ObjectID != "" {
		if !attachment.ValidObjectID(receipt.ObjectID) || p.attachment.objectID != "" && receipt.ObjectID != p.attachment.objectID {
			return Result{}, true, &storedFactCorruption{err: errors.New("attachment receipt changed object identity")}
		}
		a, e := s.readAttachmentState(ctx, tx, receipt.ObjectID, lockNone)
		if e != nil {
			return Result{}, true, e
		}
		if a.missing || receipt.RoomID != a.roomID || receipt.Metadata.ID != a.meta.ID || receipt.Metadata.Name != a.meta.Name || receipt.Metadata.Kind != a.meta.Kind || receipt.Metadata.Size != a.meta.Size || receipt.Metadata.SHA256 != a.meta.SHA256 || !receipt.Metadata.CreatedAt.Equal(a.meta.CreatedAt) || !receipt.ExpiresAt.Equal(a.expiresAt) {
			return Result{}, true, &storedFactCorruption{err: errors.New("attachment receipt disagrees with immutable reservation")}
		}
		if !p.attachment.operator {
			if a.roomID != p.key.RoomID || a.state != "referenced" && (!a.epochMatches || a.ownerID != p.userID || a.bindingID != d.authority.credential.bindingID) {
				r, e := refused(ClassAuthorization, nil)
				return r, true, e
			}
			if bytes.Equal(hash, p.hash[:]) && receipt.Kind == "reserve" && (AttachmentSpec{Name: a.meta.Name, Kind: a.meta.Kind, Size: a.meta.Size, SHA256: a.meta.SHA256}) != p.attachment.spec {
				return Result{}, true, &storedFactCorruption{err: errors.New("attachment reservation changed frozen specification")}
			}
		}
		if receipt.Metadata.Source != "upload" || receipt.Kind != "reserve" && (receipt.Metadata.MediaType != a.meta.MediaType || receipt.Metadata.Width != a.meta.Width || receipt.Metadata.Height != a.meta.Height) {
			return Result{}, true, &storedFactCorruption{err: errors.New("attachment receipt changed derived metadata")}
		}
	}
	if !bytes.Equal(hash, p.hash[:]) {
		r, e := controlConflict()
		return r, true, e
	}
	return Result{Outcome: OutcomeReplayed, Attachment: &receipt}, true, nil
}

func (s *Store) resolveAttachmentPlan(ctx context.Context, tx pgx.Tx, p opPlan, d *attachmentDependencies) (Result, error) {
	if !attachmentAllowed(p, d, false) {
		return refused(ClassAuthorization, nil)
	}
	if p.attachment.kind == "candidates" {
		var ids []string
		err := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(object_id ORDER BY object_id),ARRAY[]::text[]) FROM (SELECT object_id FROM `+s.table("attachments")+` WHERE deleted_at IS NULL AND ((state IN ('reserved','verified') AND expires_at<=pg_catalog.clock_timestamp()) OR (state='abandoned' AND abandoned_at<=pg_catalog.clock_timestamp()-interval '7 days') OR state='deletion_pending') ORDER BY object_id LIMIT $1) candidates`, p.attachment.limit).Scan(&ids)
		if err != nil {
			return resolveIncomplete(ctx, err)
		}
		return Result{Outcome: OutcomeReplayed, attachmentIDs: ids}, nil
	}
	if attachmentReadOnly(p.attachment.kind) {
		a := d.object
		if p.attachment.kind == "gc.read" {
			if d.barrier || a.missing {
				return refused(ClassInvalidControlState, nil)
			}
			return Result{Outcome: OutcomeReplayed, Attachment: attachmentReceipt(p.attachment.kind, a)}, nil
		}
		if p.attachment.kind == "upload" {
			if !attachmentAllowed(p, d, true) || a.expired || (a.state != "reserved" && a.state != "verified") {
				return refused(ClassInvalidControlState, nil)
			}
		} else if a.state != "verified" && a.state != "referenced" {
			return refused(ClassInvalidControlState, nil)
		}
		return Result{Outcome: OutcomeReplayed, Attachment: attachmentReceipt(p.attachment.kind, a)}, nil
	}
	r, found, err := s.lookupAttachment(ctx, tx, p, d)
	if err != nil {
		return catalogFailure(s, ctx, r, err, true)
	}
	if !found {
		return Result{Outcome: OutcomeNotCommitted}, nil
	}
	return r, nil
}

func (s *Store) changeAttachmentUsage(ctx context.Context, tx pgx.Tx, roomID string, size, pending int64) error {
	for _, scope := range []string{"deployment", "room"} {
		id := s.cfg.DeploymentID
		if scope == "room" {
			id = roomID
		}
		tag, err := tx.Exec(ctx, `UPDATE `+s.table("attachment_usage")+` SET bytes=bytes+$3,pending=pending+$4 WHERE scope=$1 AND scope_id=$2`, scope, id, size, pending)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			if _, err = tx.Exec(ctx, `INSERT INTO `+s.table("attachment_usage")+`(scope,scope_id,bytes,pending) VALUES($1,$2,$3,$4)`, scope, id, size, pending); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) applyAttachmentPlan(ctx context.Context, tx pgx.Tx, p opPlan, d *attachmentDependencies) (Result, error) {
	if !attachmentAllowed(p, d, false) {
		return refused(ClassAuthorization, nil)
	}
	if p.attachment.kind == "backup.begin" {
		var ended bool
		if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("attachment_backup_barriers")+` WHERE barrier_id=$1 AND ended_at IS NOT NULL)`, p.attachment.barrierID).Scan(&ended); e != nil {
			return controlStatementFailure(s, ctx, e)
		} else if ended {
			return refused(ClassInvalidControlState, nil)
		}
	}
	r, found, err := s.lookupAttachment(ctx, tx, p, d)
	if err != nil {
		return catalogFailure(s, ctx, r, err, false)
	}
	if found {
		return r, nil
	}
	if !attachmentAllowed(p, d, true) {
		return refused(ClassAuthorization, nil)
	}
	m := p.attachment
	a := d.object
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := &AttachmentReceipt{Version: 1, Kind: m.kind, BarrierID: m.barrierID}
	switch m.kind {
	case "reserve":
		if e := s.checkPersistentAdmission(ctx, tx, "objects", 1); e != nil {
			return resourceAdmissionFailure(s, ctx, e)
		}
		limits := s.cfg.AttachmentLimits
		if limits == (AttachmentLimits{}) || m.spec.Size > limits.DeploymentBytes-d.deploymentBytes || m.spec.Size > limits.RoomBytes-d.roomBytes || d.deploymentPending >= limits.DeploymentPending || d.roomPending >= limits.RoomPending {
			return refused(ClassRangeExceeded, nil)
		}
		var id [12]byte
		if _, err = rand.Read(id[:]); err != nil {
			return refused(ClassStateInvalid, err)
		}
		a = attachmentState{roomID: p.key.RoomID, ownerID: p.userID, bindingID: d.authority.credential.bindingID, state: "reserved", meta: model.Attachment{ID: "att-" + hex.EncodeToString(id[:]), Name: m.spec.Name, Kind: m.spec.Kind, Size: m.spec.Size, SHA256: m.spec.SHA256, Source: "upload", CreatedAt: now}, expiresAt: now.Add(AttachmentReservationTTL)}
		_, err = tx.Exec(ctx, `INSERT INTO `+s.table("attachments")+`(object_id,room_id,owner_user_id,binding_id,state,name,kind,size,sha256,created_at,expires_at,restore_epoch) VALUES($1,$2,$3,$4,'reserved',$5,$6,$7,$8,$9,$10,COALESCE((SELECT restore_epoch FROM `+s.table("identity_policy")+` WHERE id),''))`, a.meta.ID, a.roomID, a.ownerID, a.bindingID, a.meta.Name, a.meta.Kind, a.meta.Size, a.meta.SHA256, now, a.expiresAt)
		if err == nil {
			err = s.changeAttachmentUsage(ctx, tx, a.roomID, a.meta.Size, 1)
		}
	case "verify":
		v := m.verified.Metadata()
		if a.missing || a.expired || (a.state != "reserved" && a.state != "verified") || m.verified.DeploymentID() != s.cfg.DeploymentID || v.ID != a.meta.ID || v.Name != a.meta.Name || v.Kind != a.meta.Kind || v.Size != a.meta.Size || v.SHA256 != a.meta.SHA256 || v.Source != "upload" || v.MediaType == "" {
			return refused(ClassInvalidRequest, nil)
		}
		v.CreatedAt = a.meta.CreatedAt
		if a.state == "verified" && !a.meta.EqualMetadata(v) {
			return refused(ClassInvalidControlState, nil)
		}
		a.meta = v
		a.state = "verified"
		_, err = tx.Exec(ctx, `UPDATE `+s.table("attachments")+` SET state='verified',media_type=$2,width=$3,height=$4,verified_at=COALESCE(verified_at,$5) WHERE object_id=$1`, a.meta.ID, v.MediaType, v.Width, v.Height, now)
	case "cancel", "expire":
		if a.missing || (a.state != "reserved" && a.state != "verified") || m.kind == "expire" && !a.expired {
			return refused(ClassInvalidControlState, nil)
		}
		a.state = "abandoned"
		_, err = tx.Exec(ctx, `UPDATE `+s.table("attachments")+` SET state='abandoned',abandoned_at=$2 WHERE object_id=$1`, a.meta.ID, now)
		if err == nil {
			err = s.changeAttachmentUsage(ctx, tx, a.roomID, 0, -1)
		}
	case "mark":
		if d.barrier || a.missing || a.state != "abandoned" || !a.collectible {
			return refused(ClassInvalidControlState, nil)
		}
		a.state = "deletion_pending"
		_, err = tx.Exec(ctx, `UPDATE `+s.table("attachments")+` SET state='deletion_pending',marked_at=$2 WHERE object_id=$1`, a.meta.ID, now)
	case "deleted":
		if d.barrier || a.missing || a.state != "deletion_pending" || !a.deletedAt.IsZero() {
			return refused(ClassInvalidControlState, nil)
		}
		_, err = tx.Exec(ctx, `UPDATE `+s.table("attachments")+` SET deleted_at=$2 WHERE object_id=$1`, a.meta.ID, now)
		if err == nil {
			err = s.changeAttachmentUsage(ctx, tx, a.roomID, -a.meta.Size, 0)
		}
	case "backup.begin":
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM `+s.table("attachment_backup_barriers")+` WHERE ended_at IS NULL`).Scan(&count); err == nil && count >= 64 {
			return refused(ClassRangeExceeded, nil)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO `+s.table("attachment_backup_barriers")+`(barrier_id,created_at) VALUES($1,$2)`, m.barrierID, now)
		}
	case "backup.end":
		var changed pgconn.CommandTag
		changed, err = tx.Exec(ctx, `UPDATE `+s.table("attachment_backup_barriers")+` SET ended_at=$2 WHERE barrier_id=$1 AND ended_at IS NULL`, m.barrierID, now)
		if err == nil && changed.RowsAffected() != 1 {
			return refused(ClassInvalidControlState, nil)
		}
	default:
		return refused(ClassUnsupportedOp, nil)
	}
	if err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	if a.meta.ID != "" {
		receipt = attachmentReceipt(m.kind, a)
	}
	encoding, err := json.Marshal(receipt)
	if err != nil {
		return refused(ClassInvalidFact, err)
	}
	sum := sha256.Sum256(encoding)
	_, err = tx.Exec(ctx, `INSERT INTO `+s.table("attachment_operations")+`(deployment_id,room_id,principal,op_kind,identifier,intent_hash,result_version,result_encoding,result_hash,accepted_at) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,$9)`, p.key.Deployment, p.key.RoomID, p.key.Principal, m.kind, p.key.Identifier, p.hash[:], encoding, sum[:], now)
	if err == nil {
		if m.operator {
			err = s.appendRecoveryAudit(ctx, tx, a.roomID, "attachment."+m.kind, now)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO `+s.table("audit")+`(deployment_id,room_id,user_id,kind,outcome,at) VALUES($1,$2,$3,$4,'accepted',$5)`, p.key.Deployment, p.key.RoomID, p.userID, "attachment."+m.kind, now)
		}
	}
	if err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	return Result{Outcome: OutcomeCommitted, Attachment: receipt}, nil
}
