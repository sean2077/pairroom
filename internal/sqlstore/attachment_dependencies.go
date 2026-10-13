package sqlstore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
)

func (s *Store) readAttachmentState(ctx context.Context, q rowQuerier, id, lock string) (attachmentState, error) {
	a := attachmentState{}
	err := q.QueryRow(ctx, `SELECT room_id,owner_user_id,binding_id,state,object_id,name,kind,size,sha256,media_type,width,height,created_at,expires_at,COALESCE(abandoned_at,'0001-01-01'::timestamptz),COALESCE(marked_at,'0001-01-01'::timestamptz),COALESCE(deleted_at,'0001-01-01'::timestamptz),expires_at<=pg_catalog.clock_timestamp(),COALESCE(abandoned_at<=pg_catalog.clock_timestamp()-interval '7 days',false),restore_epoch=COALESCE((SELECT restore_epoch FROM `+s.table("identity_policy")+` WHERE id),'') FROM `+s.table("attachments")+` WHERE object_id=$1`+lock, id).Scan(&a.roomID, &a.ownerID, &a.bindingID, &a.state, &a.meta.ID, &a.meta.Name, &a.meta.Kind, &a.meta.Size, &a.meta.SHA256, &a.meta.MediaType, &a.meta.Width, &a.meta.Height, &a.meta.CreatedAt, &a.expiresAt, &a.abandonedAt, &a.markedAt, &a.deletedAt, &a.expired, &a.collectible, &a.epochMatches)
	if errors.Is(err, pgx.ErrNoRows) {
		a.missing = true
		return a, nil
	}
	a.meta.Source = "upload"
	return a, err
}

func (s *Store) readAttachmentDependencies(ctx context.Context, q rowQuerier, p opPlan, lock string) (*attachmentDependencies, error) {
	d := &attachmentDependencies{}
	m := p.attachment
	if !m.operator {
		if err := s.readDependencies(ctx, q, p.key.RoomID, p.auth, &d.authority, lock); err != nil {
			return nil, err
		}
	}
	objectLock := lock
	if lock != "" && !attachmentReadOnly(m.kind) {
		objectLock = lockExclusive
	}
	var err error
	if m.objectID != "" {
		d.object, err = s.readAttachmentState(ctx, q, m.objectID, objectLock)
		if err != nil {
			return nil, err
		}
	}
	if m.kind == "reserve" {
		for _, scope := range []string{"deployment", "room"} {
			id := s.cfg.DeploymentID
			if scope == "room" {
				id = p.key.RoomID
			}
			var size, pending int64
			err = q.QueryRow(ctx, `SELECT bytes,pending FROM `+s.table("attachment_usage")+` WHERE scope=$1 AND scope_id=$2`+objectLock, scope, id).Scan(&size, &pending)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			if scope == "room" {
				d.roomBytes, d.roomPending = size, pending
			} else {
				d.deploymentBytes, d.deploymentPending = size, pending
			}
		}
	}
	if m.operator {
		err = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("attachment_backup_barriers")+` WHERE ended_at IS NULL)`).Scan(&d.barrier)
		return d, err
	}
	roomLock := lock
	if lock != "" && !attachmentReadOnly(m.kind) {
		roomLock = lockExclusive
	}
	err = s.readRoom(ctx, q, p.key.RoomID, &d.authority, roomLock)
	return d, err
}

func attachmentAuthority(p opPlan, d dependencyState, write bool) bool {
	if !d.readAllowed() || d.roomMissing || write && (!d.writeAllowed() || d.archived) {
		return false
	}
	if p.auth.DeviceGrantID != "" {
		return false
	}
	if p.auth.BindingGrantID != "" {
		return d.credential.kind == CredentialBinding && d.credential.roomID == p.key.RoomID
	}
	return p.auth.SessionID != "" && d.credential.kind == CredentialBrowser || !d.credential.required && p.auth.SessionID == ""
}
func attachmentDraftOwner(p opPlan, d *attachmentDependencies) bool {
	return d.object.epochMatches && d.object.ownerID == p.userID && d.object.bindingID == d.authority.credential.bindingID
}
func attachmentAllowed(p opPlan, d *attachmentDependencies, write bool) bool {
	if p.attachment.operator {
		return true
	}
	if !attachmentAuthority(p, d.authority, write) {
		return false
	}
	if p.attachment.objectID == "" {
		return true
	}
	if d.object.missing || d.object.roomID != p.key.RoomID {
		return false
	}
	return d.object.state == "referenced" && !write || attachmentDraftOwner(p, d)
}
