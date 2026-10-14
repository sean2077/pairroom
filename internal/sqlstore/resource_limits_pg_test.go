package sqlstore

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func TestResourceReplayChecksActualBytesBeforeCorruptFactDecode(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Commit(t.Context(), f.command("resource-preflight")); err != nil {
		t.Fatal(err)
	}
	f.store.cfg.ResourceLimits = ResourceLimits{ReplayBytes: 512, NewWorkBytes: 256}
	if _, err := f.admin.Exec(t.Context(), `UPDATE room_events SET event_encoding=$1 WHERE room_id='room-1'`, []byte(strings.Repeat("x", 600))); err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.store.ReadonlySnapshot(t.Context(), AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassResourceLimit)
	if len(snapshot.Events) != 0 {
		t.Fatal("oversized replay disclosed a partial log")
	}
	f.store.cfg.ResourceLimits = ResourceLimits{}
	if _, err := f.admin.Exec(t.Context(), `UPDATE rooms SET current_seq=9223372036854775807 WHERE room_id='room-1'`); err != nil {
		t.Fatal(err)
	}
	_, err = f.store.ReadonlySnapshot(t.Context(), AuthContext{Deployment: "dep-test", UserID: "u-owner"}, "room-1")
	expectKernel(t, err, OutcomeRefused, ClassStateInvalid)
}

func TestResourceActionableAdmissionRaceUsesExclusiveFence(t *testing.T) {
	b := bindingPGFixture(t, nil)
	// Fixture has five enabled Users, three browser sessions, one Device,
	// one family reserving three credential identities, and one Agent: 13.
	b.f.store.cfg.ResourceLimits = ResourceLimits{ActionableIdentities: 14}
	type answer struct {
		result Result
		err    error
	}
	answers := make(chan answer, 2)
	for _, id := range []string{"resource-agent-a", "resource-agent-b"} {
		id := id
		go func() {
			key := sha256.Sum256([]byte(id))
			result, err := b.f.store.RegisterAgent(t.Context(), b.device.Auth, id, base64.RawURLEncoding.EncodeToString(key[:]), model.RuntimeClaude)
			answers <- answer{result, err}
		}()
	}
	accepted, refused := 0, 0
	for i := 0; i < 2; i++ {
		a := <-answers
		if a.err == nil && a.result.Outcome == OutcomeCommitted {
			accepted++
		} else if a.result.Outcome == OutcomeRefused && a.result.Class == ClassResourceLimit {
			refused++
		} else {
			t.Fatalf("unexpected fenced admission: %s/%s", a.result.Outcome, a.result.Class)
		}
	}
	if accepted != 1 || refused != 1 {
		t.Fatalf("exclusive admission count violated: accepted=%d refused=%d", accepted, refused)
	}
	var agents, operations int
	if err := b.f.admin.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM agents),(SELECT count(*) FROM control_operations WHERE identifier IN ('resource-agent-a','resource-agent-b'))`).Scan(&agents, &operations); err != nil {
		t.Fatal(err)
	}
	if agents != 2 || operations != 1 {
		t.Fatal("rejected agent left partial authority or receipt")
	}
}

func TestResourceRoomRecordCapCountsArchivedHistoryAndRecoversAcceptedCreate(t *testing.T) {
	f := newFixture(t)
	f.store.cfg.ResourceLimits = ResourceLimits{PersistentRooms: 2}
	c := roomCreateCommand(t)
	accepted, err := f.store.CommitRoomCreate(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(t.Context(), `UPDATE rooms SET archived_at=clock_timestamp() WHERE room_id=$1`, c.RoomID); err != nil {
		t.Fatal(err)
	}
	replay, err := f.store.CommitRoomCreate(t.Context(), c)
	if err != nil || replay.Outcome != OutcomeReplayed || replay.Seq != accepted.Seq {
		t.Fatal("quota blocked accepted-result recovery")
	}
	other := c
	other.RoomID = "room-resource-overflow"
	other.ClientID = "resource-overflow"
	result, err := f.store.CommitRoomCreate(t.Context(), other)
	if got := kernelErr(t, result, err); got.Class != ClassResourceLimit || got.Outcome != OutcomeRefused {
		t.Fatal("archive recycled persistent metadata budget")
	}
	if counts := creationCounts(t, f, other.RoomID); counts != [7]int64{} {
		t.Fatal("refused creation left partial SQL state")
	}
}

func TestResourceSoftBudgetPreservesOriginalClaimAckAndMandatoryRevoke(t *testing.T) {
	b, _, member := relayPGPair(t, nil)
	send := humanEvidenceCommand(t, b, "resource-original", HumanSendIntent{To: "slot2", Text: strings.Repeat("x", 4096)})
	accepted := humanEvidenceCommit(t, b, send)
	b.f.store.cfg.ResourceLimits = ResourceLimits{NewWorkEvents: int64(accepted.Seq)}
	refusedSend := humanEvidenceCommand(t, b, "resource-refused", HumanSendIntent{To: "slot2", Text: "new work"})
	r, err := b.f.store.CommitHumanMessage(t.Context(), refusedSend)
	if got := kernelErr(t, r, err); got.Class != ClassResourceLimit {
		t.Fatal("soft quota admitted new work")
	}
	recovered, err := b.f.store.ResolveHumanSend(t.Context(), send.Auth, send.RoomID, send.ClientID, send.Intent)
	if err != nil || recovered.Outcome != OutcomeReplayed || recovered.Fact.ID != accepted.Fact.ID {
		t.Fatal("quota prevented accepted result recovery")
	}
	claim := relayPGCommit(t, b, relayPGCommand(t, b, member, "resource-claim", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationClaim}))
	if claim.Relay.Claim == nil {
		t.Fatal("quota trapped previously queued work")
	}
	ack := relayPGCommit(t, b, relayPGCommand(t, b, member, "resource-ack", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationAck, MessageID: claim.Relay.Claim.ID, Receipt: claim.Relay.Claim.Receipt}))
	if !ack.Relay.HandedOff {
		t.Fatal("quota blocked original acknowledgement")
	}
	var revision uint64
	if err := b.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	r, err = b.f.store.CommitRoomMembership(t.Context(), RoomMembershipCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: "resource-revoke", ExpectedRevision: revision, Intent: RoomMembershipIntent{TargetUserID: b.member.Auth.UserID, Action: MembershipRemove}})
	if err != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("quota blocked mandatory revocation: %s %v", r.Class, err)
	}
	if err := b.f.store.AuthorizeRoomRead(t.Context(), member.Auth, "binding-room"); err == nil {
		t.Fatal("revoked Binding retained Room read authority")
	}
}
