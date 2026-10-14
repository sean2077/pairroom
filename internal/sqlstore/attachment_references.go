package sqlstore

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func (s *Store) readAttachmentReferences(ctx context.Context, q rowQuerier, p opPlan, ids model.EnterpriseAttachmentIDs, lock string) ([8]attachmentState, error) {
	var refs [8]attachmentState
	ordered := ids.Slice()
	sort.Strings(ordered)
	clause := lock
	if lock != "" {
		clause = lockExclusive
	}
	for _, id := range ordered {
		state, err := s.readAttachmentState(ctx, q, id, clause)
		if err != nil {
			return refs, err
		}
		for i, want := range ids {
			if want == id {
				refs[i] = state
				break
			}
		}
	}
	return refs, nil
}
func attachmentReferenceMetadata(p opPlan, d dependencyState) ([]model.Attachment, error) {
	var ids model.EnterpriseAttachmentIDs
	if p.human != nil {
		ids = p.human.AttachmentIDs
	} else if p.relay != nil {
		ids = p.relay.intent.AttachmentIDs
	}
	if ids == (model.EnterpriseAttachmentIDs{}) {
		return nil, nil
	}
	if !attachmentAuthority(p, d, true) {
		return nil, errors.New("attachment reference is not authorized")
	}
	var result []model.Attachment
	var total int64
	for i, id := range ids.Slice() {
		a := d.attachmentRefs[i]
		if a.missing || a.meta.ID != id || a.roomID != p.key.RoomID || (a.state != "verified" && a.state != "referenced") || a.state == "verified" && (!a.epochMatches || a.expired || a.ownerID != p.userID || a.bindingID != d.credential.bindingID) {
			return nil, errors.New("attachment reference is unavailable")
		}
		total += a.meta.Size
		if total > attachment.MaxTotalImageBytes {
			return nil, errors.New("message attachment size limit exceeded")
		}
		result = append(result, a.meta)
	}
	return result, nil
}
func sameAttachmentMetadata(a, b []model.Attachment) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].EqualMetadata(b[i]) {
			return false
		}
	}
	return true
}

func (s *Store) validateHumanMessageReplay(ctx context.Context, tx pgx.Tx, p opPlan, fact model.Event) error {
	if class := validateHumanSendFact(HumanSendCommand{Auth: p.auth, RoomID: p.key.RoomID, ClientID: p.key.Identifier, Intent: *p.human, Fact: fact}); class != "" {
		return &storedFactCorruption{err: errors.New("human message receipt disagrees with frozen intent")}
	}
	message, err := decodeHumanSendPayload(fact.Data)
	if err != nil {
		return &storedFactCorruption{err: err}
	}
	if p.human.QuoteID != "" {
		if err := s.validateHistoricalHumanQuote(ctx, tx, p, fact, message); err != nil {
			return err
		}
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM `+s.table("attachment_references")+` WHERE room_id=$1 AND message_id=$2`, p.key.RoomID, message.ID).Scan(&count); err != nil {
		return err
	}
	if count != len(message.Attachments) {
		return &storedFactCorruption{err: errors.New("human message reference set differs from accepted fact")}
	}
	for _, meta := range message.Attachments {
		var linked bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("attachment_references")+` WHERE room_id=$1 AND message_id=$2 AND object_id=$3 AND first_seq=$4)`, p.key.RoomID, message.ID, meta.ID, int64(fact.Seq)).Scan(&linked); err != nil {
			return err
		}
		a, e := s.readAttachmentState(ctx, tx, meta.ID, lockNone)
		if e != nil {
			return e
		}
		if !linked || a.missing || a.roomID != p.key.RoomID || a.state != "referenced" || !a.meta.EqualMetadata(meta) {
			return &storedFactCorruption{err: errors.New("human message attachment disagrees with immutable reference")}
		}
	}
	return nil
}
func (s *Store) writeAttachmentReferences(ctx context.Context, tx pgx.Tx, roomID, messageID string, seq uint64, refs [8]attachmentState) error {
	for _, a := range refs {
		if a.meta.ID == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("attachment_references")+`(room_id,object_id,message_id,first_seq) VALUES($1,$2,$3,$4) ON CONFLICT(room_id,object_id,message_id) DO NOTHING`, roomID, a.meta.ID, messageID, int64(seq)); err != nil {
			return err
		}
		if a.state == "verified" {
			if _, err := tx.Exec(ctx, `UPDATE `+s.table("attachments")+` SET state='referenced' WHERE object_id=$1 AND state='verified'`, a.meta.ID); err != nil {
				return err
			}
			if err := s.changeAttachmentUsage(ctx, tx, roomID, 0, -1); err != nil {
				return err
			}
		}
	}
	return nil
}

// A Retry retains already referenced immutable objects. It cannot adopt a
// draft, omit an original object, or change metadata while cloning a message.
func (s *Store) copyAttachmentReferences(ctx context.Context, tx pgx.Tx, roomID, originalMessageID, newMessageID string, seq uint64, expected []model.Attachment) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+s.table("attachment_references")+` WHERE room_id=$1 AND message_id=$2`, roomID, originalMessageID).Scan(&count); err != nil {
		return err
	}
	if count != len(expected) {
		return &storedFactCorruption{err: errors.New("retry attachment reference set differs from original")}
	}
	seen := map[string]bool{}
	for _, meta := range expected {
		if seen[meta.ID] {
			return &storedFactCorruption{err: errors.New("duplicate retry attachment")}
		}
		seen[meta.ID] = true
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("attachment_references")+` WHERE room_id=$1 AND message_id=$2 AND object_id=$3)`, roomID, originalMessageID, meta.ID).Scan(&exists); err != nil {
			return err
		}
		a, err := s.readAttachmentState(ctx, tx, meta.ID, lockNone)
		if err != nil {
			return err
		}
		if !exists || a.missing || a.state != "referenced" || a.roomID != roomID || !a.meta.EqualMetadata(meta) {
			return &storedFactCorruption{err: errors.New("retry attachment disagrees with immutable reference")}
		}
		if _, err = tx.Exec(ctx, `INSERT INTO `+s.table("attachment_references")+`(room_id,object_id,message_id,first_seq) VALUES($1,$2,$3,$4)`, roomID, meta.ID, newMessageID, int64(seq)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CommitHumanMessage(ctx context.Context, c HumanMessageCommand) (Result, error) {
	cmd := HumanSendCommand{Auth: c.Auth, RoomID: c.RoomID, ClientID: c.ClientID, ExpectedRevision: c.ExpectedRevision, Intent: c.Intent}
	key, class := humanSendKey(cmd)
	if class != "" {
		return refused(class, nil)
	}
	if c.Auth.Deployment != s.cfg.DeploymentID {
		return refused(ClassAuthorization, nil)
	}
	p := opPlan{auth: c.Auth, userID: c.Auth.UserID, key: key, hash: humanSendHash(key, c.Intent), expectedRevision: c.ExpectedRevision, encodingBudget: humanSendEncodingBudget, human: &c.Intent, produceHuman: true, audit: AuditRecord{Kind: auditKindAppendFact, Outcome: auditOutcomeAccepted}}
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) prepareHumanMessage(ctx context.Context, tx pgx.Tx, p opPlan, d dependencyState, metadata []model.Attachment) (model.Event, error) {
	snapshot, err := s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: p.key.RoomID, DeploymentID: p.key.Deployment, OrgID: d.orgID, HostMode: d.hostMode, Seq: d.currentSeq, Revision: d.revision})
	if err != nil {
		return model.Event{}, err
	}
	id, err := controlID("message")
	if err != nil {
		return model.Event{}, err
	}
	intent := relay.EnterpriseOperationIntent{Kind: relay.EnterpriseOperationSend, To: model.ActorID(p.human.To), Text: p.human.Text, AttachmentIDs: p.human.AttachmentIDs, QuoteID: p.human.QuoteID, Review: p.human.Review}
	prepare, err := relay.PrepareEnterpriseOperationWithAttachments(toRelaySnapshot(snapshot), relay.EnterpriseOperationCaller{Actor: model.ActorUser, UserID: p.userID, ClientID: p.key.Identifier}, intent, relay.EnterpriseOperationIDs{MessageID: id}, metadata)
	if err != nil {
		return model.Event{}, err
	}
	batch, err := prepare(time.Now().UTC())
	if err != nil {
		return model.Event{}, err
	}
	if len(batch.Facts) != 1 {
		return model.Event{}, errors.New("human message requires one fact")
	}
	fact := batch.Facts[0]
	if fact.ID == "" {
		fact.ID, err = controlID("event")
		if err != nil {
			return model.Event{}, err
		}
	}
	if class := validateHumanSendFact(HumanSendCommand{Auth: p.auth, RoomID: p.key.RoomID, ClientID: p.key.Identifier, Intent: *p.human, Fact: fact}); class != "" {
		return model.Event{}, errors.New("shared human producer returned invalid fact")
	}
	return fact, nil
}
