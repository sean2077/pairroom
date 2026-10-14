package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func devicePGCommand(id string) DeviceEnrollmentCommand {
	secret := sha256.Sum256([]byte("client-state-" + id))
	return DeviceEnrollmentCommand{RequestID: "request-" + id, DeviceID: "device-" + id, Label: "Development workstation", Origin: "https://pairroom.example.test", CallbackURI: "http://127.0.0.1:48123/callback", ClientState: base64.RawURLEncoding.EncodeToString(secret[:]), ProofHash: sha256.Sum256([]byte("private-proof-" + id)), Credential: DeviceCredentialCandidate{GrantID: "grant-" + id, AccessHash: sha256.Sum256([]byte("private-access-" + id)), RefreshHash: sha256.Sum256([]byte("private-refresh-" + id))}}
}
func devicePGEnroll(t *testing.T, f *fixture, owner BrowserSession, id string) (DeviceEnrollmentCommand, DeviceCredential) {
	t.Helper()
	ctx := context.Background()
	cmd := devicePGCommand(id)
	if r, e := f.store.BeginDeviceEnrollment(ctx, cmd); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("begin device: %+v %v", r, e)
	}
	code := sha256.Sum256([]byte("completion-code-" + id))
	if r, e := f.store.DecideDeviceEnrollment(ctx, DeviceEnrollmentDecision{Auth: owner.Auth, ClientID: "approve-" + id, RequestID: cmd.RequestID, Approve: true, CodeHash: code}); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("approve device: %+v %v", r, e)
	}
	complete := DeviceEnrollmentCompletion{RequestID: cmd.RequestID, ClientID: "complete-" + id, Origin: cmd.Origin, ProofHash: cmd.ProofHash, CodeHash: code}
	r, e := f.store.CompleteDeviceEnrollment(ctx, complete)
	if e != nil || r.Outcome != OutcomeCommitted || r.Control == nil || r.Control.GrantID != cmd.Credential.GrantID {
		t.Fatalf("complete device: %+v %v", r, e)
	}
	again, e := f.store.CompleteDeviceEnrollment(ctx, complete)
	if e != nil || again.Outcome != OutcomeReplayed {
		t.Fatalf("completion replay: %+v %v", again, e)
	}
	got, e := f.store.AuthenticateDevice(ctx, cmd.Credential.AccessHash, cmd.Origin)
	if e != nil || got.Auth.UserID != owner.Auth.UserID || got.Auth.DeviceGrantID != cmd.Credential.GrantID {
		t.Fatalf("authenticate device: %+v %v", got, e)
	}
	return cmd, got
}
func devicePGCounts(t *testing.T, f *fixture) [8]int64 {
	t.Helper()
	var r [8]int64
	e := f.admin.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM device_enrollments),(SELECT count(*) FROM devices),(SELECT count(*) FROM device_families),(SELECT count(*) FROM device_grants),(SELECT count(*) FROM agents),(SELECT count(*) FROM control_events),(SELECT count(*) FROM control_operations),(SELECT count(*) FROM audit)`).Scan(&r[0], &r[1], &r[2], &r[3], &r[4], &r[5], &r[6], &r[7])
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func devicePGRefused(t *testing.T, f *fixture, token [32]byte, origin string) {
	t.Helper()
	got, e := f.store.AuthenticateDevice(context.Background(), token, origin)
	var k *KernelError
	if !errors.As(e, &k) || k.Class != ClassAuthorization || got != (DeviceCredential{}) {
		t.Fatalf("device leaked or accepted: %+v %v", got, e)
	}
}

func TestDeviceEnrollmentRequiresOwnerCodeAndOriginalClientProof(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	_, other := identityPGLogin(t, f, "member-two", "other")
	c := devicePGCommand("proof")
	r, e := f.store.BeginDeviceEnrollment(ctx, c)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("begin: %+v %v", r, e)
	}
	before := devicePGCounts(t, f)
	devicePGRefused(t, f, c.Credential.AccessHash, c.Origin)
	code := sha256.Sum256([]byte("private-code"))
	completion := DeviceEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete", Origin: c.Origin, ProofHash: c.ProofHash, CodeHash: code}
	r, e = f.store.CompleteDeviceEnrollment(ctx, completion)
	assertIdentityAuthorization(t, r, e)
	if after := devicePGCounts(t, f); after != before {
		t.Fatal("unapproved completion changed authority")
	}
	r, e = f.store.DecideDeviceEnrollment(ctx, DeviceEnrollmentDecision{Auth: owner.Auth, ClientID: "approve", RequestID: c.RequestID, Approve: true, CodeHash: code})
	if e != nil || r.Control.Status != "approved" {
		t.Fatalf("approve: %+v %v", r, e)
	}
	r, e = f.store.DecideDeviceEnrollment(ctx, DeviceEnrollmentDecision{Auth: other.Auth, ClientID: "steal", RequestID: c.RequestID, Approve: true, CodeHash: code})
	assertIdentityAuthorization(t, r, e)
	bad := completion
	bad.ProofHash = sha256.Sum256([]byte("wrong"))
	r, e = f.store.CompleteDeviceEnrollment(ctx, bad)
	assertIdentityAuthorization(t, r, e)
	bad = completion
	bad.CodeHash = sha256.Sum256([]byte("wrong"))
	r, e = f.store.CompleteDeviceEnrollment(ctx, bad)
	assertIdentityAuthorization(t, r, e)
	bad = completion
	bad.Origin = "https://other.example.test"
	r, e = f.store.CompleteDeviceEnrollment(ctx, bad)
	assertIdentityAuthorization(t, r, e)
	r, e = f.store.CompleteDeviceEnrollment(ctx, completion)
	if e != nil || r.Control.UserID != owner.Auth.UserID {
		t.Fatalf("completion: %+v %v", r, e)
	}
	encoded, _ := json.Marshal(r.Control)
	for _, secret := range []string{"private-code", "private-proof", "private-access", "private-refresh", c.ClientState, c.CallbackURI} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("credential/control callback leaked into receipt")
		}
	}
	if _, e = f.store.AuthenticateDevice(ctx, c.Credential.AccessHash, c.Origin); e != nil {
		t.Fatal(e)
	}
	devicePGRefused(t, f, c.Credential.AccessHash, "https://other.example.test")
}
func TestDeviceApprovalLossCanReplaceCodeOnlyWithinOriginalOwner(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	c := devicePGCommand("approval")
	if _, e := f.store.BeginDeviceEnrollment(ctx, c); e != nil {
		t.Fatal(e)
	}
	old := DeviceEnrollmentDecision{Auth: owner.Auth, ClientID: "first", RequestID: c.RequestID, Approve: true, CodeHash: sha256.Sum256([]byte("lost-first-code"))}
	if _, e := f.store.DecideDeviceEnrollment(ctx, old); e != nil {
		t.Fatal(e)
	}
	replacement := old
	replacement.ClientID = "second"
	replacement.CodeHash = sha256.Sum256([]byte("new-code"))
	if _, e := f.store.DecideDeviceEnrollment(ctx, replacement); e != nil {
		t.Fatal(e)
	}
	r, e := f.store.DecideDeviceEnrollment(ctx, old)
	if e == nil || r.Outcome != OutcomeRefused || r.Control != nil {
		t.Fatal("old approval redirect was revived")
	}
	r, e = f.store.CompleteDeviceEnrollment(ctx, DeviceEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete-old", Origin: c.Origin, ProofHash: c.ProofHash, CodeHash: old.CodeHash})
	assertIdentityAuthorization(t, r, e)
	r, e = f.store.CompleteDeviceEnrollment(ctx, DeviceEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete-new", Origin: c.Origin, ProofHash: c.ProofHash, CodeHash: replacement.CodeHash})
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("new completion failed: %+v %v", r, e)
	}
}
func TestDeviceAuthorityCannotBecomeBrowserOrAdministrator(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "operator-selected", "admin")
	c, device := devicePGEnroll(t, f, owner, "admin-device")
	r, e := f.store.SetAdministrator(ctx, device.Auth, "bad-admin", owner.Auth.UserID, true)
	assertIdentityAuthorization(t, r, e)
	r, e = f.store.SetIdentityAdmission(ctx, device.Auth, "bad-admission", identityPGSubject("attacker"), true)
	assertIdentityAuthorization(t, r, e)
	intent, _ := model.PrepareEnterpriseRoomIntent("Device cannot create", model.Collaboration{})
	r, e = f.store.CommitRoomCreate(ctx, RoomCreateCommand{Auth: device.Auth, RoomID: "bad-device-room", ClientID: "bad-create", Intent: intent})
	assertIdentityAuthorization(t, r, e)
	source := sha256.Sum256([]byte("opaque-source"))
	r, e = f.store.RegisterAgent(ctx, device.Auth, "register", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeClaude)
	if e != nil || r.Control.AgentID == "" {
		t.Fatalf("register: %+v %v", r, e)
	}
	conflict, e := f.store.RegisterAgent(ctx, device.Auth, "register-other-runtime", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeCodex)
	if e == nil || conflict.Control != nil {
		t.Fatal("canonical Agent runtime relabeled")
	}
	r, e = f.store.RevokeDevice(ctx, owner.Auth, "revoke", device.DeviceID)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("revoke: %+v %v", r, e)
	}
	devicePGRefused(t, f, c.Credential.AccessHash, c.Origin)
	r, e = f.store.RegisterAgent(ctx, device.Auth, "after-revoke", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeClaude)
	assertIdentityAuthorization(t, r, e)
}
func devicePGRotation(c DeviceEnrollmentCommand, id string) DeviceRotationCommand {
	return DeviceRotationCommand{DeviceID: c.DeviceID, CurrentGrantID: c.Credential.GrantID, RotationID: "rotate-" + id, Origin: c.Origin, ClientProofHash: c.ProofHash, CurrentRefreshHash: c.Credential.RefreshHash, Next: DeviceCredentialCandidate{GrantID: "next-" + id, AccessHash: sha256.Sum256([]byte("next-access-" + id)), RefreshHash: sha256.Sum256([]byte("next-refresh-" + id))}}
}
func TestDeviceRefreshAllowsOnlyOneExactPredecessorRecovery(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	c, _ := devicePGEnroll(t, f, owner, "rotate")
	cmd := devicePGRotation(c, "first")
	r, e := f.store.RotateDeviceCredential(ctx, cmd)
	if e != nil || r.Control.GrantID != cmd.Next.GrantID {
		t.Fatalf("rotate: %+v %v", r, e)
	}
	devicePGRefused(t, f, c.Credential.AccessHash, c.Origin)
	r, e = f.store.RotateDeviceCredential(ctx, cmd)
	if e == nil || r.Class != "rotation_recovery_required" || r.Control != nil {
		t.Fatal("old refresh replayed without recovery")
	}
	bad := cmd
	bad.Next.AccessHash = sha256.Sum256([]byte("changed"))
	r, e = f.store.RecoverDeviceRotation(ctx, bad)
	if e == nil || r.Control != nil {
		t.Fatal("changed recovery candidate accepted")
	}
	r, e = f.store.RecoverDeviceRotation(ctx, cmd)
	if e != nil || r.Outcome != OutcomeCommitted || r.Control.Kind != ControlDeviceRecover || r.Control.GrantID != cmd.Next.GrantID {
		t.Fatalf("recover: %+v %v", r, e)
	}
	before := devicePGCounts(t, f)
	r, e = f.store.RecoverDeviceRotation(ctx, cmd)
	if e == nil || r.Class != "rotation_recovery_unavailable" || r.Control != nil {
		t.Fatal("recovery reused")
	}
	if devicePGCounts(t, f) != before {
		t.Fatal("refused recovery changed rows")
	}
	next := cmd
	next.CurrentGrantID, next.CurrentRefreshHash = cmd.Next.GrantID, cmd.Next.RefreshHash
	next.RotationID = "rotate-second"
	next.Next = DeviceCredentialCandidate{GrantID: "third", AccessHash: sha256.Sum256([]byte("third-access")), RefreshHash: sha256.Sum256([]byte("third-refresh"))}
	if _, e = f.store.RotateDeviceCredential(ctx, next); e != nil {
		t.Fatal(e)
	}
	r, e = f.store.RecoverDeviceRotation(ctx, cmd)
	if e == nil || r.Control != nil {
		t.Fatal("nonimmediate predecessor recovered")
	}
}
func TestDeviceUserDisableReenableNeverRestoresCredentials(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, admin := identityPGLogin(t, f, "operator-selected", "admin")
	_, member := identityPGLogin(t, f, "member-one", "member")
	c, _ := devicePGEnroll(t, f, member, "disable")
	if r, e := f.store.SetUserEnabled(ctx, admin.Auth, "disable", member.Auth.UserID, false); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("disable: %+v %v", r, e)
	}
	devicePGRefused(t, f, c.Credential.AccessHash, c.Origin)
	if _, e := f.store.SetUserEnabled(ctx, admin.Auth, "enable", member.Auth.UserID, true); e != nil {
		t.Fatal(e)
	}
	devicePGRefused(t, f, c.Credential.AccessHash, c.Origin)
	r, e := f.store.RotateDeviceCredential(ctx, devicePGRotation(c, "restore"))
	assertIdentityAuthorization(t, r, e)
	_, fresh := identityPGLogin(t, f, "member-one", "fresh")
	freshCmd := devicePGCommand("fresh")
	freshCmd.DeviceID = c.DeviceID
	if _, e := f.store.BeginDeviceEnrollment(ctx, freshCmd); e != nil {
		t.Fatal(e)
	}
	code := sha256.Sum256([]byte("fresh-code"))
	if _, e := f.store.DecideDeviceEnrollment(ctx, DeviceEnrollmentDecision{Auth: fresh.Auth, ClientID: "fresh-approve", RequestID: freshCmd.RequestID, Approve: true, CodeHash: code}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.CompleteDeviceEnrollment(ctx, DeviceEnrollmentCompletion{RequestID: freshCmd.RequestID, ClientID: "fresh-complete", Origin: freshCmd.Origin, ProofHash: freshCmd.ProofHash, CodeHash: code}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.AuthenticateDevice(ctx, freshCmd.Credential.AccessHash, freshCmd.Origin); e != nil {
		t.Fatal(e)
	}
	devicePGRefused(t, f, c.Credential.AccessHash, c.Origin)
}
func TestDeviceRotationAuditFailureRollsBackEveryCredentialEffect(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	c, _ := devicePGEnroll(t, f, owner, "rollback")
	before := devicePGCounts(t, f)
	mustExec(t, f.admin, `ALTER TABLE audit ADD CONSTRAINT test_no_rotation CHECK(kind<>'device.rotate')`)
	r, e := f.store.RotateDeviceCredential(ctx, devicePGRotation(c, "failed"))
	if k := kernelErr(t, r, e); k.Outcome != OutcomeNotCommitted || k.Class != ClassConstraintViolation {
		t.Fatalf("audit failure: %+v %v", r, e)
	}
	if devicePGCounts(t, f) != before {
		t.Fatal("audit failure retained partial credential state")
	}
	if _, e = f.store.AuthenticateDevice(ctx, c.Credential.AccessHash, c.Origin); e != nil {
		t.Fatal(e)
	}
}
func TestDeviceEnrollmentAndRotationPersistAcrossOwnershipRestart(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	c, _ := devicePGEnroll(t, f, owner, "restart")
	cmd := devicePGRotation(c, "restart")
	if _, e := f.store.RotateDeviceCredential(ctx, cmd); e != nil {
		t.Fatal(e)
	}
	if e := f.store.Close(); e != nil {
		t.Fatal(e)
	}
	var e error
	f.store, e = Open(ctx, f.cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.store.AuthenticateDevice(ctx, cmd.Next.AccessHash, cmd.Origin); e != nil {
		t.Fatal(e)
	}
	if r, e := f.store.RecoverDeviceRotation(ctx, cmd); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("restart recovery: %+v %v", r, e)
	}
	mustExec(t, f.admin, `UPDATE identity_policy SET restore_epoch='new-restore' WHERE id`)
	devicePGRefused(t, f, cmd.Next.AccessHash, cmd.Origin)
}
func TestDeviceRecoveryDeadlineIsBounded(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	c, _ := devicePGEnroll(t, f, owner, "deadline")
	cmd := devicePGRotation(c, "deadline")
	if _, e := f.store.RotateDeviceCredential(ctx, cmd); e != nil {
		t.Fatal(e)
	}
	// This is an authority-state fixture, not a production clock hook. The
	// recovery eligibility time is deliberately expired before the public call.
	if _, e := f.admin.Exec(ctx, `UPDATE device_families SET recovery_until=$1`, time.Now().Add(-time.Second)); e != nil {
		t.Fatal(e)
	}
	before := devicePGCounts(t, f)
	r, e := f.store.RecoverDeviceRotation(ctx, cmd)
	if e == nil || r.Class != "rotation_recovery_unavailable" || r.Control != nil {
		t.Fatal("expired recovery succeeded")
	}
	if devicePGCounts(t, f) != before {
		t.Fatal("expired recovery wrote receipts")
	}
}

func TestDeviceUnknownRotationHasTwoRealCommitOutcomes(t *testing.T) {
	for _, mode := range []string{"drop-response", "break-on-commit"} {
		t.Run(mode, func(t *testing.T) {
			proxy := startProxy(t, mode)
			f := identityPGFixture(t, proxy.dsn)
			ctx := context.Background()
			_, owner := identityPGLogin(t, f, "member-one", "owner")
			c, _ := devicePGEnroll(t, f, owner, "unknown")
			cmd := devicePGRotation(c, "unknown")
			before := devicePGCounts(t, f)
			proxy.arm()
			r, e := f.store.RotateDeviceCredential(ctx, cmd)
			if k := kernelErr(t, r, e); k.Outcome != OutcomeUnknown || r.Control != nil {
				t.Fatalf("lost commit leaked credentials: %+v %v", r, e)
			}
			// A caller explicitly restarts after the uncertain connection. The Store
			// does not replay effects or create a replacement mutation connection.
			if e = f.store.Close(); e != nil {
				t.Fatal(e)
			}
			f.store, e = Open(ctx, f.cfg)
			if e != nil {
				t.Fatal(e)
			}
			r, e = f.store.RecoverDeviceRotation(ctx, cmd)
			if e != nil {
				t.Fatalf("recover: %+v %v", r, e)
			}
			if mode == "drop-response" {
				if r.Outcome != OutcomeCommitted || r.Control == nil || r.Control.GrantID != cmd.Next.GrantID {
					t.Fatalf("accepted rotation recovery: %+v", r)
				}
				after := devicePGCounts(t, f)
				if after[3] != before[3]+1 || after[5] != before[5]+2 || after[6] != before[6]+2 || after[7] != before[7]+2 {
					t.Fatalf("rotation/recovery effects wrong: %v -> %v", before, after)
				}
				if _, e = f.store.AuthenticateDevice(ctx, cmd.Next.AccessHash, cmd.Origin); e != nil {
					t.Fatal(e)
				}
				devicePGRefused(t, f, c.Credential.AccessHash, c.Origin)
			} else {
				if r.Outcome != OutcomeNotCommitted || r.Control != nil || devicePGCounts(t, f) != before {
					t.Fatalf("absent rotation recovered as success: %+v", r)
				}
				if _, e = f.store.AuthenticateDevice(ctx, c.Credential.AccessHash, c.Origin); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

// This fixture seeds the existing Binding SQL authority and matching strict
// fact, independently of the separately tested browser consent producer.
func devicePGSeedBinding(t *testing.T, f *fixture, owner BrowserSession, c DeviceEnrollmentCommand, device DeviceCredential, roomID string, park bool) (string, [32]byte) {
	t.Helper()
	ctx := context.Background()
	intent, e := model.PrepareEnterpriseRoomIntent("Revocation fixture", model.Collaboration{})
	if e != nil {
		t.Fatal(e)
	}
	if r, e := f.store.CommitRoomCreate(ctx, RoomCreateCommand{Auth: owner.Auth, RoomID: roomID, ClientID: "create-" + roomID, Intent: intent}); e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("create: %+v %v", r, e)
	}
	seq := uint64(2)
	if owner.Auth.UserID != device.Auth.UserID {
		if r, e := f.store.CommitRoomMembership(ctx, RoomMembershipCommand{Auth: owner.Auth, RoomID: roomID, ClientID: "admit-" + roomID, ExpectedRevision: 1, Intent: RoomMembershipIntent{TargetUserID: device.Auth.UserID, Action: MembershipAdmit, Role: RoomMember}}); e != nil || r.Outcome != OutcomeCommitted {
			t.Fatalf("admit Binding owner: %+v %v", r, e)
		}
		seq = 3
	}
	source := sha256.Sum256([]byte("agent-" + roomID))
	r, e := f.store.RegisterAgent(ctx, device.Auth, "register-"+roomID, base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeClaude)
	if e != nil {
		t.Fatal(e)
	}
	bindingID := "binding-" + roomID
	at := time.Now().UTC().Truncate(time.Microsecond)
	fact, e := model.NewEnterpriseBindingUpdate(model.EnterpriseBinding{Version: 1, RoomID: roomID, Slot: model.ActorSlot1, BindID: bindingID, Generation: 1, AgentID: r.Control.AgentID, Runtime: model.RuntimeClaude, Active: true, ParkEnabled: park}, at)
	if e != nil {
		t.Fatal(e)
	}
	fact.Seq = seq
	encoding, _ := encodeStoredFact(fact, userFactOrigin(owner.Auth.UserID))
	token := sha256.Sum256([]byte("binding-token-" + roomID))
	tx, e := f.admin.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO bindings(binding_id,org_id,room_id,slot,generation,agent_id) VALUES($1,'org-test',$2,'slot1',1,$3)`, []any{bindingID, roomID, r.Control.AgentID}},
		{`INSERT INTO room_events(room_id,seq,event_id,fact_version,event_encoding,kind,actor,created_at,origin_kind,origin_user) VALUES($1,$7,$2,2,$3,$4,'slot1',$5,'user',$6)`, []any{roomID, fact.ID, encoding, fact.Kind, at, owner.Auth.UserID, int64(seq)}},
		{`UPDATE rooms SET current_seq=$2,revision=$2 WHERE room_id=$1`, []any{roomID, int64(seq)}},
		{`INSERT INTO binding_grants(grant_id,binding_id,family_id,token_hash,restore_epoch,created_at,expires_at) SELECT $1,$2,$3,$4,restore_epoch,$5,$6 FROM identity_policy WHERE id`, []any{"binding-grant-" + roomID, bindingID, c.RequestID, token[:], at, at.Add(time.Hour)}},
	} {
		if _, e = tx.Exec(ctx, q.sql, q.args...); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	if got, e := f.store.AuthenticateBinding(ctx, token, c.Origin); e != nil || got.BindingID != bindingID || got.RoomID != roomID {
		t.Fatalf("binding auth: %+v %v", got, e)
	}
	return bindingID, token
}
func TestDeviceCompositeUserRevocationIncludesArchivedRooms(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, admin := identityPGLogin(t, f, "operator-selected", "admin")
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	first, a := devicePGEnroll(t, f, owner, "one")
	second, b := devicePGEnroll(t, f, owner, "two")
	_, grantA := devicePGSeedBinding(t, f, owner, first, a, "revoke-one", false)
	_, grantB := devicePGSeedBinding(t, f, owner, second, b, "revoke-two", true)
	mustExec(t, f.admin, `UPDATE rooms SET archived_at=now() WHERE room_id='revoke-two'`)
	before := devicePGCounts(t, f)
	mustExec(t, f.admin, `ALTER TABLE audit ADD CONSTRAINT test_user_revoke_rollback CHECK(kind<>'user.status')`)
	failed, failedErr := f.store.SetUserEnabled(ctx, admin.Auth, "disable-owner", owner.Auth.UserID, false)
	if k := kernelErr(t, failed, failedErr); k.Outcome != OutcomeNotCommitted || k.Class != ClassConstraintViolation || failed.Control != nil {
		t.Fatalf("cross-Room audit failure: %+v %v", failed, failedErr)
	}
	var preserved int
	if e := f.admin.QueryRow(ctx, `SELECT count(*) FROM rooms r JOIN bindings b USING(room_id) JOIN binding_grants g USING(binding_id) WHERE r.room_id IN ('revoke-one','revoke-two') AND r.current_seq=2 AND r.revision=2 AND b.retired_at IS NULL AND g.revoked_at IS NULL`).Scan(&preserved); e != nil || preserved != 2 || devicePGCounts(t, f) != before {
		t.Fatalf("cross-Room rollback left partial authority: %d %v", preserved, e)
	}
	mustExec(t, f.admin, `ALTER TABLE audit DROP CONSTRAINT test_user_revoke_rollback`)
	r, e := f.store.SetUserEnabled(ctx, admin.Auth, "disable-owner", owner.Auth.UserID, false)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("composite disable: %+v %v", r, e)
	}
	for _, room := range []string{"revoke-one", "revoke-two"} {
		var seq, revision int
		var retired bool
		var encoding []byte
		if e := f.admin.QueryRow(ctx, `SELECT r.current_seq,r.revision,b.retired_at IS NOT NULL,e.event_encoding FROM rooms r JOIN bindings b USING(room_id) JOIN room_events e ON e.room_id=r.room_id AND e.seq=r.current_seq WHERE r.room_id=$1`, room).Scan(&seq, &revision, &retired, &encoding); e != nil {
			t.Fatal(e)
		}
		if seq != 3 || revision != 3 || !retired {
			t.Fatalf("partial room revocation %s: %d/%d/%v", room, seq, revision, retired)
		}
		var record storedEventRecord
		if e := json.Unmarshal(encoding, &record); e != nil {
			t.Fatal(e)
		}
		binding, e := model.DecodeEnterpriseBindingUpdate(record.Event)
		if e != nil || binding.Active || binding.Runtime != model.RuntimeClaude || binding.ParkEnabled != (room == "revoke-two") {
			t.Fatalf("retirement lost authoritative metadata: %+v %v", binding, e)
		}
	}
	devicePGRefused(t, f, first.Credential.AccessHash, first.Origin)
	devicePGRefused(t, f, second.Credential.AccessHash, second.Origin)
	for _, token := range [][32]byte{grantA, grantB} {
		got, e := f.store.AuthenticateBinding(ctx, token, first.Origin)
		var k *KernelError
		if !errors.As(e, &k) || k.Class != ClassAuthorization || got != (BindingCredential{}) {
			t.Fatalf("retired Binding credential survived: %+v %v", got, e)
		}
	}
	if _, e = f.store.SetUserEnabled(ctx, admin.Auth, "reenable-owner", owner.Auth.UserID, true); e != nil {
		t.Fatal(e)
	}
	var live int
	if e = f.admin.QueryRow(ctx, `SELECT count(*) FROM bindings WHERE retired_at IS NULL`).Scan(&live); e != nil || live != 0 {
		t.Fatalf("reenable revived Bindings: %d %v", live, e)
	}
}
func TestDeviceCompositeRevocationAuditFailureRollsBackRoomsAndGrants(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	c, d := devicePGEnroll(t, f, owner, "rollback-binding")
	_, token := devicePGSeedBinding(t, f, owner, c, d, "rollback-room", true)
	mustExec(t, f.admin, `ALTER TABLE audit ADD CONSTRAINT test_no_device_revoke CHECK(kind<>'device.revoke')`)
	before := devicePGCounts(t, f)
	r, e := f.store.RevokeDevice(ctx, owner.Auth, "revoke", c.DeviceID)
	if k := kernelErr(t, r, e); k.Outcome != OutcomeNotCommitted || k.Class != ClassConstraintViolation {
		t.Fatalf("audit rollback: %+v %v", r, e)
	}
	var seq, rev int
	var retired, revoked bool
	e = f.admin.QueryRow(ctx, `SELECT r.current_seq,r.revision,b.retired_at IS NOT NULL,g.revoked_at IS NOT NULL FROM rooms r JOIN bindings b USING(room_id) JOIN binding_grants g USING(binding_id) WHERE r.room_id='rollback-room'`).Scan(&seq, &rev, &retired, &revoked)
	if e != nil || seq != 2 || rev != 2 || retired || revoked || devicePGCounts(t, f) != before {
		t.Fatalf("audit failure left partial state: %d/%d/%v/%v %v", seq, rev, retired, revoked, e)
	}
	if _, e = f.store.AuthenticateBinding(ctx, token, c.Origin); e != nil {
		t.Fatal(e)
	}
	if _, e = f.store.AuthenticateDevice(ctx, c.Credential.AccessHash, c.Origin); e != nil {
		t.Fatal(e)
	}
}

func TestDeviceTransactionRechecksRevocationAfterRealCredentialLock(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, owner := identityPGLogin(t, f, "member-one", "owner")
	c, device := devicePGEnroll(t, f, owner, "race")
	blocker := schemaConn(t, f.dsn, f.schema)
	tx, e := blocker.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `UPDATE device_grants SET revoked_at=now() WHERE grant_id=$1`, c.Credential.GrantID); e != nil {
		t.Fatal(e)
	}
	source := sha256.Sum256([]byte("new-agent"))
	type response struct {
		r Result
		e error
	}
	done := make(chan response, 1)
	go func() {
		r, e := f.store.RegisterAgent(ctx, device.Auth, "stale-register", base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeCodex)
		done <- response{r, e}
	}()
	waitForControlLockWait(t, f.admin, f.store.work.PgConn().PID(), blocker.PgConn().PID())
	if e = tx.Commit(ctx); e != nil {
		t.Fatal(e)
	}
	got := <-done
	assertIdentityAuthorization(t, got.r, got.e)
	var count int
	if e = f.admin.QueryRow(ctx, `SELECT count(*) FROM agents`).Scan(&count); e != nil || count != 0 {
		t.Fatalf("stale native auth created Agent: %d %v", count, e)
	}
}

func TestDeviceMembershipRevocationIsAtomicAndLimitedToOneRoom(t *testing.T) {
	for _, action := range []MembershipAction{MembershipRemove, MembershipChangeRole} {
		t.Run(string(action), func(t *testing.T) {
			f := identityPGFixture(t, nil)
			ctx := context.Background()
			_, owner := identityPGLogin(t, f, "operator-selected", "owner")
			_, member := identityPGLogin(t, f, "member-one", "member")
			c, d := devicePGEnroll(t, f, member, "member-device")
			_, affected := devicePGSeedBinding(t, f, owner, c, d, "affected-room", true)
			_, unaffected := devicePGSeedBinding(t, f, member, c, d, "other-room", false)
			intent := RoomMembershipIntent{TargetUserID: member.Auth.UserID, Action: action}
			if action == MembershipChangeRole {
				intent.Role = RoomViewer
			}
			cmd := RoomMembershipCommand{Auth: owner.Auth, RoomID: "affected-room", ClientID: "remove-authority", ExpectedRevision: 3, Intent: intent}
			r, e := f.store.CommitRoomMembership(ctx, cmd)
			if e != nil || r.Outcome != OutcomeCommitted || r.Seq != 5 || r.Revision != 4 {
				t.Fatalf("composite membership: %+v %v", r, e)
			}
			var role string
			var disabled, retired bool
			var seq, rev int
			e = f.admin.QueryRow(ctx, `SELECT m.role,m.disabled,b.retired_at IS NOT NULL,r.current_seq,r.revision FROM room_members m JOIN bindings b USING(room_id) JOIN rooms r USING(room_id) WHERE m.room_id='affected-room' AND m.user_id=$1`, member.Auth.UserID).Scan(&role, &disabled, &retired, &seq, &rev)
			if e != nil || !retired || seq != 5 || rev != 4 || action == MembershipRemove && !disabled || action == MembershipChangeRole && (disabled || role != "viewer") {
				t.Fatalf("partial member authority: %s/%v/%v/%d/%d %v", role, disabled, retired, seq, rev, e)
			}
			got, e := f.store.AuthenticateBinding(ctx, affected, c.Origin)
			var k *KernelError
			if !errors.As(e, &k) || k.Class != ClassAuthorization || got != (BindingCredential{}) {
				t.Fatal("affected Binding grant survived")
			}
			if _, e = f.store.AuthenticateBinding(ctx, unaffected, c.Origin); e != nil {
				t.Fatalf("membership revoke crossed Room boundary: %v", e)
			}
			if _, e = f.store.AuthenticateDevice(ctx, c.Credential.AccessHash, c.Origin); e != nil {
				t.Fatalf("membership revoke disabled device family: %v", e)
			}
			r, e = f.store.CommitRoomMembership(ctx, cmd)
			if e != nil || r.Outcome != OutcomeReplayed || r.Seq != 5 || r.Revision != 4 {
				t.Fatalf("composite member receipt replay: %+v %v", r, e)
			}
			var count int
			if e = f.admin.QueryRow(ctx, `SELECT count(*) FROM room_events WHERE room_id='affected-room'`).Scan(&count); e != nil || count != 5 {
				t.Fatal("member replay repeated retirement")
			}
		})
	}
}
