package sqlstore

import (
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
)

const ClassRestorePending = "restore_in_progress"

type RestoreReceipt struct {
	RestoreID    string `json:"restore_id"`
	ManifestHash string `json:"manifest_hash"`
	RestoreEpoch string `json:"restore_epoch"`
	Imported     bool   `json:"imported"`
	Complete     bool   `json:"complete"`
}
type restoreMutation struct {
	kind, id string
	digest   [32]byte
	bundle   *enterprisebackup.Bundle
	writer   *enterprisebackup.Writer
	objects  []model.Attachment
}
type restoreDependencies struct {
	missing, imported, complete, pristine, pending bool
	digest                                         [32]byte
	epoch                                          string
}

// EnsureRestoreReady is required before opening any network listener. The
// PostgreSQL marker survives loss of all local CLI journals.
func (s *Store) EnsureRestoreReady(ctx context.Context) error {
	r, e := s.resolveWithFallback(ctx, opPlan{restore: &restoreMutation{kind: "ready"}})
	if e != nil {
		return e
	}
	if r.Outcome != OutcomeReplayed {
		return refusedError(ClassRestorePending, nil)
	}
	return nil
}
func (s *Store) restorePlan(m *restoreMutation) opPlan {
	return opPlan{restore: m, key: OpKey{Deployment: s.cfg.DeploymentID, Identifier: m.id}, hash: m.digest}
}
func (s *Store) ResolveRestore(ctx context.Context, id string, digest [32]byte) (Result, error) {
	if !validRoomCreateIdentity(id) || digest == ([32]byte{}) {
		return refused(ClassInvalidRequest, nil)
	}
	return s.resolveWithFallback(ctx, s.restorePlan(&restoreMutation{kind: "status", id: id, digest: digest}))
}
func (s *Store) BackupOffline(ctx context.Context, objects *enterpriseobject.Store, path, id string) (enterprisebackup.Manifest, error) {
	var empty enterprisebackup.Manifest
	if objects == nil || objects.DeploymentID() != s.cfg.DeploymentID || !validRoomCreateIdentity(id) {
		return empty, refusedError(ClassInvalidRequest, nil)
	}
	if _, e := s.BeginAttachmentBackup(ctx, objects, id); e != nil {
		return empty, e
	}
	// The retention barrier remains installed on every failure, including an
	// uncertain SQL outcome. No complete manifest is published from that path.
	header, e := s.backupHeader(ctx, id)
	if e != nil {
		return empty, e
	}
	w, e := enterprisebackup.NewWriter(path, header)
	if e != nil {
		return empty, e
	}
	m := &restoreMutation{kind: "export", id: id, writer: w}
	r, e := s.commitWithRestarts(ctx, s.restorePlan(m), model.Event{})
	if e = confirmedBackupExport(r, e); e != nil {
		return empty, e
	}
	for _, meta := range m.objects {
		v, e := objects.Inspect(ctx, meta.ID)
		if e != nil {
			return empty, e
		}
		got := v.Metadata()
		got.CreatedAt = meta.CreatedAt
		if !got.EqualMetadata(meta) {
			return empty, errors.New("backup object metadata disagrees with SQL")
		}
		data, e := objects.ReadVerified(ctx, meta)
		if e != nil {
			return empty, e
		}
		e = w.WriteObject(ctx, meta, bytes.NewReader(data))
		if e != nil {
			return empty, e
		}
	}
	manifest, e := w.Seal(ctx)
	if e != nil {
		return empty, e
	}
	if _, e = enterprisebackup.Open(ctx, path, header.DeploymentID, header.OrgID); e != nil {
		return empty, e
	}
	if _, e = s.EndAttachmentBackup(ctx, objects, id); e != nil {
		return empty, e
	}
	return manifest, nil
}

func confirmedBackupExport(result Result, err error) error {
	if err != nil {
		return err
	}
	if result.Outcome == OutcomeCommitted {
		return nil
	}
	class := result.Class
	if class == "" {
		class = ClassStateInvalid
	}
	outcome := result.Outcome
	if outcome != OutcomeUnknown && outcome != OutcomeNotCommitted && outcome != OutcomeRefused && outcome != OutcomeConflict {
		outcome = OutcomeUnknown
	}
	return &KernelError{Outcome: outcome, Class: class, Err: errors.New("backup SQL export was not confirmed")}
}
func (s *Store) backupHeader(ctx context.Context, id string) (enterprisebackup.Header, error) {
	opCtx, release, e := s.beginOperation(ctx)
	if e != nil {
		return enterprisebackup.Header{}, e
	}
	defer release()
	h := enterprisebackup.Header{BackupID: id, DeploymentID: s.cfg.DeploymentID, CreatedAt: time.Now().UTC(), SchemaVersion: enterprisebackup.SchemaVersion, FactCodecVersion: enterprisebackup.FactCodecVersion, ObjectSchemaVersion: enterprisebackup.ObjectSchemaVersion}
	e = s.work.QueryRow(opCtx, `SELECT org_id,(SELECT restore_epoch FROM `+s.table("identity_policy")+` WHERE id) FROM `+s.table("deployment")+` WHERE id`).Scan(&h.OrgID, &h.RestoreEpoch)
	return h, e
}

// RestoreOffline copies only validated immutable objects before atomic import.
// A single-use SQL reservation gates listeners throughout copy and recovery.
// Repeating an import never creates a second effect: unknown is resolved only.
func (s *Store) RestoreOffline(ctx context.Context, objects *enterpriseobject.Store, b *enterprisebackup.Bundle, id string) (Result, error) {
	if b == nil || objects == nil || objects.DeploymentID() != s.cfg.DeploymentID || !validRoomCreateIdentity(id) || b.Manifest().Header.DeploymentID != s.cfg.DeploymentID {
		return refused(ClassInvalidRequest, nil)
	}
	if e := validateRestoreAnchors(ctx, b); e != nil {
		return refused(ClassInvalidRequest, e)
	}
	digest := b.Digest()
	status, statusErr := s.ResolveRestore(ctx, id, digest)
	if statusErr != nil {
		return status, statusErr
	}
	if status.Outcome == OutcomeReplayed {
		if status.Restore != nil && status.Restore.Imported {
			return status, nil
		}
		return refused(ClassRestorePending, errors.New("interrupted restore requires operator inspection; only resolve is automatic"))
	}
	pristine, pristineErr := objects.Pristine(ctx)
	if pristineErr != nil {
		return notCommitted(ClassStateInvalid, pristineErr)
	}
	if !pristine {
		return refused(ClassInvalidControlState, nil)
	}
	p := s.restorePlan(&restoreMutation{kind: "begin", id: id, digest: digest})
	r, e := s.commitWithRestarts(ctx, p, model.Event{})
	if e != nil {
		return r, e
	}
	if r.Restore != nil && r.Restore.Imported {
		return s.ResolveRestore(ctx, id, digest)
	}
	if e = objects.BeginBackup(ctx, "restore-"+id); e != nil {
		return notCommitted(ClassStateInvalid, e)
	}
	for _, meta := range b.Manifest().Objects {
		data, e := b.ReadObject(ctx, meta.ID)
		if e != nil {
			return notCommitted(ClassInvalidRequest, e)
		}
		v, e := objects.PutImmutable(ctx, meta.ID, AttachmentSpec{Name: meta.Name, Kind: meta.Kind, Size: meta.Size, SHA256: meta.SHA256}, bytes.NewReader(data))
		if e != nil {
			return notCommitted(ClassStateInvalid, e)
		}
		actual := v.Metadata()
		actual.CreatedAt = meta.CreatedAt
		if !actual.EqualMetadata(meta) {
			return notCommitted(ClassStateInvalid, errors.New("restored object metadata mismatch"))
		}
	}
	return s.commitWithRestarts(ctx, s.restorePlan(&restoreMutation{kind: "import", id: id, digest: digest, bundle: b}), model.Event{})
}
func (s *Store) CompleteRestore(ctx context.Context, objects *enterpriseobject.Store, id string, digest [32]byte) (Result, error) {
	if objects == nil || objects.DeploymentID() != s.cfg.DeploymentID || !validRoomCreateIdentity(id) || digest == ([32]byte{}) {
		return refused(ClassInvalidRequest, nil)
	}
	r, e := s.ResolveRestore(ctx, id, digest)
	if e != nil {
		return r, e
	}
	if r.Restore == nil || !r.Restore.Imported {
		return refused(ClassRestorePending, nil)
	}
	p := s.restorePlan(&restoreMutation{kind: "objects", id: id, digest: digest})
	r, e = s.resolveWithFallback(ctx, p)
	if e != nil {
		return r, e
	}
	for _, meta := range r.restoreObjects {
		v, e := objects.Inspect(ctx, meta.ID)
		if e != nil {
			return refused(ClassStateInvalid, e)
		}
		actual := v.Metadata()
		actual.CreatedAt = meta.CreatedAt
		if !actual.EqualMetadata(meta) {
			return refused(ClassStateInvalid, nil)
		}
	}
	r, e = s.commitWithRestarts(ctx, s.restorePlan(&restoreMutation{kind: "complete", id: id, digest: digest}), model.Event{})
	if e != nil {
		return r, e
	}
	if e = objects.EndBackup(ctx, "restore-"+id); e != nil {
		return r, e
	}
	return r, nil
}
func (s *Store) readRestoreDependencies(ctx context.Context, q rowQuerier, p opPlan, lock string) (*restoreDependencies, error) {
	d := &restoreDependencies{}
	m := p.restore
	var hash []byte
	e := q.QueryRow(ctx, `SELECT manifest_hash,restore_epoch,imported_at IS NOT NULL,completed_at IS NOT NULL FROM `+s.table("restore_runs")+` WHERE restore_id=$1`+lock, m.id).Scan(&hash, &d.epoch, &d.imported, &d.complete)
	if errors.Is(e, pgx.ErrNoRows) {
		d.missing = true
	} else if e != nil {
		return nil, e
	} else if len(hash) != 32 {
		return nil, errors.New("invalid restore hash")
	} else {
		copy(d.digest[:], hash)
	}
	e = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("restore_runs")+` WHERE completed_at IS NULL)`).Scan(&d.pending)
	if e != nil {
		return nil, e
	}
	if m.kind == "begin" || m.kind == "import" {
		d.pristine = true
		for _, name := range enterprisebackup.Tables() {
			if name == "organizations" || name == "configured_org" || name == "deployment" {
				continue
			}
			var exists bool
			if e = q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table(name)+`)`).Scan(&exists); e != nil {
				return nil, e
			}
			if exists {
				d.pristine = false
				break
			}
		}
	}
	return d, nil
}
func restoreResult(m *restoreMutation, d *restoreDependencies, outcome Outcome) Result {
	return Result{Outcome: outcome, Restore: &RestoreReceipt{RestoreID: m.id, ManifestHash: fmt.Sprintf("%x", d.digest), RestoreEpoch: d.epoch, Imported: d.imported, Complete: d.complete}}
}
func (s *Store) resolveRestorePlan(ctx context.Context, tx pgx.Tx, p opPlan, d *restoreDependencies) (Result, error) {
	m := p.restore
	if m.kind == "health" {
		return s.resolveReadiness(ctx, tx, d.pending)
	}
	if m.kind == "ready" {
		if d.pending {
			return refused(ClassRestorePending, nil)
		}
		return Result{Outcome: OutcomeReplayed}, nil
	}
	if d.missing {
		return Result{Outcome: OutcomeNotCommitted}, nil
	}
	if d.digest != m.digest {
		return controlConflict()
	}
	r := restoreResult(m, d, OutcomeReplayed)
	if m.kind == "objects" {
		objects, e := s.restoreObjectMetadata(ctx, tx)
		if e != nil {
			return resolveIncomplete(ctx, e)
		}
		r.restoreObjects = objects
	}
	return r, nil
}
func (s *Store) restoreObjectMetadata(ctx context.Context, tx pgx.Tx) ([]model.Attachment, error) {
	var ids []string
	e := tx.QueryRow(ctx, `SELECT COALESCE(array_agg(object_id ORDER BY object_id),ARRAY[]::text[]) FROM `+s.table("attachments")+` WHERE state IN ('verified','referenced') AND deleted_at IS NULL`).Scan(&ids)
	if e != nil {
		return nil, e
	}
	if len(ids) > enterprisebackup.MaxObjects {
		return nil, errors.New("too many backup objects")
	}
	result := make([]model.Attachment, 0, len(ids))
	for _, id := range ids {
		a, e := s.readAttachmentState(ctx, tx, id, lockNone)
		if e != nil {
			return nil, e
		}
		result = append(result, a.meta)
	}
	return result, nil
}
func (s *Store) applyRestorePlan(ctx context.Context, tx pgx.Tx, p opPlan, d *restoreDependencies) (Result, error) {
	m := p.restore
	now := time.Now().UTC().Truncate(time.Microsecond)
	if m.kind == "export" {
		if d.pending {
			return refused(ClassRestorePending, nil)
		}
		for _, name := range enterprisebackup.Tables() {
			e := m.writer.WriteTable(ctx, name, func(emit func(json.RawMessage) error) error {
				rows, e := tx.Query(ctx, `SELECT to_jsonb(t)::text FROM `+s.table(name)+` t ORDER BY to_jsonb(t)::text`)
				if e != nil {
					return e
				}
				defer rows.Close()
				for rows.Next() {
					var raw string
					if e = rows.Scan(&raw); e != nil {
						return e
					}
					if e = emit(json.RawMessage(raw)); e != nil {
						return e
					}
				}
				return rows.Err()
			})
			if e != nil {
				return controlStatementFailure(s, ctx, e)
			}
		}
		objects, e := s.restoreObjectMetadata(ctx, tx)
		if e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		m.objects = objects
		return Result{Outcome: OutcomeCommitted}, nil
	}
	if !d.missing && d.digest != m.digest {
		return controlConflict()
	}
	if m.kind == "begin" {
		if !d.missing {
			return restoreResult(m, d, OutcomeReplayed), nil
		}
		if !d.pristine || d.pending {
			return refused(ClassInvalidControlState, nil)
		}
		if s.recoveryStarted || s.recoverySealed {
			return refused(ClassStartupRecovery, errors.New("restore requires an unused offline Store incarnation"))
		}
		epoch, e := controlID("restore")
		if e != nil {
			return refused(ClassStateInvalid, e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO `+s.table("restore_runs")+`(restore_id,manifest_hash,restore_epoch,started_at) VALUES($1,$2,$3,$4)`, m.id, m.digest[:], epoch, now); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		d.missing = false
		d.digest = m.digest
		d.epoch = epoch
		return restoreResult(m, d, OutcomeCommitted), nil
	}
	if d.missing {
		return refused(ClassRestorePending, nil)
	}
	if m.kind == "complete" {
		if d.complete {
			return restoreResult(m, d, OutcomeReplayed), nil
		}
		if !d.imported || !s.recoveryCompleted.Load() {
			return refused(ClassStartupRecovery, nil)
		}
		var recovered bool
		if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("startup_recovery_runs")+` recovery JOIN `+s.table("restore_runs")+` restored ON restored.restore_id=$2 WHERE recovery.ownership_epoch=$1 AND recovery.completed_at>=restored.imported_at)`, s.lease.epoch, m.id).Scan(&recovered); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		if !recovered {
			return refused(ClassStartupRecovery, nil)
		}
		if _, e := tx.Exec(ctx, `UPDATE `+s.table("restore_runs")+` SET completed_at=$2 WHERE restore_id=$1 AND completed_at IS NULL`, m.id, now); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
		d.complete = true
		return restoreResult(m, d, OutcomeCommitted), nil
	}
	if m.kind != "import" || m.bundle == nil {
		return refused(ClassInvalidRequest, nil)
	}
	if d.imported {
		return restoreResult(m, d, OutcomeReplayed), nil
	}
	if !d.pristine {
		return refused(ClassInvalidControlState, nil)
	}
	if e := s.importRestoreRows(ctx, tx, m.bundle, d.epoch, now); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	if _, e := tx.Exec(ctx, `UPDATE `+s.table("restore_runs")+` SET imported_at=$2 WHERE restore_id=$1 AND imported_at IS NULL`, m.id, now); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	d.imported = true
	return restoreResult(m, d, OutcomeCommitted), nil
}

// This order and column allowlist are compiled into the binary. Data selects
// neither SQL text nor DDL. All rows, revocations and the barrier share COMMIT.
var restoreTableOrder = []string{"users", "trust_connections", "external_identities", "devices", "agents", "projects", "rooms", "room_members", "slot_generations", "room_slots", "bindings", "room_events", "operations", "audit", "identity_policy", "identity_admissions", "organization_admins", "browser_sessions", "control_events", "control_operations", "relay_operations", "device_enrollments", "device_families", "device_grants", "binding_grants", "governance_actions", "governance_operations", "binding_requests", "binding_operations", "project_members", "catalog_operations", "attachments", "attachment_usage", "attachment_references", "attachment_operations", "attachment_backup_barriers"}

func validateRestoreAnchors(ctx context.Context, b *enterprisebackup.Bundle) error {
	header := b.Manifest().Header
	for _, name := range []string{"organizations", "configured_org", "deployment", "identity_policy"} {
		count := 0
		e := b.TableRows(ctx, name, func(raw json.RawMessage) error {
			count++
			var row struct {
				ID         bool   `json:"id"`
				Org        string `json:"org_id"`
				Deployment string `json:"deployment_id"`
				Epoch      string `json:"restore_epoch"`
			}
			if json.Unmarshal(raw, &row) != nil {
				return enterprisebackup.ErrInvalid
			}
			switch name {
			case "organizations":
				if row.Org != header.OrgID {
					return enterprisebackup.ErrInvalid
				}
			case "configured_org":
				if !row.ID || row.Org != header.OrgID {
					return enterprisebackup.ErrInvalid
				}
			case "deployment":
				if !row.ID || row.Org != header.OrgID || row.Deployment != header.DeploymentID {
					return enterprisebackup.ErrInvalid
				}
			case "identity_policy":
				if !row.ID || row.Epoch != header.RestoreEpoch {
					return enterprisebackup.ErrInvalid
				}
			}
			return nil
		})
		if e != nil {
			return e
		}
		if count != 1 {
			return enterprisebackup.ErrInvalid
		}
	}
	return nil
}

func (s *Store) importRestoreRows(ctx context.Context, tx pgx.Tx, b *enterprisebackup.Bundle, epoch string, at time.Time) error {
	if e := validateRestoreAnchors(ctx, b); e != nil {
		return e
	}
	header := b.Manifest().Header
	var org string
	if e := tx.QueryRow(ctx, `SELECT org_id FROM `+s.table("deployment")+` WHERE id`).Scan(&org); e != nil {
		return e
	}
	if org != header.OrgID {
		return enterprisebackup.ErrInvalid
	}
	// Organization disable is policy, not an import-time default.
	if e := b.TableRows(ctx, "organizations", func(raw json.RawMessage) error {
		var row struct {
			Org      string `json:"org_id"`
			Disabled bool   `json:"disabled"`
		}
		if json.Unmarshal(raw, &row) != nil || row.Org != org {
			return enterprisebackup.ErrInvalid
		}
		_, e := tx.Exec(ctx, `UPDATE `+s.table("organizations")+` SET disabled=$2 WHERE org_id=$1`, org, row.Disabled)
		return e
	}); e != nil {
		return e
	}
	for _, name := range restoreTableOrder {
		columns := enterprisebackup.Columns(name)
		quoted := make([]string, len(columns))
		for i, col := range columns {
			quoted[i] = pgx.Identifier{col}.Sanitize()
		}
		cols := strings.Join(quoted, ",")
		query := `INSERT INTO ` + s.table(name) + ` (` + cols + `) SELECT ` + cols + ` FROM pg_catalog.json_populate_record(NULL::` + s.table(name) + `,$1::json)`
		if e := b.TableRows(ctx, name, func(raw json.RawMessage) error { _, e := tx.Exec(ctx, query, string(raw)); return e }); e != nil {
			return e
		}
	}
	for _, name := range []string{"browser_sessions", "device_families", "device_grants", "binding_grants"} {
		if _, e := tx.Exec(ctx, `UPDATE `+s.table(name)+` SET revoked_at=COALESCE(revoked_at,$1)`, at); e != nil {
			return e
		}
	}
	for _, name := range []string{"device_enrollments", "binding_requests"} {
		if _, e := tx.Exec(ctx, `UPDATE `+s.table(name)+` SET status='invalidated' WHERE status IN ('pending','approved')`); e != nil {
			return e
		}
	}
	if _, e := tx.Exec(ctx, `UPDATE `+s.table("attachment_backup_barriers")+` SET ended_at=COALESCE(ended_at,$1)`, at); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, `UPDATE `+s.table("identity_policy")+` SET restore_epoch=$1,revision=revision+1 WHERE id`, epoch); e != nil {
		return e
	}
	if _, e := tx.Exec(ctx, `UPDATE `+s.table("governance_actions")+` SET invalidated_at=COALESCE(invalidated_at,$1) WHERE consumed_at IS NULL`, at); e != nil {
		return e
	}
	// Preserve old history without manufacturing a governance consumption.
	// Revoked sessions permanently invalidate every old governance proof.
	for _, item := range [][2]string{{"audit", "audit_seq"}, {"control_events", "seq"}} {
		table, col := item[0], item[1]
		if _, e := tx.Exec(ctx, `SELECT pg_catalog.setval(pg_catalog.pg_get_serial_sequence($1,$2),COALESCE((SELECT max(`+col+`) FROM `+s.table(table)+`),1),(SELECT count(*)>0 FROM `+s.table(table)+`))`, s.table(table), col); e != nil {
			return e
		}
	}
	objects, e := s.restoreObjectMetadata(ctx, tx)
	if e != nil {
		return e
	}
	expected := b.Manifest().Objects
	if len(objects) != len(expected) {
		return errors.New("backup object manifest does not cover SQL")
	}
	for i := range objects {
		if !objects[i].EqualMetadata(expected[i]) {
			return errors.New("backup object manifest disagrees with SQL")
		}
	}
	_, e = tx.Exec(ctx, `INSERT INTO `+s.table("audit")+`(deployment_id,actor_kind,kind,outcome,detail,at) VALUES($1,'operator','deployment.restore','accepted',$2,$3)`, s.cfg.DeploymentID, fmt.Sprintf("manifest-sha256:%x", b.Digest()), at)
	return e
}
