package sqlstore

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"github.com/sean2077/pairroom/internal/review"
)

func evidenceAnchor() review.Anchor {
	return review.Anchor{Schema: 1, Workspace: "endpoint workspace observation", Base: strings.Repeat("a", 40), Head: strings.Repeat("b", 40), DirtySHA256: strings.Repeat("c", 64)}
}
func humanEvidenceCommand(t *testing.T, b *bindingPGState, id string, intent HumanSendIntent) HumanMessageCommand {
	t.Helper()
	var revision uint64
	if err := b.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id='binding-room'`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return HumanMessageCommand{Auth: b.owner.Auth, RoomID: "binding-room", ClientID: id, Intent: intent, ExpectedRevision: revision}
}
func humanEvidenceCommit(t *testing.T, b *bindingPGState, c HumanMessageCommand) Result {
	t.Helper()
	r, err := b.f.store.CommitHumanMessage(t.Context(), c)
	if err != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("human evidence commit: %+v %v", r, err)
	}
	return r
}

func TestHumanEvidenceQuoteMergesImmutableReferencesAndPreservesRecovery(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	bindingPGComplete(t, b, bindingPGCommand(b, "quote-target", model.ActorSlot1, 1, false))
	_, first := attachmentPGVerified(t, b, objects, b.owner.Auth, "source", []byte("source evidence"))
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{first.ID})
	source := humanEvidenceCommit(t, b, humanEvidenceCommand(t, b, "source-send", HumanSendIntent{To: "slot1", Text: "complete original body", AttachmentIDs: ids, Review: evidenceAnchor()}))
	sourceMessage, err := decodeHumanSendPayload(source.Fact.Data)
	if err != nil {
		t.Fatal(err)
	}
	_, second := attachmentPGVerified(t, b, objects, b.owner.Auth, "direct", []byte("direct evidence"))
	ids, _ = model.NewEnterpriseAttachmentIDs([]string{second.ID, first.ID})
	intent := HumanSendIntent{To: "slot1", Text: "quoted reply", AttachmentIDs: ids, QuoteID: sourceMessage.ID, Review: evidenceAnchor()}
	cmd := humanEvidenceCommand(t, b, "quoted-send", intent)
	before := attachmentPGCounts(t, b)
	result := humanEvidenceCommit(t, b, cmd)
	payload, err := decodeHumanSendPayload(result.Fact.Data)
	if err != nil || payload.Author != relay.EnterpriseHumanAuthor(b.owner.Auth.UserID) || payload.Quote == nil || payload.Quote.FromHandle != "@user" || payload.Quote.Author != sourceMessage.Author || payload.Quote.Text != sourceMessage.Text || payload.Review == nil || *payload.Review != intent.Review || len(payload.Attachments) != 2 || !payload.Attachments[0].EqualMetadata(second) || !payload.Attachments[1].EqualMetadata(first) {
		t.Fatal("quoted producer lost explicit/source evidence")
	}
	after := attachmentPGCounts(t, b)
	if after[2] != before[2]+2 || after[5] != before[5]-1 || after[4] != before[4] || after[6] != before[6]+1 {
		t.Fatalf("quote references/quota not atomic: %v -> %v", before, after)
	}
	if err := objects.Delete(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	for _, resolve := range []bool{true, false} {
		var got Result
		var err error
		if resolve {
			got, err = b.f.store.ResolveHumanSend(t.Context(), cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
		} else {
			got, err = b.f.store.CommitHumanMessage(t.Context(), cmd)
		}
		if err != nil || got.Outcome != OutcomeReplayed || !reflect.DeepEqual(got.Fact, result.Fact) {
			t.Fatalf("accepted evidence changed on recovery: %+v %v", got, err)
		}
	}
	for _, change := range []func(*HumanSendIntent){func(i *HumanSendIntent) { i.QuoteID = "another-source" }, func(i *HumanSendIntent) { i.Review.Head = strings.Repeat("d", 40) }} {
		changed := cmd.Intent
		change(&changed)
		r, err := b.f.store.ResolveHumanSend(t.Context(), cmd.Auth, cmd.RoomID, cmd.ClientID, changed)
		if kernelErr(t, r, err).Class != ClassPayloadConflict {
			t.Fatal("quote/review missing from idempotency hash")
		}
	}
	if attachmentPGCounts(t, b) != after {
		t.Fatal("receipt recovery changed refs/quota/facts")
	}
	r, err := b.f.store.CommitHumanSend(t.Context(), HumanSendCommand{Auth: cmd.Auth, RoomID: cmd.RoomID, ClientID: "forged-raw", Intent: cmd.Intent, Fact: result.Fact})
	if kernelErr(t, r, err).Class != ClassInvalidRequest {
		t.Fatal("raw fact entry attested a quote")
	}
}

func TestHumanEvidenceQuoteAuditFailureRollsBackReferences(t *testing.T) {
	b, objects := attachmentPGFixture(t, nil)
	bindingPGComplete(t, b, bindingPGCommand(b, "audit-quote", model.ActorSlot1, 1, false))
	_, meta := attachmentPGVerified(t, b, objects, b.owner.Auth, "original", []byte("immutable"))
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	source := humanEvidenceCommit(t, b, humanEvidenceCommand(t, b, "original-send", HumanSendIntent{To: "slot1", Text: "original", AttachmentIDs: ids}))
	original, _ := decodeHumanSendPayload(source.Fact.Data)
	before := attachmentPGCounts(t, b)
	if _, err := b.f.admin.Exec(t.Context(), `ALTER TABLE audit ADD CONSTRAINT refuse_quoted_append CHECK(kind<>'fact.append') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	cmd := humanEvidenceCommand(t, b, "refused-quote", HumanSendIntent{To: "slot1", Text: "reply", QuoteID: original.ID})
	r, err := b.f.store.CommitHumanMessage(t.Context(), cmd)
	if kernelErr(t, r, err).Class != ClassConstraintViolation {
		t.Fatal("audit fault did not reach quote transaction")
	}
	if attachmentPGCounts(t, b) != before {
		t.Fatal("quote rollback left partial object reference/fact/quota")
	}
}

func TestRelayEvidenceQuoteReferencesAreAtomicAndReplayedBeforeNewPeerChecks(t *testing.T) {
	b, first, _ := relayPGPair(t, nil)
	limits := AttachmentLimits{DeploymentBytes: 1024, RoomBytes: 512, DeploymentPending: 16, RoomPending: 8}
	b.f.store.cfg.AttachmentLimits = limits
	b.f.cfg.AttachmentLimits = limits
	objects, err := enterpriseobject.Open(filepath.Join(t.TempDir(), "objects"), b.f.cfg.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	_, meta := attachmentPGVerified(t, b, objects, first.Auth, "native-object", []byte("native evidence"))
	ids, _ := model.NewEnterpriseAttachmentIDs([]string{meta.ID})
	source := relayPGCommit(t, b, relayPGCommand(t, b, first, "native-source", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "native original", AttachmentIDs: ids, ExpectedPeerGeneration: 1}))
	cmd := relayPGCommand(t, b, first, "native-quote", relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, Text: "explicit response", QuoteID: source.Relay.Message.ID, Review: evidenceAnchor(), ExpectedPeerGeneration: 1})
	before := attachmentPGCounts(t, b)
	result := relayPGCommit(t, b, cmd)
	after := attachmentPGCounts(t, b)
	if after[2] != before[2]+1 || after[4] != before[4] || after[5] != before[5] || result.Relay.Message.Quote == nil || len(result.Relay.Message.Attachments) != 1 {
		t.Fatal("Native quote failed to retain source reference without quota change")
	}
	bindingPGComplete(t, b, bindingPGCommand(b, "quote-replaced-peer", model.ActorSlot2, 1, true))
	after = attachmentPGCounts(t, b)
	got, err := b.f.store.ResolveRelayOperation(context.Background(), cmd)
	if err != nil || got.Outcome != OutcomeReplayed || !reflect.DeepEqual(got.Relay, result.Relay) {
		t.Fatalf("quoted Native receipt replay: %+v %v", got, err)
	}
	cmd.Intent.Review.DirtySHA256 = strings.Repeat("d", 64)
	r, err := b.f.store.CommitRelayOperation(t.Context(), cmd)
	if kernelErr(t, r, err).Class != ClassPayloadConflict {
		t.Fatal("Native review changed under accepted ID")
	}
	if attachmentPGCounts(t, b) != after {
		t.Fatal("Native replay changed attachment quota/reference set")
	}
	cmd.ClientID = "late-new-quote"
	cmd.Intent.Review = evidenceAnchor()
	r, err = b.f.store.CommitRelayOperation(t.Context(), cmd)
	if kernelErr(t, r, err).Class != ClassRevisionConflict {
		t.Fatal("new effect with stale revision was not refused")
	}
	cmd = relayPGCommand(t, b, first, cmd.ClientID, cmd.Intent)
	r, err = b.f.store.CommitRelayOperation(t.Context(), cmd)
	if kernelErr(t, r, err).Class != ClassPeerChanged {
		t.Fatal("new quote retagged old peer context")
	}
}
