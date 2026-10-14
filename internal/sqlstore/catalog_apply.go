package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
)

func (s *Store) lookupCatalog(ctx context.Context, tx pgx.Tx, plan opPlan) (Result, bool, error) {
	var intent, encoding, digest []byte
	var version int
	err := tx.QueryRow(ctx, `SELECT intent_hash,result_version,result_encoding,result_hash FROM `+s.table("catalog_operations")+` WHERE deployment_id=$1 AND principal=$2 AND op_kind=$3 AND identifier=$4`, plan.key.Deployment, plan.userID, plan.catalog.kind, plan.key.Identifier).Scan(&intent, &version, &encoding, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(intent, plan.hash[:]) {
		e := &KernelError{Outcome: OutcomeConflict, Class: ClassPayloadConflict}
		return Result{Outcome: OutcomeConflict, Class: ClassPayloadConflict, Err: e}, true, e
	}
	sum := sha256.Sum256(encoding)
	var receipt CatalogReceipt
	if version != 1 || !bytes.Equal(sum[:], digest) || json.Unmarshal(encoding, &receipt) != nil {
		return Result{}, true, &storedFactCorruption{err: errors.New("invalid catalog receipt encoding")}
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, encoding) || receipt.Version != 1 || receipt.Intent != plan.catalog.cmd.Intent || !validateCatalogIntent(receipt.Intent) {
		return Result{}, true, &storedFactCorruption{err: errors.New("catalog receipt disagrees with accepted intent")}
	}
	var fact model.Event
	if receipt.Intent.RoomID != "" {
		if receipt.Seq < 2 || receipt.Revision != plan.catalog.cmd.ExpectedRevision+1 {
			return Result{}, true, &storedFactCorruption{err: errors.New("invalid catalog Room cursor")}
		}
		fact, err = s.loadFact(ctx, tx, receipt.Intent.RoomID, receipt.Seq)
		if err != nil {
			return Result{}, true, err
		}
		change, err := model.DecodeEnterpriseRoomMetadata(fact)
		if err != nil || change != catalogMetadata(receipt.Intent) {
			return Result{}, true, &storedFactCorruption{err: errors.New("catalog receipt has a different Room fact")}
		}
	} else if receipt.Seq != 0 || strings.HasPrefix(receipt.Intent.Kind, "project.") && receipt.Revision != plan.catalog.cmd.ExpectedRevision+1 || (receipt.Intent.Kind == "session.revoke" || receipt.Intent.Kind == "agent.retire") && receipt.Revision != 0 {
		return Result{}, true, &storedFactCorruption{err: errors.New("invalid catalog control cursor")}
	}
	return Result{Outcome: OutcomeReplayed, Catalog: &receipt, Fact: fact, Seq: receipt.Seq, Revision: receipt.Revision}, true, nil
}

func catalogMetadata(intent CatalogIntent) model.EnterpriseRoomMetadataChange {
	return model.EnterpriseRoomMetadataChange{Version: 1, Action: strings.TrimPrefix(intent.Kind, "room."), Name: intent.Name, ProjectID: intent.ProjectID}
}

func catalogFailure(s *Store, ctx context.Context, result Result, err error, readOnly bool) (Result, error) {
	if isStoredFactCorruption(err) {
		return refused(ClassStoredFactUnreadable, err)
	}
	var kernel *KernelError
	if errors.As(err, &kernel) {
		return result, err
	}
	if readOnly {
		return resolveIncomplete(ctx, err)
	}
	return controlStatementFailure(s, ctx, err)
}

func (s *Store) resolveCatalogPlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *catalogDependencies) (Result, error) {
	if !catalogReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	if catalogReadOnly(plan.catalog.kind) {
		page, err := s.readCatalogPage(ctx, tx, plan)
		if err != nil {
			return resolveIncomplete(ctx, err)
		}
		return Result{Outcome: OutcomeReplayed, catalogPage: &page}, nil
	}
	result, found, err := s.lookupCatalog(ctx, tx, plan)
	if err != nil {
		return catalogFailure(s, ctx, result, err, true)
	}
	if !found {
		return Result{Outcome: OutcomeNotCommitted}, nil
	}
	return result, nil
}

func (s *Store) applyCatalogPlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *catalogDependencies) (Result, error) {
	if !catalogReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	result, found, err := s.lookupCatalog(ctx, tx, plan)
	if err != nil {
		return catalogFailure(s, ctx, result, err, false)
	}
	if found {
		return result, nil
	}
	if !catalogWriteAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	cmd := plan.catalog.cmd
	intent := cmd.Intent
	if strings.HasPrefix(intent.Kind, "room.") && cmd.ExpectedRevision != d.roomRevision || strings.HasPrefix(intent.Kind, "project.") && intent.Kind != "project.create" && cmd.ExpectedRevision != d.projectRevision {
		return Result{Outcome: OutcomeConflict, Class: ClassRevisionConflict}, nil
	}
	if d.roomSeq >= math.MaxInt64 || d.roomRevision >= math.MaxInt64 || d.projectRevision >= math.MaxInt64 {
		return refused(ClassRangeExceeded, nil)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	receipt := CatalogReceipt{Version: 1, Intent: intent}
	var fact model.Event
	switch intent.Kind {
	case "project.create":
		if !d.projectMissing {
			return refused(ClassInvalidControlState, nil)
		}
		if e := s.checkPersistentAdmission(ctx, tx, "projects", 1); e != nil {
			return resourceAdmissionFailure(s, ctx, e)
		}
		if e := s.checkMembershipAdmission(ctx, tx, "project", intent.ProjectID, 1); e != nil {
			return resourceAdmissionFailure(s, ctx, e)
		}
		_, err = tx.Exec(ctx, `INSERT INTO `+s.table("projects")+`(project_id,org_id,display_name,owner_user_id,revision,created_at) VALUES($1,$2,$3,$4,1,$5)`, intent.ProjectID, d.orgID, intent.Name, plan.userID, now)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO `+s.table("project_members")+`(project_id,org_id,user_id,role,created_at) VALUES($1,$2,$3,'owner',$4)`, intent.ProjectID, d.orgID, plan.userID, now)
		}
		receipt.Revision = 1
	case "project.rename", "project.archive", "project.restore", "project.member.add", "project.member.remove":
		if strings.HasPrefix(intent.Kind, "project.member.") {
			if d.targetMissing || intent.TargetUserID == d.projectOwner || d.projectTargetRole == "owner" || intent.Kind == "project.member.add" && (d.targetDisabled || !d.projectTargetMissing && !d.projectTargetDisabled) || intent.Kind == "project.member.remove" && (d.projectTargetMissing || d.projectTargetDisabled) {
				return refused(ClassInvalidControlState, nil)
			}
			if intent.Kind == "project.member.add" {
				if d.projectTargetMissing {
					if e := s.checkMembershipAdmission(ctx, tx, "project", intent.ProjectID, 1); e != nil {
						return resourceAdmissionFailure(s, ctx, e)
					}
				}
				_, err = tx.Exec(ctx, `INSERT INTO `+s.table("project_members")+`(project_id,org_id,user_id,role,created_at) VALUES($1,$2,$3,'member',$4) ON CONFLICT(project_id,user_id) DO UPDATE SET disabled=false`, intent.ProjectID, d.orgID, intent.TargetUserID, now)
			} else {
				_, err = tx.Exec(ctx, `UPDATE `+s.table("project_members")+` SET disabled=true WHERE project_id=$1 AND user_id=$2`, intent.ProjectID, intent.TargetUserID)
			}
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE `+s.table("projects")+` SET display_name=CASE WHEN $2='project.rename' THEN $3 ELSE display_name END,archived_at=CASE WHEN $2='project.archive' THEN $4 WHEN $2='project.restore' THEN NULL ELSE archived_at END,revision=revision+1 WHERE project_id=$1`, intent.ProjectID, intent.Kind, intent.Name, now)
		}
		receipt.Revision = d.projectRevision + 1
	case "room.rename", "room.archive", "room.restore", "room.project":
		if intent.Kind == "room.project" && intent.ProjectID != "" && d.projectArchived || intent.Kind == "room.archive" && d.roomArchived || intent.Kind == "room.restore" && !d.roomArchived {
			return refused(ClassInvalidControlState, nil)
		}
		if intent.Kind == "room.archive" {
			cutoff, e := approvalCutoff(ctx)
			if e != nil {
				return catalogFailure(s, ctx, Result{}, e, false)
			}
			tag, e := tx.Exec(ctx, `UPDATE `+s.table("binding_requests")+` SET status='denied' WHERE room_id=$1 AND status='approved' AND expires_at>$2`, intent.RoomID, cutoff)
			if e != nil {
				return catalogFailure(s, ctx, Result{}, e, false)
			}
			setAuditInvalidations(plan, tag.RowsAffected())
		}
		initial, e := s.loadFact(ctx, tx, intent.RoomID, 1)
		if e != nil {
			return catalogFailure(s, ctx, Result{}, e, false)
		}
		creation, e := model.DecodeEnterpriseRoomCreation(initial)
		if e != nil || creation.Enterprise.OrgID != d.orgID || creation.Enterprise.DeploymentID != plan.key.Deployment {
			return refused(ClassStoredFactUnreadable, e)
		}
		fact, err = model.NewEnterpriseRoomMetadata(intent.RoomID, d.roomSeq+1, catalogMetadata(intent), now)
		if err == nil && intent.Kind != "room.archive" {
			snapshot, e := s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: intent.RoomID, DeploymentID: plan.key.Deployment, OrgID: d.orgID, HostMode: string(model.HostNative), Seq: d.roomSeq, Revision: d.roomRevision})
			if e != nil {
				return resourceAdmissionFailure(s, ctx, e)
			}
			if e = s.checkRoomFactAdmission(ctx, tx, plan, []model.Event{fact}, snapshot); e != nil {
				return resourceAdmissionFailure(s, ctx, e)
			}
		}
		if err == nil {
			err = s.writeStoredRoomEvent(ctx, tx, fact, userFactOrigin(plan.userID))
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET display_name=CASE WHEN $2='room.rename' THEN $3 ELSE display_name END,archived_at=CASE WHEN $2='room.archive' THEN $4 WHEN $2='room.restore' THEN NULL ELSE archived_at END,project_id=CASE WHEN $2='room.project' THEN NULLIF($5,'') ELSE project_id END,current_seq=current_seq+1,revision=revision+1 WHERE room_id=$1`, intent.RoomID, intent.Kind, intent.Name, now, intent.ProjectID)
		}
		receipt.Seq, receipt.Revision = d.roomSeq+1, d.roomRevision+1
	case "session.revoke":
		_, err = tx.Exec(ctx, `UPDATE `+s.table("browser_sessions")+` SET revoked_at=$2 WHERE session_id=$1 AND revoked_at IS NULL`, intent.SessionID, now)
	case "agent.retire":
		scope := authorityRevocationScope{UserID: plan.userID, AgentID: intent.AgentID}
		err = s.retireAuthorityBindings(ctx, tx, scope, plan.userID)
		if err == nil {
			var count int64
			count, err = s.revokeNativeCredentials(ctx, tx, scope)
			if err == nil {
				setAuditInvalidations(plan, count)
			}
		}
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE `+s.table("agents")+` SET retired_at=COALESCE(retired_at,$2),retired_reason=COALESCE(retired_reason,'agent.revoked') WHERE agent_id=$1`, intent.AgentID, now)
		}
	default:
		return refused(ClassUnsupportedOp, nil)
	}
	if err != nil {
		return catalogFailure(s, ctx, Result{}, err, false)
	}
	encoding, err := json.Marshal(receipt)
	if err != nil {
		return refused(ClassInvalidFact, err)
	}
	digest := sha256.Sum256(encoding)
	_, err = tx.Exec(ctx, `INSERT INTO `+s.table("catalog_operations")+`(deployment_id,principal,op_kind,identifier,intent_hash,result_version,result_encoding,result_hash,accepted_at) VALUES($1,$2,$3,$4,$5,1,$6,$7,$8)`, plan.key.Deployment, plan.userID, intent.Kind, plan.key.Identifier, plan.hash[:], encoding, digest[:], now)
	if err == nil {
		target := intent.RoomID
		if strings.HasPrefix(intent.Kind, "project.") {
			target = intent.ProjectID
		}
		if intent.Kind == "agent.retire" {
			target = intent.AgentID
		}
		if intent.Kind == "session.revoke" {
			target = intent.SessionID
		}
		err = s.writeMutationAudit(ctx, tx, plan, intent.RoomID, plan.userID, "user", "catalog."+intent.Kind, "accepted", "", target, auditCount(plan), now)
	}
	if err != nil {
		return catalogFailure(s, ctx, Result{}, err, false)
	}
	return Result{Outcome: OutcomeCommitted, Catalog: &receipt, Fact: fact, Seq: receipt.Seq, Revision: receipt.Revision}, nil
}
