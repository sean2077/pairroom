package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
)

func attachmentPGFixture(t *testing.T, transform func(string) string) (*bindingPGState, *enterpriseobject.Store) {
	t.Helper()
	b := bindingPGFixture(t, transform)
	limits := AttachmentLimits{DeploymentBytes: 1024, RoomBytes: 512, DeploymentPending: 16, RoomPending: 8}
	b.f.cfg.AttachmentLimits = limits
	b.f.store.cfg.AttachmentLimits = limits
	objects, err := enterpriseobject.Open(filepath.Join(t.TempDir(), "objects"), b.f.cfg.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	return b, objects
}
func attachmentPGReserve(auth AuthContext, id string, data []byte) AttachmentReserveCommand {
	sum := sha256.Sum256(data)
	return AttachmentReserveCommand{Auth: auth, RoomID: "binding-room", ClientID: id, Spec: AttachmentSpec{Name: "evidence.txt", Kind: "file", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}}
}
func attachmentPGVerified(t *testing.T, b *bindingPGState, objects *enterpriseobject.Store, auth AuthContext, id string, data []byte) (AttachmentReserveCommand, model.Attachment) {
	t.Helper()
	c := attachmentPGReserve(auth, id, data)
	r, err := b.f.store.ReserveAttachment(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	v, err := objects.PutImmutable(t.Context(), r.Attachment.ObjectID, c.Spec, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	r, err = b.f.store.VerifyAttachment(t.Context(), AttachmentVerifyCommand{Auth: auth, RoomID: c.RoomID, ClientID: "verify-" + id, ObjectID: r.Attachment.ObjectID, Verified: v})
	if err != nil {
		t.Fatal(err)
	}
	return c, r.Attachment.Metadata
}
func attachmentPGCounts(t *testing.T, b *bindingPGState) [7]int64 {
	t.Helper()
	var n [7]int64
	err := b.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM attachments),(SELECT count(*) FROM attachment_operations),(SELECT count(*) FROM attachment_references),(SELECT count(*) FROM audit),(SELECT COALESCE(sum(bytes),0) FROM attachment_usage WHERE scope='deployment'),(SELECT COALESCE(sum(pending),0) FROM attachment_usage WHERE scope='deployment'),(SELECT current_seq FROM rooms WHERE room_id='binding-room')`).Scan(&n[0], &n[1], &n[2], &n[3], &n[4], &n[5], &n[6])
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestAttachmentReservationVerificationAndReadOnlyRecovery(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	c, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "first", []byte("private evidence"))
	before := attachmentPGCounts(t, b)
	for range 2 {
		r, e := b.f.store.ResolveAttachmentReservation(t.Context(), c)
		if e != nil || r.Outcome != OutcomeReplayed || r.Attachment.ObjectID != meta.ID || r.Attachment.State != "reserved" {
			t.Fatalf("reserve resolve: %+v %v", r, e)
		}
	}
	if err := objects.Delete(t.Context(), meta.ID); err != nil {
		t.Fatal(err)
	}
	r, e := b.f.store.ResolveAttachmentVerification(t.Context(), AttachmentObjectCommand{Auth: b.owner.Auth, RoomID: c.RoomID, ClientID: "verify-first", ObjectID: meta.ID})
	if e != nil || r.Outcome != OutcomeReplayed || !r.Attachment.Metadata.EqualMetadata(meta) {
		t.Fatalf("verification recovery depended on bytes: %+v %v", r, e)
	}
	if got := attachmentPGCounts(t, b); got != before {
		t.Fatalf("Resolve wrote effects: %v -> %v", before, got)
	}
	changed := c
	changed.Spec.Name = "different.txt"
	r, e = b.f.store.ReserveAttachment(t.Context(), changed)
	if k := kernelErr(t, r, e); k.Class != ClassPayloadConflict {
		t.Fatal(r)
	}
	if _, e = b.f.store.RevokeSession(t.Context(), b.owner.Auth, "revoke-attachment-reader"); e != nil {
		t.Fatal(e)
	}
	r, e = b.f.store.ResolveAttachmentReservation(t.Context(), c)
	if k := kernelErr(t, r, e); k.Class != ClassAuthorization || r.Attachment != nil {
		t.Fatal("revoked reader recovered receipt")
	}
}
func TestAttachmentRoomAndCredentialScope(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	_, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "scope", []byte("draft"))
	for _, auth := range []AuthContext{b.member.Auth, b.device.Auth, b.admin.Auth} {
		if _, e := b.f.store.ReadAttachment(t.Context(), auth, "binding-room", meta.ID); e == nil {
			t.Fatal("foreign scope read draft")
		}
	}
	if _, e := b.f.store.ReadAttachment(t.Context(), b.owner.Auth, "room-1", meta.ID); e == nil {
		t.Fatal("cross-Room object read")
	}
	r, e := b.f.store.ReserveAttachment(t.Context(), attachmentPGReserve(b.device.Auth, "device-cannot-upload", []byte("x")))
	if k := kernelErr(t, r, e); k.Class != ClassAuthorization {
		t.Fatal(r)
	}
	request := bindingPGCommand(b, "attachment", model.ActorSlot1, 1, false)
	_, grant := bindingPGComplete(t, b, request)
	_, bound := attachmentPGVerified(t, b, objects, grant.Auth, "narrow-upload", []byte("bound evidence"))
	if _, e = b.f.store.ReadAttachment(t.Context(), grant.Auth, "binding-room", bound.ID); e != nil {
		t.Fatal(e)
	}
	if _, e = b.f.store.ReadAttachment(t.Context(), b.member.Auth, "binding-room", bound.ID); e == nil {
		t.Fatal("browser adopted unreferenced Binding draft")
	}
	if _, e = b.f.store.RevokeDevice(t.Context(), b.member.Auth, "revoke-upload-device", b.device.DeviceID); e != nil {
		t.Fatal(e)
	}
	if _, e = b.f.store.ReadAttachment(t.Context(), grant.Auth, "binding-room", bound.ID); e == nil {
		t.Fatal("revoked narrow grant read object")
	}
}
func TestAttachmentHumanMessageSharesOriginalKeyAndAtomicReferences(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	bindingPGComplete(t, b, bindingPGCommand(b, "human-target", model.ActorSlot1, 1, false))
	_, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "send", []byte("message evidence"))
	var revision uint64
	if e := b.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); e != nil {
		t.Fatal(e)
	}
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	c := HumanMessageCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: "human-shared-key", ExpectedRevision: revision, Intent: HumanSendIntent{To: "slot1", AttachmentIDs: ids}}
	before := attachmentPGCounts(t, b)
	r, e := b.f.store.CommitHumanMessage(t.Context(), c)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("send: %+v %v", r, e)
	}
	payload, e := decodeHumanSendPayload(r.Fact.Data)
	if e != nil || len(payload.Attachments) != 1 || !payload.Attachments[0].EqualMetadata(meta) {
		t.Fatal("message did not preserve SQL metadata")
	}
	after := attachmentPGCounts(t, b)
	if after[2] != before[2]+1 || after[5] != before[5]-1 || after[6] != before[6]+1 || after[4] != before[4] {
		t.Fatalf("reference/quota/fact not atomic: %v -> %v", before, after)
	}
	old, e := b.f.store.CommitHumanSend(t.Context(), HumanSendCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: c.ClientID, Intent: c.Intent, Fact: r.Fact})
	if e != nil || old.Outcome != OutcomeReplayed || !reflect.DeepEqual(old.Fact, r.Fact) {
		t.Fatalf("old path duplicated key: %+v %v", old, e)
	}
	if _, e = b.f.store.ReadAttachment(t.Context(), b.member.Auth, "binding-room", meta.ID); e != nil {
		t.Fatal("Room reader cannot read referenced object")
	}
	c.Intent.Text = "changed"
	changed, e := b.f.store.CommitHumanMessage(t.Context(), c)
	if k := kernelErr(t, changed, e); k.Class != ClassPayloadConflict {
		t.Fatal(changed)
	}
	if attachmentPGCounts(t, b) != after {
		t.Fatal("replay/conflict changed quota/references")
	}
}
func TestAttachmentAuditFailuresLeaveNoPartialReservationOrReference(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	c := attachmentPGReserve(b.owner.Auth, "audit-reserve", []byte("reserved"))
	before := attachmentPGCounts(t, b)
	if _, e := b.f.admin.Exec(t.Context(), `ALTER TABLE audit ADD CONSTRAINT reject_attachment_reserve CHECK(kind<>'attachment.reserve') NOT VALID`); e != nil {
		t.Fatal(e)
	}
	r, e := b.f.store.ReserveAttachment(t.Context(), c)
	if k := kernelErr(t, r, e); k.Class != ClassConstraintViolation {
		t.Fatal(r)
	}
	if attachmentPGCounts(t, b) != before {
		t.Fatal("audit failure left reservation/quota")
	}
	if _, e = b.f.admin.Exec(t.Context(), `ALTER TABLE audit DROP CONSTRAINT reject_attachment_reserve`); e != nil {
		t.Fatal(e)
	}
	bindingPGComplete(t, b, bindingPGCommand(b, "audit-target", model.ActorSlot1, 1, false))
	_, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "audit-message", []byte("evidence"))
	var revision uint64
	b.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision)
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	before = attachmentPGCounts(t, b)
	if _, e = b.f.admin.Exec(t.Context(), `ALTER TABLE audit ADD CONSTRAINT reject_attachment_message CHECK(kind<>'fact.append') NOT VALID`); e != nil {
		t.Fatal(e)
	}
	r, e = b.f.store.CommitHumanMessage(t.Context(), HumanMessageCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: "atomic-attachment-fact", ExpectedRevision: revision, Intent: HumanSendIntent{To: "slot1", Text: "evidence", AttachmentIDs: ids}})
	if k := kernelErr(t, r, e); k.Class != ClassConstraintViolation {
		t.Fatal(r)
	}
	if attachmentPGCounts(t, b) != before {
		t.Fatal("audit failure left reference/fact/quota")
	}
	var state string
	if e = b.f.admin.QueryRow(t.Context(), `SELECT state FROM attachments WHERE object_id=$1`, meta.ID).Scan(&state); e != nil || state != "verified" {
		t.Fatal("audit failure changed draft")
	}
}
func TestAttachmentQuotaUsesDatabaseGuardAcrossWorkConnections(t *testing.T) {
	b, _ := attachmentPGFixture(t, nil)
	b.f.cfg.AttachmentLimits = AttachmentLimits{DeploymentBytes: 8, RoomBytes: 8, DeploymentPending: 1, RoomPending: 1}
	b.f.store.cfg.AttachmentLimits = b.f.cfg.AttachmentLimits
	work := schemaConn(t, b.f.dsn, b.f.schema)
	if e := configureSession(t.Context(), work, b.f.schema); e != nil {
		t.Fatal(e)
	}
	other := &Store{cfg: b.f.cfg, lease: b.f.store.lease, work: work, sem: make(chan struct{}, 1)}
	stores := []*Store{b.f.store, other}
	start := make(chan struct{})
	var wg sync.WaitGroup
	var results [2]Result
	var errs [2]error
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = stores[i].ReserveAttachment(context.Background(), attachmentPGReserve(b.owner.Auth, []string{"race-one", "race-two"}[i], []byte("eightbit")))
		}(i)
	}
	close(start)
	wg.Wait()
	wins := 0
	for i, r := range results {
		if r.Outcome == OutcomeCommitted && errs[i] == nil {
			wins++
		} else if k := kernelErr(t, r, errs[i]); k.Class != ClassRangeExceeded {
			t.Fatal(r)
		}
	}
	if wins != 1 {
		t.Fatal(results)
	}
	counts := attachmentPGCounts(t, b)
	if counts[0] != 1 || counts[4] != 8 || counts[5] != 1 {
		t.Fatal(counts)
	}
}
func TestAttachmentUnknownReservationBothRealCommitOutcomes(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			b, _ := attachmentPGFixture(t, proxy.dsn)
			c := attachmentPGReserve(b.owner.Auth, "unknown-reserve", []byte("evidence"))
			before := attachmentPGCounts(t, b)
			proxy.arm()
			r, e := b.f.store.ReserveAttachment(t.Context(), c)
			if k := kernelErr(t, r, e); k.Outcome != OutcomeUnknown || r.Attachment != nil {
				t.Fatal("unknown disclosed object candidate")
			}
			resolved, e := b.f.store.ResolveAttachmentReservation(t.Context(), c)
			if e != nil {
				t.Fatal(e)
			}
			after := attachmentPGCounts(t, b)
			if mode == "drop-response" {
				if resolved.Outcome != OutcomeReplayed || resolved.Attachment == nil || after[0] != before[0]+1 {
					t.Fatal(resolved)
				}
			} else if resolved.Outcome != OutcomeNotCommitted || resolved.Attachment != nil || after != before {
				t.Fatal(resolved)
			}
			if attachmentPGCounts(t, b) != after {
				t.Fatal("Resolve retried reservation")
			}
		})
	}
}
func TestAttachmentRetentionBackupAndDeletionQuotaRelease(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	c, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "collect", []byte("retained bytes"))
	command := AttachmentObjectCommand{Auth: b.owner.Auth, RoomID: c.RoomID, ClientID: "cancel-collect", ObjectID: meta.ID}
	if _, e := b.f.store.CancelAttachment(t.Context(), command); e != nil {
		t.Fatal(e)
	}
	before := attachmentPGCounts(t, b)
	if report, e := b.f.store.CollectAttachments(t.Context(), objects, 32); e != nil || report.Deleted != 0 {
		t.Fatalf("grace not enforced: %+v %v", report, e)
	}
	// Only the test changes the clock boundary; production never rewrites it.
	if _, e := b.f.admin.Exec(t.Context(), `ALTER TABLE attachments DISABLE TRIGGER attachments_times_once; UPDATE attachments SET abandoned_at=pg_catalog.clock_timestamp()-interval '8 days'; ALTER TABLE attachments ENABLE TRIGGER attachments_times_once`); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.store.BeginAttachmentBackup(t.Context(), objects, "manifest-one"); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.store.CollectAttachments(t.Context(), objects, 32); e == nil {
		t.Fatal("active backup allowed GC")
	}
	if _, e := objects.Inspect(t.Context(), meta.ID); e != nil {
		t.Fatal("backup lost bytes")
	}
	if _, e := b.f.store.EndAttachmentBackup(t.Context(), objects, "manifest-one"); e != nil {
		t.Fatal(e)
	}
	report, e := b.f.store.CollectAttachments(t.Context(), objects, 32)
	if e != nil || report.Deleted != 1 {
		t.Fatalf("GC: %+v %v", report, e)
	}
	after := attachmentPGCounts(t, b)
	if after[4] != before[4]-meta.Size || after[5] != 0 {
		t.Fatal("quota released at wrong boundary")
	}
	if _, e = objects.PutImmutable(t.Context(), meta.ID, c.Spec, bytes.NewBufferString("retained bytes")); e == nil {
		t.Fatal("late upload resurrected deleted bytes")
	}
	if report, e = b.f.store.CollectAttachments(t.Context(), objects, 32); e != nil || report.Deleted != 0 {
		t.Fatal("GC released quota twice")
	}
}

func TestAttachmentVerificationUnknownDoesNotNeedCurrentObject(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			b, objects := attachmentPGFixture(t, proxy.dsn)
			c := attachmentPGReserve(b.owner.Auth, "unknown-verify", []byte("verified bytes"))
			reserved, e := b.f.store.ReserveAttachment(t.Context(), c)
			if e != nil {
				t.Fatal(e)
			}
			id := reserved.Attachment.ObjectID
			v, e := objects.PutImmutable(t.Context(), id, c.Spec, bytes.NewBufferString("verified bytes"))
			if e != nil {
				t.Fatal(e)
			}
			before := attachmentPGCounts(t, b)
			proxy.arm()
			r, e := b.f.store.VerifyAttachment(t.Context(), AttachmentVerifyCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: "verify-unknown", ObjectID: id, Verified: v})
			if k := kernelErr(t, r, e); k.Outcome != OutcomeUnknown || r.Attachment != nil {
				t.Fatal(r)
			}
			if e = objects.Delete(t.Context(), id); e != nil {
				t.Fatal(e)
			}
			r, e = b.f.store.ResolveAttachmentVerification(t.Context(), AttachmentObjectCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: "verify-unknown", ObjectID: id})
			if e != nil {
				t.Fatal(e)
			}
			after := attachmentPGCounts(t, b)
			if mode == "drop-response" {
				if r.Outcome != OutcomeReplayed || r.Attachment.State != "verified" || after[1] != before[1]+1 {
					t.Fatal(r)
				}
			} else if r.Outcome != OutcomeNotCommitted || r.Attachment != nil || after != before {
				t.Fatal(r)
			}
			if attachmentPGCounts(t, b) != after {
				t.Fatal("verification Resolve wrote effects")
			}
		})
	}
}
func TestAttachmentExpiredReservationIsAbandonedBeforeGrace(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	c := attachmentPGReserve(b.owner.Auth, "expired", []byte("expired bytes"))
	r, e := b.f.store.ReserveAttachment(t.Context(), c)
	if e != nil {
		t.Fatal(e)
	}
	id := r.Attachment.ObjectID
	if _, e = b.f.admin.Exec(t.Context(), `ALTER TABLE attachments DISABLE TRIGGER attachments_identity_immutable; UPDATE attachments SET created_at=pg_catalog.clock_timestamp()-interval '2 hours',expires_at=pg_catalog.clock_timestamp()-interval '1 hour'; ALTER TABLE attachments ENABLE TRIGGER attachments_identity_immutable`); e != nil {
		t.Fatal(e)
	}
	if _, e = b.f.store.ReadUploadReservation(t.Context(), c.Auth, c.RoomID, id); e == nil {
		t.Fatal("expired reservation accepted upload")
	}
	before := attachmentPGCounts(t, b)
	report, e := b.f.store.CollectAttachments(t.Context(), objects, 32)
	if e != nil || report.Abandoned != 1 || report.Deleted != 0 {
		t.Fatalf("expire: %+v %v", report, e)
	}
	after := attachmentPGCounts(t, b)
	if after[4] != before[4] || after[5] != 0 {
		t.Fatal("expiry released bytes before physical deletion")
	}
	if report, e = b.f.store.CollectAttachments(t.Context(), objects, 32); e != nil || report.Examined != 0 {
		t.Fatal("expiry skipped grace or repeated")
	}
}
func TestAttachmentGCPartialFailureResumesWithoutDoubleRelease(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	c, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "partial-gc", []byte("retained"))
	if _, e := b.f.store.CancelAttachment(t.Context(), AttachmentObjectCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: "cancel-partial", ObjectID: meta.ID}); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.admin.Exec(t.Context(), `ALTER TABLE attachments DISABLE TRIGGER attachments_times_once; UPDATE attachments SET abandoned_at=pg_catalog.clock_timestamp()-interval '8 days'; ALTER TABLE attachments ENABLE TRIGGER attachments_times_once`); e != nil {
		t.Fatal(e)
	}
	before := attachmentPGCounts(t, b)
	if e := objects.BeginBackup(t.Context(), "disk-only-barrier"); e != nil {
		t.Fatal(e)
	}
	if _, e := b.f.store.CollectAttachments(t.Context(), objects, 32); e == nil {
		t.Fatal("physical deletion ignored retention")
	}
	var state string
	if e := b.f.admin.QueryRow(t.Context(), `SELECT state FROM attachments WHERE object_id=$1`, meta.ID).Scan(&state); e != nil || state != "deletion_pending" {
		t.Fatal("logical mark did not precede deletion")
	}
	if attachmentPGCounts(t, b)[4] != before[4] {
		t.Fatal("failed physical delete released quota")
	}
	if e := objects.EndBackup(t.Context(), "disk-only-barrier"); e != nil {
		t.Fatal(e)
	}
	report, e := b.f.store.CollectAttachments(t.Context(), objects, 32)
	if e != nil || report.Deleted != 1 {
		t.Fatal(report, e)
	}
	if attachmentPGCounts(t, b)[4] != 0 {
		t.Fatal("resume did not release quota")
	}
	if _, e = b.f.store.ResolveAttachmentCancellation(t.Context(), AttachmentObjectCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: "cancel-partial", ObjectID: meta.ID}); e != nil {
		t.Fatal("historical cancellation receipt disappeared")
	}
}
func TestAttachmentRejectsRehashedReceiptSemanticCorruption(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	c, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "corrupt", []byte("evidence"))
	original, e := b.f.store.ResolveAttachmentVerification(t.Context(), AttachmentObjectCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: "verify-corrupt", ObjectID: meta.ID})
	if e != nil {
		t.Fatal(e)
	}
	receipt := *original.Attachment
	receipt.State = "referenced"
	encoding, e := json.Marshal(receipt)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(encoding)
	// Use separate fixture statements so no driver interpolation is required.
	if _, e = b.f.admin.Exec(t.Context(), `ALTER TABLE attachment_operations DISABLE TRIGGER attachment_operations_immutable`); e != nil {
		t.Fatal(e)
	}
	if _, e = b.f.admin.Exec(t.Context(), `UPDATE attachment_operations SET result_encoding=$1,result_hash=$2 WHERE op_kind='verify'`, encoding, sum[:]); e != nil {
		t.Fatal(e)
	}
	if _, e = b.f.admin.Exec(t.Context(), `ALTER TABLE attachment_operations ENABLE TRIGGER attachment_operations_immutable`); e != nil {
		t.Fatal(e)
	}
	r, e := b.f.store.ResolveAttachmentVerification(t.Context(), AttachmentObjectCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: "verify-corrupt", ObjectID: meta.ID})
	if k := kernelErr(t, r, e); k.Class != ClassStoredFactUnreadable || r.Attachment != nil {
		t.Fatal(r)
	}
}
