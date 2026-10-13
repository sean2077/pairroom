package sqlstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"github.com/sean2077/pairroom/internal/enterprisebackup"
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeadlessEnrollmentGuessBudgetPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "headless-owner")
	c := devicePGCommand("headless-budget")
	c.RequestID = "request-" + strings.Repeat("a", 32)
	c.TransportMode = DeviceTransportHeadless
	c.CallbackURI = ""
	c.ClientState = ""
	c.ConfirmationHash = sha256.Sum256([]byte("correct"))
	if r, e := f.store.BeginDeviceEnrollment(ctx, c); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("begin: %+v %v", r, e)
	}
	for i := 0; i < 5; i++ {
		r, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: string(rune('a' + i)), RequestID: c.RequestID, Approve: true, ConfirmationHash: sha256.Sum256([]byte("wrong"))})
		if e != nil || r.Outcome != OutcomeCommitted {
			t.Fatalf("guess %d: %+v %v", i, r, e)
		}
		h, ok := r.HeadlessEnrollmentResult()
		want := "confirmation_rejected"
		if i == 4 {
			want = "verification_exhausted"
		}
		if !ok || h.Disposition != want {
			t.Fatalf("guess result: %+v", h)
		}

	}
	f.store.Close()
	var e error
	f.store, e = Open(ctx, f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.store.Close() })
	r, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "after-restart", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash})
	if e == nil || r.Outcome == OutcomeCommitted {
		t.Fatalf("exhaustion reset %+v %v", r, e)
	}
	r, e = f.store.BeginDeviceEnrollment(ctx, c)
	assertIdentityAuthorization(t, r, e)
	counts := devicePGCounts(t, f)
	if counts[1] != 0 || counts[2] != 0 || counts[3] != 0 {
		t.Fatalf("guess minted credentials %v", counts)
	}
}
func headlessPGCommand(id string) DeviceEnrollmentCommand {
	c := devicePGCommand(id)
	sum := sha256.Sum256([]byte(id))
	c.RequestID = "request-" + hex.EncodeToString(sum[:16])
	c.TransportMode = DeviceTransportHeadless
	c.CallbackURI = ""
	c.ClientState = ""
	c.ConfirmationHash = sha256.Sum256([]byte("correct-" + id))
	return c
}
func headlessBegin(t *testing.T, f *fixture, id string) DeviceEnrollmentCommand {
	t.Helper()
	c := headlessPGCommand(id)
	r, e := f.store.BeginDeviceEnrollment(context.Background(), c)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("begin: %+v %v", r, e)
	}
	h, ok := r.HeadlessEnrollmentResult()
	if !ok || h.Sequence != 0 || h.Status != "pending" || h.NextPollAt.IsZero() {
		t.Fatalf("begin observation: %+v", h)
	}
	r, e = f.store.BeginDeviceEnrollment(context.Background(), c)
	if e != nil || r.Outcome != OutcomeReplayed {
		t.Fatalf("begin replay %+v %v", r, e)
	}
	if _, ok = r.HeadlessEnrollmentResult(); !ok {
		t.Fatal("replay missing typed result")
	}
	return c
}
func TestHeadlessEnrollmentCompletionAndReplayPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "headless-owner")
	c := headlessBegin(t, f, "complete")
	decision := HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "approve", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash}
	if r, e := f.store.DecideHeadlessDeviceEnrollment(ctx, decision); e != nil || r.Control.Status != "approved" {
		t.Fatalf("approve %+v %v", r, e)
	}
	if r, e := f.store.DecideHeadlessDeviceEnrollment(ctx, decision); e != nil || r.Outcome != OutcomeReplayed {
		t.Fatalf("replay %+v %v", r, e)
	}
	completion := HeadlessEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete", Origin: c.Origin, ProofHash: c.ProofHash, ConfirmationHash: c.ConfirmationHash}
	bad := completion
	bad.ProofHash = sha256.Sum256([]byte("wrong"))
	r, e := f.store.CompleteHeadlessDeviceEnrollment(ctx, bad)
	assertIdentityAuthorization(t, r, e)
	for i := 0; i < 2; i++ {
		r, e = f.store.CompleteHeadlessDeviceEnrollment(ctx, completion)
		if e != nil || r.Control == nil || r.Control.GrantID != c.Credential.GrantID {
			t.Fatalf("complete %+v %v", r, e)
		}
	}
	counts := devicePGCounts(t, f)
	if counts[1] != 1 || counts[2] != 1 || counts[3] != 1 {
		t.Fatalf("duplicate grant %v", counts)
	}
	if _, e = f.admin.Exec(ctx, `UPDATE identity_policy SET restore_epoch='new-restore' WHERE id`); e != nil {
		t.Fatal(e)
	}
	r, e = f.store.CompleteHeadlessDeviceEnrollment(ctx, completion)
	assertIdentityAuthorization(t, r, e)
}
func TestHeadlessPollSlotRateAndProofPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	c := headlessBegin(t, f, "poll")
	poll := HeadlessEnrollmentPoll{RequestID: c.RequestID, Origin: c.Origin, ProofHash: c.ProofHash, Sequence: 1}
	bad := poll
	bad.ProofHash = sha256.Sum256([]byte("wrong"))
	r, e := f.store.PollHeadlessDeviceEnrollment(ctx, bad)
	assertIdentityAuthorization(t, r, e)
	before := devicePGCounts(t, f)
	r, e = f.store.PollHeadlessDeviceEnrollment(ctx, poll)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("early poll %+v %v", r, e)
	}
	h, _ := r.HeadlessEnrollmentResult()
	if h.Status != "slow_down" || h.Disposition != "" {
		t.Fatalf("early %+v", h)
	}
	r, e = f.store.PollHeadlessDeviceEnrollment(ctx, poll)
	if e != nil || r.Outcome != OutcomeReplayed {
		t.Fatalf("duplicate %+v %v", r, e)
	}
	f.store.Close()
	f.store, e = Open(ctx, f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.store.Close() })
	time.Sleep(time.Until(h.NextPollAt) + 20*time.Millisecond)
	poll.Sequence = 2
	r, e = f.store.PollHeadlessDeviceEnrollment(ctx, poll)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("normal %+v %v", r, e)
	}
	h, _ = r.HeadlessEnrollmentResult()
	if h.Status != "pending" {
		t.Fatalf("normal %+v", h)
	}
	after := devicePGCounts(t, f)
	if after[5] != before[5] || after[6] != before[6] || after[7] != before[7]+1 {
		t.Fatalf("normal appended global rows %v %v", before, after)
	}
	old := poll
	old.Sequence = 1
	r, e = f.store.ResolveHeadlessDeviceEnrollmentPoll(ctx, old)
	if e != nil || r.Outcome != OutcomeUnknown || r.Control != nil {
		t.Fatalf("overwritten slot guessed %+v %v", r, e)
	}
	for seq := uint64(3); seq <= 6; seq++ {
		poll.Sequence = seq
		r, e = f.store.PollHeadlessDeviceEnrollment(ctx, poll)
		if e != nil || r.Outcome != OutcomeCommitted {
			t.Fatalf("rate %+v %v", r, e)
		}
	}
	h, _ = r.HeadlessEnrollmentResult()
	if h.Disposition != "polling_exhausted" {
		t.Fatalf("budget %+v", h)
	}
	var count int
	if e = f.admin.QueryRow(ctx, `SELECT poll_violations FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&count); e != nil || count != 5 {
		t.Fatalf("counter %d %v", count, e)
	}
}
func TestHeadlessGuessAuditRollbackPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "headless-owner")
	c := headlessBegin(t, f, "rollback")
	if _, e := f.admin.Exec(ctx, `CREATE FUNCTION reject_headless_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit failure'; END $$; CREATE TRIGGER reject_headless_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION reject_headless_audit()`); e != nil {
		t.Fatal(e)
	}
	r, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "guess", RequestID: c.RequestID, Approve: true, ConfirmationHash: sha256.Sum256([]byte("wrong"))})
	if e == nil || r.Outcome == OutcomeCommitted {
		t.Fatalf("audit failure accepted %+v %v", r, e)
	}
	var count int
	if e = f.admin.QueryRow(ctx, `SELECT confirmation_failures FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&count); e != nil || count != 0 {
		t.Fatalf("rolled-back count %d %v", count, e)
	}
}
func TestHeadlessUnknownCommitRecoveryPG(t *testing.T) {
	for _, stage := range []string{"begin", "poll", "complete"} {
		for _, mode := range []string{"drop-response", "break-on-commit"} {
			t.Run(stage+"/"+mode, func(t *testing.T) {
				proxy := startProxy(t, mode)
				f := identityPGFixture(t, proxy.dsn)
				ctx := context.Background()
				_, owner := identityPGLogin(t, f, "member-one", "owner")
				c := headlessPGCommand("unknown")
				if stage != "begin" {
					headlessBegin(t, f, "unknown")
				}
				if stage == "complete" {
					if _, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "approve", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash}); e != nil {
						t.Fatal(e)
					}
				}
				poll := HeadlessEnrollmentPoll{RequestID: c.RequestID, Origin: c.Origin, ProofHash: c.ProofHash, Sequence: 1}
				complete := HeadlessEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete", Origin: c.Origin, ProofHash: c.ProofHash, ConfirmationHash: c.ConfirmationHash}
				proxy.arm()
				var r Result
				var e error
				switch stage {
				case "begin":
					r, e = f.store.BeginDeviceEnrollment(ctx, c)
				case "poll":
					r, e = f.store.PollHeadlessDeviceEnrollment(ctx, poll)
				case "complete":
					r, e = f.store.CompleteHeadlessDeviceEnrollment(ctx, complete)
				}
				if k := kernelErr(t, r, e); k.Outcome != OutcomeUnknown || r.Control != nil {
					t.Fatalf("uncertain %+v %v", r, e)
				}
				f.store.Close()
				f.store, e = Open(ctx, f.cfg)
				if e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { f.store.Close() })
				switch stage {
				case "begin":
					r, e = f.store.BeginDeviceEnrollment(ctx, c)
				case "poll":
					r, e = f.store.ResolveHeadlessDeviceEnrollmentPoll(ctx, poll)
				case "complete":
					r, e = f.store.CompleteHeadlessDeviceEnrollment(ctx, complete)
				}
				if e != nil {
					t.Fatalf("recovery %+v %v", r, e)
				}
				if mode == "drop-response" && r.Outcome != OutcomeReplayed {
					t.Fatalf("committed recovery %+v", r)
				}
				if mode == "break-on-commit" && stage == "poll" && r.Outcome != OutcomeNotCommitted {
					t.Fatalf("absent poll %+v", r)
				}
				counts := devicePGCounts(t, f)
				if stage == "complete" && (counts[1] != 1 || counts[2] != 1 || counts[3] != 1) {
					t.Fatalf("duplicate credential %v", counts)
				}
			})
		}
	}
}
func TestHeadlessApprovedRestoreDirectAndRecoveryPG(t *testing.T) {
	ctx := context.Background()
	b, objects := backupPGSource(t)
	c := headlessBegin(t, b.f, "restore-headless")
	if _, e := b.f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: b.owner.Auth, ClientID: "approve-headless", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash}); e != nil {
		t.Fatal(e)
	}
	poll := HeadlessEnrollmentPoll{RequestID: c.RequestID, Origin: c.Origin, ProofHash: c.ProofHash, Sequence: 1}
	if _, e := b.f.store.PollHeadlessDeviceEnrollment(ctx, poll); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "bundle")
	if _, e := b.f.store.BackupOffline(ctx, objects, path, "backup-headless"); e != nil {
		t.Fatal(e)
	}
	bundle, e := enterprisebackup.Open(ctx, path, b.f.cfg.DeploymentID, "org-test")
	if e != nil {
		t.Fatal(e)
	}
	b.f.store.Close()
	target := backupPGPristine(t)
	target.cfg.AttachmentLimits = b.f.cfg.AttachmentLimits
	target.store.cfg.AttachmentLimits = target.cfg.AttachmentLimits
	restored, e := enterpriseobject.Open(filepath.Join(t.TempDir(), "restored"), target.cfg.DeploymentID)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = target.store.RestoreOffline(ctx, restored, bundle, "restore-headless"); e != nil {
		t.Fatal(e)
	}
	if _, e = target.store.RecoverDeliveries(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = target.store.CompleteRestore(ctx, restored, "restore-headless", bundle.Digest()); e != nil {
		t.Fatal(e)
	}
	complete := HeadlessEnrollmentCompletion{RequestID: c.RequestID, ClientID: "direct-after-restore", Origin: c.Origin, ProofHash: c.ProofHash, ConfirmationHash: c.ConfirmationHash}
	for i := 0; i < 2; i++ {
		r, e := target.store.CompleteHeadlessDeviceEnrollment(ctx, complete)
		assertIdentityAuthorization(t, r, e)
	}
	r, e := target.store.ResolveHeadlessDeviceEnrollmentPoll(ctx, poll)
	if e != nil {
		t.Fatal(e)
	}
	h, _ := r.HeadlessEnrollmentResult()
	if h.Status != "invalidated" {
		t.Fatalf("restored poll revived %+v", h)
	}
	var grants int
	if e = target.admin.QueryRow(ctx, `SELECT count(*) FROM device_grants WHERE grant_id=$1`, c.Credential.GrantID).Scan(&grants); e != nil || grants != 0 {
		t.Fatalf("old candidate minted %d %v", grants, e)
	}
	_, owner := identityPGLogin(t, target, "member-one", "fresh-restored-owner")
	fresh := headlessBegin(t, target, "fresh-after-restore")
	if _, e = target.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "fresh-approve", RequestID: fresh.RequestID, Approve: true, ConfirmationHash: fresh.ConfirmationHash}); e != nil {
		t.Fatal(e)
	}
	complete = HeadlessEnrollmentCompletion{RequestID: fresh.RequestID, ClientID: "fresh-complete", Origin: fresh.Origin, ProofHash: fresh.ProofHash, ConfirmationHash: fresh.ConfirmationHash}
	if r, e = target.store.CompleteHeadlessDeviceEnrollment(ctx, complete); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("fresh %+v %v", r, e)
	}
}
func TestHeadlessGuessSharedSessionsAndApprovalPositivePG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner-one")
	_, other := identityPGLogin(t, f, "member-one", "owner-two")
	c := headlessBegin(t, f, "four-then-correct")
	for i := 0; i < 4; i++ {
		auth := owner.Auth
		if i%2 != 0 {
			auth = other.Auth
		}
		decision := HeadlessEnrollmentDecision{Auth: auth, ClientID: fmt.Sprintf("guess-%d", i), RequestID: c.RequestID, Approve: true, ConfirmationHash: sha256.Sum256([]byte("wrong"))}
		r, e := f.store.DecideHeadlessDeviceEnrollment(ctx, decision)
		if e != nil || r.Outcome != OutcomeCommitted {
			t.Fatalf("guess %+v %v", r, e)
		}
		r, e = f.store.DecideHeadlessDeviceEnrollment(ctx, decision)
		if e != nil || r.Outcome != OutcomeReplayed {
			t.Fatalf("guess replay %+v %v", r, e)
		}
	}
	if _, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: other.Auth, ClientID: "correct", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash}); e != nil {
		t.Fatal(e)
	}
	var failures, approvals int
	if e := f.admin.QueryRow(ctx, `SELECT confirmation_failures,approval_count FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&failures, &approvals); e != nil || failures != 4 || approvals != 1 {
		t.Fatalf("counts %d %d %v", failures, approvals, e)
	}
}
func TestHeadlessFreshAlphaShapeRejectsDriftPG(t *testing.T) {
	for _, mutation := range []string{`ALTER TABLE device_enrollments DROP CONSTRAINT device_enrollments_transport_shape`, `DROP TRIGGER device_enrollments_state_guard ON device_enrollments`, `ALTER TABLE device_enrollments ALTER COLUMN confirmation_failures DROP NOT NULL`, `ALTER TABLE device_enrollments DROP CONSTRAINT device_enrollments_poll_violations_check;ALTER TABLE device_enrollments ADD CONSTRAINT device_enrollments_poll_violations_check CHECK(poll_violations BETWEEN 0 AND 6)`} {
		t.Run(mutation, func(t *testing.T) {
			f := identityPGFixture(t, nil)
			f.store.Close()
			if _, e := f.admin.Exec(context.Background(), mutation); e != nil {
				t.Fatal(e)
			}
			s, e := Open(context.Background(), f.cfg)
			if e == nil {
				s.Close()
				t.Fatal("weak schema opened")
			}
		})
	}
}
func TestHeadlessPollBudget120AndTTLPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	c := headlessBegin(t, f, "budget120")
	if _, e := f.admin.Exec(ctx, strings.ReplaceAll(`ALTER TABLE device_enrollments DISABLE TRIGGER device_enrollments_state_guard; UPDATE device_enrollments SET poll_sequence=119,last_poll_intent_hash=repeat('a',32)::bytea,last_poll_status='pending',last_poll_next_at=next_poll_at WHERE request_id=$1; ALTER TABLE device_enrollments ENABLE TRIGGER device_enrollments_state_guard`, "$1", "'"+c.RequestID+"'")); e != nil {
		t.Fatal(e)
	}
	poll := HeadlessEnrollmentPoll{RequestID: c.RequestID, Origin: c.Origin, ProofHash: c.ProofHash, Sequence: 120}
	r, e := f.store.PollHeadlessDeviceEnrollment(ctx, poll)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("120 %+v %v", r, e)
	}
	h, _ := r.HeadlessEnrollmentResult()
	if h.Status != "invalidated" || h.Disposition != "polling_exhausted" || h.Sequence != 120 {
		t.Fatalf("120 result %+v", h)
	}
	r, e = f.store.ResolveHeadlessDeviceEnrollmentPoll(ctx, poll)
	h, _ = r.HeadlessEnrollmentResult()
	if e != nil || r.Outcome != OutcomeReplayed || h.Disposition != "polling_exhausted" {
		t.Fatalf("120 replay %+v %v", h, e)
	}
	expired := headlessBegin(t, f, "expired")
	if _, e = f.admin.Exec(ctx, strings.ReplaceAll(`ALTER TABLE device_enrollments DISABLE TRIGGER device_enrollments_identity_immutable;ALTER TABLE device_enrollments DISABLE TRIGGER device_enrollments_state_guard;UPDATE device_enrollments SET created_at=pg_catalog.statement_timestamp()-interval '11 minutes',expires_at=pg_catalog.statement_timestamp()-interval '1 minute',next_poll_at=pg_catalog.statement_timestamp()-interval '1 minute' WHERE request_id=$1;ALTER TABLE device_enrollments ENABLE TRIGGER device_enrollments_identity_immutable;ALTER TABLE device_enrollments ENABLE TRIGGER device_enrollments_state_guard`, "$1", "'"+expired.RequestID+"'")); e != nil {
		t.Fatal(e)
	}
	poll = HeadlessEnrollmentPoll{RequestID: expired.RequestID, Origin: expired.Origin, ProofHash: expired.ProofHash, Sequence: 1}
	r, e = f.store.PollHeadlessDeviceEnrollment(ctx, poll)
	h, _ = r.HeadlessEnrollmentResult()
	if e != nil || h.Status != "expired" {
		t.Fatalf("expiry %+v %v", h, e)
	}
}
func TestHeadlessRevocationNeverRevivesPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	_, admin := identityPGLogin(t, f, "operator-selected", "admin")
	c := headlessBegin(t, f, "disabled-owner")
	if _, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "approve", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetUserEnabled(ctx, admin.Auth, "disable", owner.Auth.UserID, false); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetUserEnabled(ctx, admin.Auth, "enable", owner.Auth.UserID, true); e != nil {
		t.Fatal(e)
	}
	complete := HeadlessEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete", Origin: c.Origin, ProofHash: c.ProofHash, ConfirmationHash: c.ConfirmationHash}
	r, e := f.store.CompleteHeadlessDeviceEnrollment(ctx, complete)
	assertIdentityAuthorization(t, r, e)
	var state string
	if e = f.admin.QueryRow(ctx, `SELECT status FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&state); e != nil || state != "invalidated" {
		t.Fatalf("revived %s %v", state, e)
	}
}
func TestHeadlessConcurrentSamePollDoesNotDoubleCountPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	c := headlessBegin(t, f, "concurrent")
	poll := HeadlessEnrollmentPoll{RequestID: c.RequestID, Origin: c.Origin, ProofHash: c.ProofHash, Sequence: 1}
	barrier := make(chan struct{})
	results := make(chan Result, 2)
	errs := make(chan error, 2)
	for _, store := range []*Store{f.store, f.store} {
		go func(store *Store) {
			<-barrier
			r, e := store.PollHeadlessDeviceEnrollment(ctx, poll)
			results <- r
			errs <- e
		}(store)
	}
	close(barrier)
	for i := 0; i < 2; i++ {
		r, e := <-results, <-errs
		if e != nil || r.Outcome != OutcomeCommitted && r.Outcome != OutcomeReplayed {
			t.Fatalf("concurrent %+v %v", r, e)
		}
	}
	var count int
	if e := f.admin.QueryRow(ctx, `SELECT poll_violations FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&count); e != nil || count != 1 {
		t.Fatalf("double count %d %v", count, e)
	}
}
func TestHeadlessEffectAuditRollbackPG(t *testing.T) {
	for _, stage := range []string{"approval", "completion", "rate-invalidation"} {
		t.Run(stage, func(t *testing.T) {
			f := identityPGFixture(t, nil)
			ctx := context.Background()
			_, owner := identityPGLogin(t, f, "member-one", "owner")
			c := headlessBegin(t, f, "audit-"+stage)
			if stage == "completion" {
				if _, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "approve", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash}); e != nil {
					t.Fatal(e)
				}
			}
			if stage == "rate-invalidation" {
				for seq := uint64(1); seq <= 4; seq++ {
					if _, e := f.store.PollHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentPoll{RequestID: c.RequestID, Origin: c.Origin, ProofHash: c.ProofHash, Sequence: seq}); e != nil {
						t.Fatal(e)
					}
				}
			}
			before := devicePGCounts(t, f)
			if _, e := f.admin.Exec(ctx, `CREATE FUNCTION reject_headless_effect_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic audit failure'; END $$; CREATE TRIGGER reject_headless_effect_audit BEFORE INSERT ON audit FOR EACH ROW EXECUTE FUNCTION reject_headless_effect_audit()`); e != nil {
				t.Fatal(e)
			}
			var r Result
			var e error
			switch stage {
			case "approval":
				r, e = f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "approve", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash})
			case "completion":
				r, e = f.store.CompleteHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete", Origin: c.Origin, ProofHash: c.ProofHash, ConfirmationHash: c.ConfirmationHash})
			case "rate-invalidation":
				r, e = f.store.PollHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentPoll{RequestID: c.RequestID, Origin: c.Origin, ProofHash: c.ProofHash, Sequence: 5})
			}
			if e == nil || r.Outcome == OutcomeCommitted {
				t.Fatalf("audit failure accepted %+v %v", r, e)
			}
			if after := devicePGCounts(t, f); after != before {
				t.Fatalf("rollback effects %v %v", before, after)
			}
			if stage == "rate-invalidation" {
				var seq uint64
				var violations int
				var state string
				if e = f.admin.QueryRow(ctx, `SELECT poll_sequence,poll_violations,status FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&seq, &violations, &state); e != nil || seq != 4 || violations != 4 || state != "pending" {
					t.Fatalf("rollback row %d %d %s %v", seq, violations, state, e)
				}
			}
		})
	}
}
func TestHeadlessApprovedTTLAndPermanentDeviceRevokePG(t *testing.T) {
	for _, mode := range []string{"expired-approved", "permanent-device-revoke"} {
		t.Run(mode, func(t *testing.T) {
			f := identityPGFixture(t, nil)
			ctx := context.Background()
			_, owner := identityPGLogin(t, f, "member-one", "owner")
			c := headlessBegin(t, f, mode)
			if _, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "approve", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash}); e != nil {
				t.Fatal(e)
			}
			completion := HeadlessEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete", Origin: c.Origin, ProofHash: c.ProofHash, ConfirmationHash: c.ConfirmationHash}
			if mode == "expired-approved" {
				if _, e := f.admin.Exec(ctx, strings.ReplaceAll(`ALTER TABLE device_enrollments DISABLE TRIGGER device_enrollments_identity_immutable;ALTER TABLE device_enrollments DISABLE TRIGGER device_enrollments_state_guard;UPDATE device_enrollments SET created_at=pg_catalog.statement_timestamp()-interval '11 minutes',expires_at=pg_catalog.statement_timestamp()-interval '1 minute',next_poll_at=pg_catalog.statement_timestamp()-interval '1 minute' WHERE request_id=$1;ALTER TABLE device_enrollments ENABLE TRIGGER device_enrollments_identity_immutable;ALTER TABLE device_enrollments ENABLE TRIGGER device_enrollments_state_guard`, "$1", "'"+c.RequestID+"'")); e != nil {
					t.Fatal(e)
				}
			} else {
				if _, e := f.store.CompleteHeadlessDeviceEnrollment(ctx, completion); e != nil {
					t.Fatal(e)
				}
				if _, e := f.store.RevokeDevice(ctx, owner.Auth, "revoke", c.DeviceID); e != nil {
					t.Fatal(e)
				}
			}
			before := devicePGCounts(t, f)
			r, e := f.store.CompleteHeadlessDeviceEnrollment(ctx, completion)
			assertIdentityAuthorization(t, r, e)
			if after := devicePGCounts(t, f); after != before {
				t.Fatalf("refused completed effects %v %v", before, after)
			}
		})
	}
}
func TestHeadlessTwoAccountsConsentHasOneOwnerPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, one := identityPGLogin(t, f, "member-one", "owner-one")
	_, two := identityPGLogin(t, f, "member-two", "owner-two")
	c := headlessBegin(t, f, "two-owners")
	barrier := make(chan struct{})
	type answer struct {
		r Result
		e error
	}
	results := make(chan answer, 2)
	for i, owner := range []BrowserSession{one, two} {
		go func(i int, owner BrowserSession) {
			<-barrier
			r, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: fmt.Sprintf("approve-%d", i), RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash})
			results <- answer{r, e}
		}(i, owner)
	}
	close(barrier)
	committed, refused := 0, 0
	for i := 0; i < 2; i++ {
		a := <-results
		if a.e == nil && a.r.Outcome == OutcomeCommitted {
			committed++
		} else if a.r.Class == ClassAuthorization {
			refused++
		} else {
			t.Fatalf("unexpected race %+v %v", a.r, a.e)
		}
	}
	if committed != 1 || refused != 1 {
		t.Fatalf("consent outcomes %d %d", committed, refused)
	}
	var count int
	if e := f.admin.QueryRow(ctx, `SELECT approval_count FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&count); e != nil || count != 1 {
		t.Fatalf("approval count %d %v", count, e)
	}
}
func TestHeadlessApprovedOwnerCanDenyWithoutChangingConsentPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	_, other := identityPGLogin(t, f, "member-two", "other")
	c := headlessBegin(t, f, "approved-deny")
	if _, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "approve", RequestID: c.RequestID, Approve: true, ConfirmationHash: c.ConfirmationHash}); e != nil {
		t.Fatal(e)
	}
	r, e := f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: other.Auth, ClientID: "other-deny", RequestID: c.RequestID})
	assertIdentityAuthorization(t, r, e)
	r, e = f.store.DecideHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentDecision{Auth: owner.Auth, ClientID: "owner-deny", RequestID: c.RequestID})
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("deny %+v %v", r, e)
	}
	h, _ := r.HeadlessEnrollmentResult()
	if h.Status != "denied" {
		t.Fatalf("deny %+v", h)
	}
	var count int
	var hash []byte
	var user string
	if e = f.admin.QueryRow(ctx, `SELECT approval_count,code_hash,approved_user_id FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&count, &hash, &user); e != nil || count != 1 || !bytes.Equal(hash, c.ConfirmationHash[:]) || user != owner.Auth.UserID {
		t.Fatalf("consent changed %d %s %v", count, user, e)
	}
	r, e = f.store.CompleteHeadlessDeviceEnrollment(ctx, HeadlessEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete", Origin: c.Origin, ProofHash: c.ProofHash, ConfirmationHash: c.ConfirmationHash})
	assertIdentityAuthorization(t, r, e)
}
func TestHeadlessExpiredQuotaReplayUsesTTLDispositionPG(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	c := headlessBegin(t, f, "expired-quota")
	poll := HeadlessEnrollmentPoll{RequestID: c.RequestID, Origin: c.Origin, ProofHash: c.ProofHash}
	for seq := uint64(1); seq <= 5; seq++ {
		poll.Sequence = seq
		if _, e := f.store.PollHeadlessDeviceEnrollment(ctx, poll); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := f.admin.Exec(ctx, strings.ReplaceAll(`ALTER TABLE device_enrollments DISABLE TRIGGER device_enrollments_identity_immutable;ALTER TABLE device_enrollments DISABLE TRIGGER device_enrollments_state_guard;UPDATE device_enrollments SET created_at=pg_catalog.statement_timestamp()-interval '11 minutes',expires_at=pg_catalog.statement_timestamp()-interval '1 minute',next_poll_at=pg_catalog.statement_timestamp()-interval '1 minute',last_poll_next_at=pg_catalog.statement_timestamp()-interval '1 minute' WHERE request_id=$1;ALTER TABLE device_enrollments ENABLE TRIGGER device_enrollments_identity_immutable;ALTER TABLE device_enrollments ENABLE TRIGGER device_enrollments_state_guard`, "$1", "'"+c.RequestID+"'")); e != nil {
		t.Fatal(e)
	}
	r, e := f.store.ResolveHeadlessDeviceEnrollmentPoll(ctx, poll)
	h, _ := r.HeadlessEnrollmentResult()
	if e != nil || r.Outcome != OutcomeReplayed || h.Status != "expired" || h.Disposition != "" {
		t.Fatalf("quota won TTL %+v %v", h, e)
	}
}
