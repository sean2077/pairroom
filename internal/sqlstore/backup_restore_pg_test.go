package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
)

func backupPGPristine(t *testing.T) *fixture {
	t.Helper()
	dsn := testDSN(t)
	cfg, _ := initializeFixture(t, dsn)
	if e := InitializeDeployment(t.Context(), cfg, "org-test"); e != nil {
		t.Fatal(e)
	}
	admin := schemaConn(t, dsn, cfg.Schema)
	s, e := Open(t.Context(), cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &fixture{store: s, cfg: cfg, dsn: dsn, schema: cfg.Schema, admin: admin}
}
func backupPGSource(t *testing.T) (*bindingPGState, *enterpriseobject.Store) {
	t.Helper()
	f := backupPGPristine(t)
	if _, e := f.store.RecoverDeliveries(t.Context()); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.InitializeIdentity(t.Context(), identityPGConfig()); e != nil {
		t.Fatal(e)
	}
	_, owner := identityPGLogin(t, f, "member-one", "backup-owner")
	_, admin := identityPGLogin(t, f, "operator-selected", "backup-admin")
	c, device := devicePGEnroll(t, f, owner, "backup-device")
	devicePGSeedBinding(t, f, owner, c, device, "binding-room", true)
	limits := AttachmentLimits{DeploymentBytes: 1024, RoomBytes: 512, DeploymentPending: 16, RoomPending: 8}
	f.cfg.AttachmentLimits = limits
	f.store.cfg.AttachmentLimits = limits
	objects, e := enterpriseobject.Open(filepath.Join(t.TempDir(), "objects"), f.cfg.DeploymentID)
	if e != nil {
		t.Fatal(e)
	}
	return &bindingPGState{f: f, owner: owner, member: owner, admin: admin, device: device, enrollment: c}, objects
}
func TestBackupRestoreAtomicRevocationFreshEpochAndObjectBoundary(t *testing.T) {
	ctx := context.Background()
	b, objects := backupPGSource(t)
	_, referenced := attachmentPGVerified(t, b, objects, b.owner.Auth, "referenced", []byte("retained history evidence"))
	_, draft := attachmentPGVerified(t, b, objects, b.owner.Auth, "draft", []byte("old verified draft"))
	reserve, e := b.f.store.ReserveAttachment(ctx, attachmentPGReserve(b.owner.Auth, "unuploaded", []byte("unuploaded")))
	if e != nil {
		t.Fatal(e)
	}
	var revision uint64
	if e = b.f.admin.QueryRow(ctx, `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); e != nil {
		t.Fatal(e)
	}
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{referenced.ID})
	if _, e = b.f.store.CommitHumanMessage(ctx, HumanMessageCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: "with-reference", ExpectedRevision: revision, Intent: HumanSendIntent{To: "slot1", AttachmentIDs: ids, Text: "durable evidence"}}); e != nil {
		t.Fatal(e)
	}
	pending := devicePGCommand("before-restore-pending")
	approved := devicePGCommand("before-restore-approved")
	for _, c := range []DeviceEnrollmentCommand{pending, approved} {
		if _, e = b.f.store.BeginDeviceEnrollment(ctx, c); e != nil {
			t.Fatal(e)
		}
	}
	code := sha256.Sum256([]byte("old approval"))
	if _, e = b.f.store.DecideDeviceEnrollment(ctx, DeviceEnrollmentDecision{Auth: b.owner.Auth, ClientID: "approve-before-restore", RequestID: approved.RequestID, Approve: true, CodeHash: code}); e != nil {
		t.Fatal(e)
	}
	bundlePath := filepath.Join(t.TempDir(), "bundle")
	manifest, e := b.f.store.BackupOffline(ctx, objects, bundlePath, "backup-once")
	if e != nil {
		t.Fatal(e)
	}
	if len(manifest.Objects) != 2 {
		t.Fatal("unuploaded draft was treated as required bytes")
	}
	bundle, e := enterprisebackup.Open(ctx, bundlePath, b.f.cfg.DeploymentID, "org-test")
	if e != nil {
		t.Fatal(e)
	}
	if e = b.f.store.Close(); e != nil {
		t.Fatal(e)
	}
	target := backupPGPristine(t)
	target.cfg.AttachmentLimits = b.f.cfg.AttachmentLimits
	target.store.cfg.AttachmentLimits = target.cfg.AttachmentLimits
	targetObjects, e := enterpriseobject.Open(filepath.Join(t.TempDir(), "restored-objects"), target.cfg.DeploymentID)
	if e != nil {
		t.Fatal(e)
	}
	r, e := target.store.RestoreOffline(ctx, targetObjects, bundle, "restore-once")
	if e != nil || r.Outcome != OutcomeCommitted || r.Restore == nil || !r.Restore.Imported || r.Restore.Complete {
		t.Fatalf("restore: %+v %v", r, e)
	}
	if e = target.store.EnsureRestoreReady(ctx); e == nil {
		t.Fatal("SQL restore barrier permitted listener")
	}
	if _, e = target.store.CompleteRestore(ctx, targetObjects, "restore-once", bundle.Digest()); e == nil {
		t.Fatal("completed before new Recovery")
	}
	// Simulate loss of local process/journal: durable PostgreSQL barrier remains.
	if e = target.store.Close(); e != nil {
		t.Fatal(e)
	}
	target.store, e = Open(ctx, target.cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = target.store.Close() })
	if e = target.store.EnsureRestoreReady(ctx); e == nil {
		t.Fatal("reopen lost SQL restore barrier")
	}
	resolved, e := target.store.ResolveRestore(ctx, "restore-once", bundle.Digest())
	if e != nil || resolved.Restore.RestoreEpoch == manifest.Header.RestoreEpoch || !resolved.Restore.Imported {
		t.Fatal("resolve changed or lost restore", e)
	}
	if _, e = target.store.RecoverDeliveries(ctx); e != nil {
		t.Fatal(e)
	}
	r, e = target.store.CompleteRestore(ctx, targetObjects, "restore-once", bundle.Digest())
	if e != nil || !r.Restore.Complete {
		t.Fatalf("complete: %+v %v", r, e)
	}
	if e = target.store.EnsureRestoreReady(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = target.store.AuthenticateDevice(ctx, b.enrollment.Credential.AccessHash, b.enrollment.Origin); e == nil {
		t.Fatal("old Device grant revived")
	}
	if _, e = target.store.CompleteDeviceEnrollment(ctx, DeviceEnrollmentCompletion{RequestID: approved.RequestID, ClientID: "late-complete", Origin: approved.Origin, ProofHash: approved.ProofHash, CodeHash: code}); e == nil {
		t.Fatal("old approved enrollment minted fresh authority")
	}
	_, fresh := identityPGLogin(t, target, "member-one", "fresh-after-restore")
	for _, id := range []string{draft.ID, reserve.Attachment.ObjectID} {
		if _, e = target.store.ReadUploadReservation(ctx, fresh.Auth, "binding-room", id); e == nil {
			t.Fatal("fresh login revived old attachment draft")
		}
	}
	got, e := target.store.ReadAttachment(ctx, fresh.Auth, "binding-room", referenced.ID)
	if e != nil || got != referenced {
		t.Fatalf("referenced history not readable: %+v %v", got, e)
	}
	data, e := targetObjects.ReadVerified(ctx, referenced)
	if e != nil || !bytes.Equal(data, []byte("retained history evidence")) {
		t.Fatal(e)
	}
	sourceData, e := objects.ReadVerified(ctx, referenced)
	if e != nil || !bytes.Equal(data, sourceData) {
		t.Fatal("restore mutated source", e)
	}
	var revoked, pendingCount int
	if e = target.admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM browser_sessions WHERE revoked_at IS NULL AND session_id<>$1),(SELECT count(*) FROM device_enrollments WHERE status IN ('pending','approved'))`, fresh.Auth.SessionID).Scan(&revoked, &pendingCount); e != nil || revoked != 0 || pendingCount != 0 {
		t.Fatal("old authorities survived", e)
	}
	before := resolved.Restore.RestoreEpoch
	r, e = target.store.RestoreOffline(ctx, targetObjects, bundle, "restore-once")
	if e != nil || r.Outcome != OutcomeReplayed || r.Restore.RestoreEpoch != before {
		t.Fatal("repeat restore mutated epoch", e)
	}
}
func TestBackupFailureRetainsBothBarriersAndPublishesNoManifest(t *testing.T) {
	b, objects := backupPGSource(t)
	_, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "missing-byte", []byte("missing"))
	if e := objects.Delete(t.Context(), meta.ID); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "failed-bundle")
	if _, e := b.f.store.BackupOffline(t.Context(), objects, path, "failed-backup"); e == nil {
		t.Fatal("reported complete backup without required object")
	}
	if _, e := os.Stat(filepath.Join(path, "manifest.json")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("published partial manifest")
	}
	var active bool
	if e := b.f.admin.QueryRow(t.Context(), `SELECT ended_at IS NULL FROM attachment_backup_barriers WHERE barrier_id='failed-backup'`).Scan(&active); e != nil || !active {
		t.Fatal("lost SQL retention barrier", e)
	}
	if e := objects.Delete(t.Context(), meta.ID); !errors.Is(e, enterpriseobject.ErrBackup) {
		t.Fatal("lost filesystem retention barrier", e)
	}
}
func TestAdministratorRoleRemovalWhileTargetDisabled(t *testing.T) {
	f := identityPGFixture(t, nil)
	_, admin := identityPGLogin(t, f, "operator-selected", "admin-policy-owner")
	_, other := identityPGLogin(t, f, "member-one", "admin-policy-other")
	if _, e := f.store.SetAdministrator(t.Context(), admin.Auth, "promote-disabled-target", other.Auth.UserID, true); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetUserEnabled(t.Context(), admin.Auth, "disable-other-admin", other.Auth.UserID, false); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetAdministrator(t.Context(), admin.Auth, "forbidden-grant", other.Auth.UserID, true); e == nil {
		t.Fatal("granted Administrator to disabled Target")
	}
	if r, e := f.store.SetAdministrator(t.Context(), admin.Auth, "remove-disabled-target", other.Auth.UserID, false); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("disabled Target role removal: %+v %v", r, e)
	}
	var enabled, disabled bool
	if e := f.admin.QueryRow(t.Context(), `SELECT a.enabled,u.disabled FROM organization_admins a JOIN users u USING(user_id) WHERE user_id=$1`, other.Auth.UserID).Scan(&enabled, &disabled); e != nil || enabled || !disabled {
		t.Fatal("removed role reenabled User", e)
	}
}
