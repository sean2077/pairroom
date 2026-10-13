package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func compoundSecondDevice(t *testing.T, b *bindingPGState, id string) *bindingPGState {
	t.Helper()
	enrollment, device := devicePGEnroll(t, b.f, b.member, id)
	source := sha256.Sum256([]byte("compound-source-" + id))
	registered, err := b.f.store.RegisterAgent(t.Context(), device.Auth, "register-"+id, base64.RawURLEncoding.EncodeToString(source[:]), model.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	second := *b
	second.device, second.enrollment, second.agentID = device, enrollment, registered.Control.AgentID
	return &second
}

func compoundMessages(t *testing.T, b *bindingPGState, auth AuthContext) map[string]relay.Message {
	t.Helper()
	snapshot, err := b.f.store.ReadonlySnapshot(t.Context(), auth, "binding-room")
	if err != nil {
		t.Fatal(err)
	}
	projected, err := relay.ProjectEnterpriseSnapshot(toRelaySnapshot(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	messages := map[string]relay.Message{}
	for _, message := range projected.Messages {
		messages[message.ID] = message
	}
	return messages
}

func compoundRole(t *testing.T, b *bindingPGState) string {
	t.Helper()
	var role string
	var disabled bool
	if err := b.f.admin.QueryRow(t.Context(), `SELECT role,disabled FROM room_members WHERE room_id='binding-room' AND user_id=$1`, b.member.Auth.UserID).Scan(&role, &disabled); err != nil || disabled {
		t.Fatal("original User membership was disabled", err)
	}
	return role
}

func TestCompoundSameUserTwoDevicesRevokeOnePreservesOtherRefreshBindingAndHistory(t *testing.T) {
	b := bindingPGFixture(t, nil)
	firstRequest := bindingPGCommand(b, "device-one-slot", model.ActorSlot1, 1, false)
	_, first := bindingPGComplete(t, b, firstRequest)
	other := compoundSecondDevice(t, b, "device-two")
	secondRequest := bindingPGCommand(other, "device-two-slot", model.ActorSlot2, 1, false)
	_, second := bindingPGComplete(t, other, secondRequest)
	if b.device.DeviceID == other.device.DeviceID || b.device.Auth.UserID != other.device.Auth.UserID {
		t.Fatal("fixture is not two live Devices for one User")
	}
	humanEvidenceCommit(t, b, humanEvidenceCommand(t, b, "device-scope-history", HumanSendIntent{To: "slot2", Text: "original Room history for surviving device"}))
	before := compoundMessages(t, b, b.member.Auth)
	if len(before) != 1 || compoundRole(t, b) != "member" {
		t.Fatal("fixture lacks original member history")
	}
	revoked, err := b.f.store.RevokeDevice(t.Context(), b.member.Auth, "revoke-first-only", b.device.DeviceID)
	if err != nil || revoked.Outcome != OutcomeCommitted {
		t.Fatal("single Device revocation failed", err)
	}
	devicePGRefused(t, b.f, b.enrollment.Credential.AccessHash, b.enrollment.Origin)
	result, err := b.f.store.RotateDeviceCredential(t.Context(), devicePGRotation(b.enrollment, "revoked-first"))
	if k := kernelErr(t, result, err); k.Class != ClassAuthorization || result.Control != nil {
		t.Fatal("revoked first Device refresh survived")
	}
	if credential, err := b.f.store.AuthenticateBinding(t.Context(), firstRequest.Grant.TokenHash, b.enrollment.Origin); err == nil || credential != (BindingCredential{}) {
		t.Fatal("first Device narrow grant survived")
	}
	if snapshot, err := b.f.store.ReadonlySnapshot(t.Context(), first.Auth, "binding-room"); err == nil || len(snapshot.Events) != 0 {
		t.Fatal("revoked first narrow credential disclosed private history")
	}
	beforeDenied := compoundBusinessCounts(t, b)
	denied, deniedErr := b.f.store.CommitRelayOperation(t.Context(), relayPGCommand(t, b, first, "revoked-first-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
	if k := kernelErr(t, denied, deniedErr); k.Class != ClassAuthorization || denied.Relay != nil || len(denied.Facts) != 0 || compoundBusinessCounts(t, b) != beforeDenied {
		t.Fatal("revoked first narrow credential emitted envelope or business effect")
	}
	live, err := b.f.store.AuthenticateDevice(t.Context(), other.enrollment.Credential.AccessHash, other.enrollment.Origin)
	if err != nil || live.DeviceID != other.device.DeviceID || live.Auth.UserID != b.member.Auth.UserID {
		t.Fatal("single revocation widened to second live access", err)
	}
	rotation := devicePGRotation(other.enrollment, "surviving-second")
	rotated, err := b.f.store.RotateDeviceCredential(t.Context(), rotation)
	if err != nil || rotated.Outcome != OutcomeCommitted {
		t.Fatal("surviving second refresh failed", err)
	}
	renewed, err := b.f.store.AuthenticateDevice(t.Context(), rotation.Next.AccessHash, other.enrollment.Origin)
	if err != nil || renewed.DeviceID != other.device.DeviceID || renewed.Auth.DeviceGrantID != rotation.Next.GrantID {
		t.Fatal("actual rotated access did not authenticate", err)
	}
	current, err := b.f.store.AuthenticateBinding(t.Context(), secondRequest.Grant.TokenHash, other.enrollment.Origin)
	if err != nil || current.BindingID != second.BindingID || current.Generation != second.Generation {
		t.Fatal("second Binding did not survive independent refresh", err)
	}
	if compoundRole(t, b) != "member" || !reflect.DeepEqual(compoundMessages(t, b, b.member.Auth), before) {
		t.Fatal("single Device revocation changed User membership or history")
	}
	claim := relayPGCommit(t, b, relayPGCommand(t, b, current, "surviving-second-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
	if claim.Relay.Claim == nil {
		t.Fatal("surviving second Binding could not claim actual queued history")
	}
	ack := relayPGCommit(t, b, relayPGCommand(t, b, current, "surviving-second-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: claim.Relay.Claim.ID, Receipt: claim.Relay.Claim.Receipt}))
	if !ack.Relay.HandedOff {
		t.Fatal("surviving Binding could not settle its own receipt")
	}
	var liveBindings int
	if err := b.f.admin.QueryRow(t.Context(), `SELECT count(*) FROM bindings b JOIN agents a USING(agent_id) JOIN devices d USING(device_id) WHERE d.user_id=$1 AND b.retired_at IS NULL`, b.member.Auth.UserID).Scan(&liveBindings); err != nil || liveBindings != 1 {
		t.Fatal("single Device revoke retired wrong Binding set", err)
	}
	t.Log("same User/two Devices: first access/refresh/narrow/read denied; second rotated access + original Binding claim/ACK live; User membership/history retained")
}

func TestCompoundTypedReplacementPreservesOriginalUserMembershipHistoryAndAuthors(t *testing.T) {
	b := bindingPGFixture(t, nil)
	oldRequest := bindingPGCommand(b, "original-participant", model.ActorSlot2, 1, false)
	original, old := bindingPGComplete(t, b, oldRequest)
	humanEvidenceCommit(t, b, humanEvidenceCommand(t, b, "original-stable-human", HumanSendIntent{To: "slot2", Text: "stable original authenticated human author"}))
	native := relayPGCommit(t, b, relayPGCommand(t, b, old, "original-native-author", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: model.ActorUser, Text: "original native generation evidence", ExpectedPeerGeneration: 1}))
	before := compoundMessages(t, b, b.member.Auth)
	var human relay.Message
	for _, message := range before {
		if message.From == model.ActorUser {
			human = message
		}
	}
	if human.Author != relay.EnterpriseHumanAuthor(b.owner.Auth.UserID) || native.Relay.Message.FromRuntime != model.RuntimeClaude || native.Relay.Message.FromGeneration != 1 {
		t.Fatal("fixture lacks stable human/native provenance")
	}
	replacement := bindingPGCommand(b, "replacement-participant", model.ActorSlot2, 1, true)
	_, fresh := bindingPGComplete(t, b, replacement)
	if fresh.Generation != 2 || compoundRole(t, b) != "member" {
		t.Fatal("replacement changed User membership or generation")
	}
	if err := b.f.store.AuthorizeRoomRead(t.Context(), b.member.Auth, "binding-room"); err != nil {
		t.Fatal("replacement removed original User Room access", err)
	}
	after := compoundMessages(t, b, b.member.Auth)
	if after[human.ID].Author != human.Author || after[human.ID].From != human.From || after[human.ID].Text != human.Text || after[human.ID].TargetGeneration != 1 || after[human.ID].State != "cancelled" || !reflect.DeepEqual(after[native.Relay.Message.ID], before[native.Relay.Message.ID]) {
		t.Fatal("replacement rewrote original human/native provenance or history")
	}
	if credential, err := b.f.store.AuthenticateBinding(t.Context(), oldRequest.Grant.TokenHash, b.enrollment.Origin); err == nil || credential != (BindingCredential{}) {
		t.Fatal("old generation narrow token authenticated")
	}
	if snapshot, err := b.f.store.ReadonlySnapshot(t.Context(), old.Auth, "binding-room"); err == nil || len(snapshot.Events) != 0 {
		t.Fatal("old narrow generation disclosed private Room facts")
	}
	beforeDenied := compoundBusinessCounts(t, b)
	rejected, err := b.f.store.CommitRelayOperation(t.Context(), relayPGCommand(t, b, old, "retired-generation-send", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: model.ActorUser, Text: "must not publish"}))
	if k := kernelErr(t, rejected, err); k.Class != ClassAuthorization || rejected.Relay != nil || len(rejected.Facts) != 0 {
		t.Fatal("old narrow generation produced an effect")
	}
	resolved, err := b.f.store.ResolveRelayOperation(t.Context(), relayPGCommand(t, b, old, "original-native-author", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: model.ActorUser, Text: "original native generation evidence", ExpectedPeerGeneration: 1}))
	if k := kernelErr(t, resolved, err); k.Class != ClassAuthorization || resolved.Relay != nil || len(resolved.Facts) != 0 {
		t.Fatal("old narrow token disclosed its accepted private receipt after replacement")
	}
	if compoundBusinessCounts(t, b) != beforeDenied {
		t.Fatal("retired narrow attempts wrote business fact/operation/audit")
	}
	// Original Device-scoped completion recovery remains allowed while that
	// Device and User are live. Replacement cannot relabel an accepted receipt.
	replayed, err := b.f.store.ResolveBindingCompletion(t.Context(), BindingCompletionCommand{Auth: oldRequest.Auth, RoomID: oldRequest.RoomID, RequestID: oldRequest.RequestID, ClientID: "complete-" + oldRequest.RequestID, ProofHash: oldRequest.ProofHash})
	if err != nil || replayed.Outcome != OutcomeReplayed || !reflect.DeepEqual(replayed.BindingAuthority, original.BindingAuthority) || !reflect.DeepEqual(replayed.Facts, original.Facts) {
		t.Fatal("live original Device completion recovery was improperly revoked", err)
	}
	if current, err := b.f.store.AuthenticateBinding(t.Context(), replacement.Grant.TokenHash, b.enrollment.Origin); err != nil || current.Generation != 2 {
		t.Fatal("replacement narrow generation unavailable", err)
	}
	t.Log("typed replacement: original User role/read/history and stable human/native authors retained; old narrow grant denied; live original Device-scope completion receipt replayed exactly")
}

func compoundBusinessCounts(t *testing.T, b *bindingPGState) [3]int64 {
	t.Helper()
	var counts [3]int64
	if err := b.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM room_events WHERE room_id='binding-room' AND kind=$1),(SELECT count(*) FROM relay_operations WHERE room_id='binding-room'),(SELECT count(*) FROM audit WHERE room_id='binding-room' AND kind LIKE 'relay.%')`, relay.EventMessage).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestCompoundUserDisableSendAndClaimBothDatabaseLockOrderings(t *testing.T) {
	for _, kind := range []relay.EnterpriseOperationKind{relay.EnterpriseOperationSend, relay.EnterpriseOperationClaim} {
		for _, disableFirst := range []bool{false, true} {
			name := string(kind) + "-business-first"
			if disableFirst {
				name = string(kind) + "-disable-first"
			}
			t.Run(name, func(t *testing.T) {
				b, owner, member := relayPGPair(t, nil)
				var originalID string
				if kind == relay.EnterpriseOperationClaim {
					sent := relayPGCommit(t, b, relayPGCommand(t, b, owner, "before-disable-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "owner message queued before race", ExpectedPeerGeneration: 1}))
					originalID = sent.Relay.Message.ID
				}
				before := compoundBusinessCounts(t, b)
				intent := relay.EnterpriseOperationIntent{Kind: kind}
				if kind == relay.EnterpriseOperationSend {
					intent.Text = "accepted native source before User disable"
					intent.ExpectedPeerGeneration = 1
				}
				command := relayPGCommand(t, b, member, "racing-business", intent)
				work := schemaConn(t, b.f.dsn, b.f.schema)
				if err := configureSession(t.Context(), work, b.f.schema); err != nil {
					t.Fatal(err)
				}
				other := &Store{cfg: b.f.store.cfg, lease: b.f.store.lease, work: work, sem: make(chan struct{}, 1)}
				blocker := schemaConn(t, b.f.dsn, b.f.schema)
				tx, err := blocker.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(context.Background())
				// Business-first holds Room while its shared ownership fence is live.
				// Disable-first holds target User after disable owns the exclusive fence;
				// the business then waits on that fence. The exact blocking chain is observed.
				query := `SELECT room_id FROM rooms WHERE room_id='binding-room' FOR UPDATE`
				var lockArgs []any
				if disableFirst {
					query = `SELECT user_id FROM users WHERE user_id=$1 FOR UPDATE`
					lockArgs = []any{b.member.Auth.UserID}
				}
				if _, err := tx.Exec(t.Context(), query, lockArgs...); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
				defer cancel()
				var business, disabled Result
				var businessErr, disableErr error
				var wg sync.WaitGroup
				launchBusiness := func() {
					wg.Add(1)
					go func() { defer wg.Done(); business, businessErr = b.f.store.CommitRelayOperation(ctx, command) }()
				}
				launchDisable := func() {
					wg.Add(1)
					go func() {
						defer wg.Done()
						disabled, disableErr = other.SetUserEnabled(ctx, b.admin.Auth, "racing-disable", b.member.Auth.UserID, false)
					}()
				}
				defer func() { _ = tx.Rollback(context.Background()); cancel(); wg.Wait() }()
				firstPID := b.f.store.work.PgConn().PID()
				secondPID := other.work.PgConn().PID()
				if disableFirst {
					launchDisable()
					firstPID, secondPID = secondPID, firstPID
				} else {
					launchBusiness()
				}
				waitForControlLockWait(t, b.f.admin, firstPID, blocker.PgConn().PID())
				if disableFirst {
					launchBusiness()
				} else {
					launchDisable()
				}
				waitIdentityControlGuardChain(t, b.f, []uint32{firstPID, secondPID}, blocker.PgConn().PID())
				if err := tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				wg.Wait()
				if disableErr != nil || disabled.Outcome != OutcomeCommitted {
					t.Fatal("typed User disable did not commit", disableErr)
				}
				if disableFirst {
					if k := kernelErr(t, business, businessErr); k.Class != ClassAuthorization || business.Relay != nil || len(business.Facts) != 0 {
						t.Fatal("disable-first admitted business or a private envelope")
					}
					if compoundBusinessCounts(t, b) != before {
						t.Fatal("disable-first wrote business facts/receipt/audit")
					}
				} else {
					if businessErr != nil || business.Outcome != OutcomeCommitted || business.Relay == nil || len(business.Facts) != 1 {
						t.Fatal("business-first original accepted effect lost", businessErr)
					}
					after := compoundBusinessCounts(t, b)
					if after != [3]int64{before[0] + 1, before[1] + 1, before[2] + 1} {
						t.Fatal("business-first fact/receipt/audit was not exactly once")
					}
					var stored []byte
					var storedSeq, storedRevision uint64
					if err := b.f.admin.QueryRow(t.Context(), `SELECT result_encoding,result_seq,result_revision FROM relay_operations WHERE room_id='binding-room' AND identifier='racing-business'`).Scan(&stored, &storedSeq, &storedRevision); err != nil {
						t.Fatal(err)
					}
					var receipt relay.EnterpriseOperationResult
					if json.Unmarshal(stored, &receipt) != nil || !reflect.DeepEqual(&receipt, business.Relay) || storedSeq != business.Seq || storedRevision != business.Revision {
						t.Fatal("User disable rewrote accepted original receipt/cursor")
					}
					var encoding []byte
					if err := b.f.admin.QueryRow(t.Context(), `SELECT event_encoding FROM room_events WHERE room_id='binding-room' AND seq=$1`, business.Seq).Scan(&encoding); err != nil {
						t.Fatal(err)
					}
					var canonical storedEventRecord
					if json.Unmarshal(encoding, &canonical) != nil || canonical.OriginKind != "user" || canonical.OriginUser == nil || *canonical.OriginUser != b.member.Auth.UserID || !reflect.DeepEqual(canonical.Event, business.Facts[0]) {
						t.Fatal("User disable rewrote committed canonical fact or immutable original claimant/source User")
					}
					messages := compoundMessages(t, b, b.owner.Auth)
					if kind == relay.EnterpriseOperationClaim {
						if business.Relay.Claim == nil || business.Relay.Claim.ID != originalID || business.Relay.Claim.Receipt == "" || business.Relay.Claim.Envelope == "" || messages[originalID].State != "unknown" || messages[originalID].FromRuntime != model.RuntimeClaude || messages[originalID].TargetGeneration != 1 {
							t.Fatal("accepted claim lost receipt/envelope or retirement changed original identity")
						}
					} else {
						originalID = business.Relay.Message.ID
						if messages[originalID].State != "queued" || messages[originalID].FromRuntime != model.RuntimeClaude || messages[originalID].FromGeneration != 1 || messages[originalID].TargetGeneration != 1 {
							t.Fatal("source disable undid accepted queued message provenance")
						}
						liveClaim := relayPGCommit(t, b, relayPGCommand(t, b, owner, "surviving-owner-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
						if liveClaim.Relay.Claim == nil || liveClaim.Relay.Claim.ID != originalID {
							t.Fatal("live target could not receive already accepted work")
						}
					}
				}
				if snapshot, err := b.f.store.ReadonlySnapshot(t.Context(), member.Auth, "binding-room"); err == nil || len(snapshot.Events) != 0 {
					t.Fatal("disabled old narrow credential disclosed private state")
				}
				resolved, err := b.f.store.ResolveRelayOperation(t.Context(), command)
				if k := kernelErr(t, resolved, err); k.Class != ClassAuthorization || resolved.Relay != nil || len(resolved.Facts) != 0 {
					t.Fatal("disabled old narrow credential disclosed accepted receipt")
				}
				if err := b.f.store.AuthorizeRoomRead(t.Context(), b.owner.Auth, "binding-room"); err != nil {
					t.Fatal("target User disable widened to original live Owner", err)
				}
				t.Logf("%s: exact PostgreSQL lock chain observed; business outcome=%s; disabled credentials fail closed; accepted original facts/receipts preserved", name, business.Outcome)
			})
		}
	}
}
