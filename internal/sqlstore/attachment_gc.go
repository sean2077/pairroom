package sqlstore

import (
	"context"
	"errors"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
)

// CollectAttachments is an explicit operator action, never an HTTP authority.
// Each logical transition uses the ordinary fenced runner. Filesystem deletion
// happens only after a confirmed mark; a lost result stops the batch unchanged
// for later explicit reconciliation. A durable object tombstone makes deletion
// retryable without allowing delayed upload resurrection.
func (s *Store) CollectAttachments(ctx context.Context, objects *enterpriseobject.Store, limit int) (AttachmentCollectionReport, error) {
	var report AttachmentCollectionReport
	if objects == nil || objects.DeploymentID() != s.cfg.DeploymentID || limit < 1 || limit > 32 {
		return report, refusedError(ClassInvalidRequest, nil)
	}
	r, err := s.runAttachment(ctx, AuthContext{}, "", "candidates", attachmentMutation{kind: "candidates", operator: true, limit: limit}, true)
	if err != nil {
		return report, err
	}
	for _, id := range r.attachmentIDs {
		report.Examined++
		current, e := s.runAttachment(ctx, AuthContext{}, "", id, attachmentMutation{kind: "gc.read", objectID: id, operator: true}, true)
		if e != nil {
			return report, e
		}
		switch current.Attachment.State {
		case "reserved", "verified":
			_, e = s.runAttachment(ctx, AuthContext{}, "", id, attachmentMutation{kind: "expire", objectID: id, operator: true}, false)
			if e != nil {
				return report, e
			}
			report.Abandoned++
			continue
		case "abandoned":
			_, e = s.runAttachment(ctx, AuthContext{}, "", id, attachmentMutation{kind: "mark", objectID: id, operator: true}, false)
			if e != nil {
				return report, e
			}
		case "deletion_pending":
		default:
			return report, refusedError(ClassInvalidControlState, nil)
		}
		if e = objects.Delete(ctx, id); e != nil {
			return report, e
		}
		_, e = s.runAttachment(ctx, AuthContext{}, "", id, attachmentMutation{kind: "deleted", objectID: id, operator: true}, false)
		if e != nil {
			return report, e
		}
		report.Deleted++
	}
	return report, nil
}

// BeginAttachmentBackup installs the filesystem retention gate before its SQL
// marker. A crash or unknown SQL outcome may retain excess bytes, never delete
// bytes needed by a restore point. The operator reuses the same barrier ID.
func (s *Store) BeginAttachmentBackup(ctx context.Context, objects *enterpriseobject.Store, id string) (Result, error) {
	if objects == nil || objects.DeploymentID() != s.cfg.DeploymentID || !validRoomCreateIdentity(id) {
		return refused(ClassInvalidRequest, nil)
	}
	if err := objects.BeginBackup(ctx, id); err != nil {
		return notCommitted(ClassStateInvalid, err)
	}
	return s.runAttachment(ctx, AuthContext{}, "", id, attachmentMutation{kind: "backup.begin", barrierID: id, operator: true}, false)
}

// EndAttachmentBackup releases SQL first, then the durable filesystem gate.
// IDs are single-use; retrying a completed begin never opens a new restore
// point. A backup coordinator must use a fresh ID for each new manifest.
func (s *Store) EndAttachmentBackup(ctx context.Context, objects *enterpriseobject.Store, id string) (Result, error) {
	if objects == nil || objects.DeploymentID() != s.cfg.DeploymentID || !validRoomCreateIdentity(id) {
		return refused(ClassInvalidRequest, nil)
	}
	r, err := s.runAttachment(ctx, AuthContext{}, "", id, attachmentMutation{kind: "backup.end", barrierID: id, operator: true}, false)
	if err != nil {
		return r, err
	}
	if r.Outcome != OutcomeCommitted && r.Outcome != OutcomeReplayed {
		return r, errors.New("backup retention release was not confirmed")
	}
	if err = objects.EndBackup(ctx, id); err != nil {
		return r, err
	}
	return r, nil
}
