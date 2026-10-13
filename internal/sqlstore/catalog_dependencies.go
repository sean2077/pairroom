package sqlstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

type catalogDependencies struct {
	actorMissing, actorDisabled, targetMissing, targetDisabled                      bool
	credential                                                                      sessionDependency
	orgID                                                                           string
	projectMissing, projectMemberMissing, projectMemberDisabled, projectArchived    bool
	projectOwner, projectRole                                                       string
	projectTargetRole                                                               string
	projectTargetMissing, projectTargetDisabled                                     bool
	projectRevision                                                                 uint64
	roomMissing, roomMemberMissing, roomMemberDisabled, roomArchived, ownerDisabled bool
	roomOwner, roomRole, roomProject                                                string
	roomSeq, roomRevision                                                           uint64
	sessionMissing, sessionRevoked                                                  bool
	sessionUser                                                                     string
	agentMissing, agentRetired                                                      bool
	agentUser                                                                       string
	revocationDigest                                                                [32]byte
	archiveApprovals                                                                [32]byte
}

func (s *Store) readCatalogDependencies(ctx context.Context, q rowQuerier, plan opPlan, lock string) (*catalogDependencies, error) {
	d := &catalogDependencies{}
	m := plan.catalog
	roomID, projectID, target := m.cmd.Intent.RoomID, m.cmd.Intent.ProjectID, m.cmd.Intent.TargetUserID
	if catalogReadOnly(m.kind) {
		roomID, projectID = m.query.RoomID, m.query.ProjectID
	}
	if roomID != "" {
		err := q.QueryRow(ctx, `SELECT owner_user_id FROM `+s.table("rooms")+` WHERE room_id=$1`, roomID).Scan(&d.roomOwner)
		if errors.Is(err, pgx.ErrNoRows) {
			d.roomMissing = true
		} else if err != nil {
			return nil, err
		}
	}
	ids := []string{plan.userID}
	for _, id := range []string{target, d.roomOwner} {
		if id != "" && !containsBindingID(ids, id) {
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
		if id == target {
			d.targetMissing, d.targetDisabled = missing, disabled
		}
		if id == d.roomOwner {
			d.ownerDisabled = disabled || missing
		}
	}
	credentialLock := lock
	if lock != "" && m.kind == "session.revoke" {
		credentialLock = lockExclusive
	}
	var err error
	d.credential, err = s.readCredential(ctx, q, plan.auth, credentialLock)
	if err != nil {
		return nil, err
	}
	if err = q.QueryRow(ctx, `SELECT org_id FROM `+s.table("deployment")+` WHERE deployment_id=$1`, plan.key.Deployment).Scan(&d.orgID); err != nil {
		return nil, err
	}
	if projectID != "" {
		for _, id := range ids {
			if id != plan.userID && id != target {
				continue
			}
			memberLock := lock
			if lock != "" && id == target {
				memberLock = lockExclusive
			}
			var role string
			var disabled, missing bool
			err = q.QueryRow(ctx, `SELECT role,disabled FROM `+s.table("project_members")+` WHERE project_id=$1 AND user_id=$2`+memberLock, projectID, id).Scan(&role, &disabled)
			if errors.Is(err, pgx.ErrNoRows) {
				missing = true
			} else if err != nil {
				return nil, err
			}
			if id == plan.userID {
				d.projectRole, d.projectMemberDisabled, d.projectMemberMissing = role, disabled, missing
			}
			if id == target {
				d.projectTargetRole, d.projectTargetDisabled, d.projectTargetMissing = role, disabled, missing
			}
		}
		projectLock := lock
		if lock != "" && !catalogReadOnly(m.kind) {
			projectLock = lockExclusive
		}
		err = q.QueryRow(ctx, `SELECT COALESCE(owner_user_id,''),archived_at IS NOT NULL,revision FROM `+s.table("projects")+` WHERE project_id=$1 AND org_id=$2`+projectLock, projectID, d.orgID).Scan(&d.projectOwner, &d.projectArchived, &d.projectRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			d.projectMissing = true
		} else if err != nil {
			return nil, err
		}
	}
	if roomID != "" {
		err = q.QueryRow(ctx, `SELECT role,disabled FROM `+s.table("room_members")+` WHERE room_id=$1 AND user_id=$2`+lock, roomID, plan.userID).Scan(&d.roomRole, &d.roomMemberDisabled)
		if errors.Is(err, pgx.ErrNoRows) {
			d.roomMemberMissing = true
		} else if err != nil {
			return nil, err
		}
		if m.kind == "room.archive" {
			d.archiveApprovals, err = s.readCatalogArchiveApprovals(ctx, q, roomID, lock)
			if err != nil {
				return nil, err
			}
		}
		roomLock := lock
		if lock != "" && !catalogReadOnly(m.kind) {
			roomLock = lockExclusive
		}
		err = q.QueryRow(ctx, `SELECT owner_user_id,COALESCE(project_id,''),archived_at IS NOT NULL,current_seq,revision FROM `+s.table("rooms")+` WHERE room_id=$1 AND org_id=$2`+roomLock, roomID, d.orgID).Scan(&d.roomOwner, &d.roomProject, &d.roomArchived, &d.roomSeq, &d.roomRevision)
		if errors.Is(err, pgx.ErrNoRows) {
			d.roomMissing = true
		} else if err != nil {
			return nil, err
		}
	}
	if m.kind == "session.revoke" {
		sessionLock := lock
		if lock != "" {
			sessionLock = lockExclusive
		}
		err = q.QueryRow(ctx, `SELECT user_id,revoked_at IS NOT NULL FROM `+s.table("browser_sessions")+` WHERE session_id=$1`+sessionLock, m.cmd.Intent.SessionID).Scan(&d.sessionUser, &d.sessionRevoked)
		if errors.Is(err, pgx.ErrNoRows) {
			d.sessionMissing = true
		} else if err != nil {
			return nil, err
		}
	}
	if m.kind == "agent.retire" {
		err = q.QueryRow(ctx, `SELECT d.user_id,a.retired_at IS NOT NULL FROM `+s.table("agents")+` a JOIN `+s.table("devices")+` d USING(device_id) WHERE a.agent_id=$1`, m.cmd.Intent.AgentID).Scan(&d.agentUser, &d.agentRetired)
		if errors.Is(err, pgx.ErrNoRows) {
			d.agentMissing = true
		} else if err != nil {
			return nil, err
		}
		d.revocationDigest, err = s.readAuthorityRevocation(ctx, q, authorityRevocationScope{UserID: plan.userID, AgentID: m.cmd.Intent.AgentID}, lock)
		if err != nil {
			return nil, err
		}
	}
	return d, nil
}

// The approved set is part of discovery equality, not just an UPDATE filter.
// A newly approved or consumed request between discovery and the exclusive
// ownership guard changes this digest and restarts the pure discovery phase.
// Lock requests in stable identity order before the Room consistency row.
func (s *Store) readCatalogArchiveApprovals(ctx context.Context, q rowQuerier, roomID, lock string) ([32]byte, error) {
	var result [32]byte
	var ids []string
	err := q.QueryRow(ctx, `SELECT COALESCE(array_agg(request_id ORDER BY request_id),ARRAY[]::text[]) FROM (SELECT request_id FROM `+s.table("binding_requests")+` WHERE room_id=$1 AND status='approved' ORDER BY request_id LIMIT 10001) selected`, roomID).Scan(&ids)
	if err != nil {
		return result, err
	}
	if len(ids) > 10000 {
		return result, errors.New("archive approval scope exceeds bounded transaction")
	}
	digest := sha256.New()
	for _, id := range ids {
		clause := lockNone
		if lock != "" {
			clause = lockExclusive
		}
		var status, approvedBy string
		if err = q.QueryRow(ctx, `SELECT status,COALESCE(approved_by,'') FROM `+s.table("binding_requests")+` WHERE request_id=$1`+clause, id).Scan(&status, &approvedBy); err != nil {
			return result, err
		}
		writePart(digest, []byte(id))
		writePart(digest, []byte(status))
		writePart(digest, []byte(approvedBy))
	}
	copy(result[:], digest.Sum(nil))
	return result, nil
}

func catalogReadAllowed(plan opPlan, d *catalogDependencies) bool {
	if d == nil || d.actorMissing || d.actorDisabled || !d.credential.allowed() || d.credential.kind != CredentialBrowser {
		return false
	}
	m := plan.catalog
	roomRequired := m.cmd.Intent.RoomID != "" || m.query.RoomID != ""
	if roomRequired && (d.roomMissing || d.roomMemberMissing || d.roomMemberDisabled) {
		return false
	}
	projectRequired := m.cmd.Intent.ProjectID != "" || m.query.ProjectID != ""
	// Creation has no membership yet. Every other Project-specific disclosure,
	// including an accepted receipt, requires current explicit membership.
	if projectRequired && m.kind != "project.create" && (d.projectMissing || d.projectMemberMissing || d.projectMemberDisabled) {
		return false
	}
	if m.kind == "session.revoke" && (d.sessionMissing || d.sessionUser != plan.userID) {
		return false
	}
	if m.kind == "agent.retire" && (d.agentMissing || d.agentUser != plan.userID) {
		return false
	}
	return true
}

func catalogWriteAllowed(plan opPlan, d *catalogDependencies) bool {
	if !catalogReadAllowed(plan, d) {
		return false
	}
	switch {
	case plan.catalog.kind == "project.create", plan.catalog.kind == "session.revoke", plan.catalog.kind == "agent.retire":
		return true
	case strings.HasPrefix(plan.catalog.kind, "project."):
		return d.projectOwner == plan.userID && d.projectRole == "owner" && (!d.projectArchived || plan.catalog.kind == "project.restore")
	case strings.HasPrefix(plan.catalog.kind, "room."):
		return d.roomOwner == plan.userID && d.roomRole == "owner" && !d.ownerDisabled && (!d.roomArchived || plan.catalog.kind == "room.restore")
	}
	return false
}
