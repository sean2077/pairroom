package sqlstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func (s *Store) validateHumanMessageCandidate(ctx context.Context, tx pgx.Tx, p opPlan, d dependencyState, fact model.Event, explicit []model.Attachment) error {
	payload, err := decodeHumanSendPayload(fact.Data)
	if err != nil {
		return err
	}
	if p.human.QuoteID == "" {
		if !sameAttachmentMetadata(payload.Attachments, explicit) {
			return errors.New("human explicit attachment metadata differs")
		}
		return nil
	}
	if !p.produceHuman {
		return errors.New("raw human fact cannot attest quoted content")
	}
	if validateHumanSendFact(HumanSendCommand{Auth: p.auth, RoomID: p.key.RoomID, ClientID: p.key.Identifier, Intent: *p.human, Fact: fact}) != "" {
		return errors.New("invalid quoted human candidate")
	}
	snapshot, err := s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: p.key.RoomID, DeploymentID: p.key.Deployment, OrgID: d.orgID, HostMode: d.hostMode, Seq: d.currentSeq, Revision: d.revision})
	if err != nil {
		return err
	}
	source, err := quotedMessage(toRelaySnapshot(snapshot), p.human.QuoteID)
	if err != nil {
		return err
	}
	return matchQuotedHuman(payload, source, explicit)
}

func quotedMessage(snapshot relay.EnterpriseRoomSnapshot, id string) (relay.Message, error) {
	page, err := relay.ProjectEnterpriseHistory(snapshot, relay.HistoryQuery{ID: id})
	if err != nil {
		return relay.Message{}, err
	}
	if len(page.Messages) != 1 {
		return relay.Message{}, errors.New("quoted source is not in this Room")
	}
	return page.Messages[0], nil
}
func matchQuotedHuman(payload humanSendPayload, source relay.Message, explicit []model.Attachment) error {
	handle := "@user"
	if source.From.ValidParticipant() {
		handle = model.NativeParticipantIdentityFor(source.From).MentionHandle
	}
	if payload.Quote == nil || payload.Quote.FromHandle != handle || payload.Quote.Text != source.Text {
		return errors.New("quoted content disagrees with original Room message")
	}
	author := ""
	if source.From == model.ActorUser && relay.ValidEnterpriseHumanAuthor(source.Author) {
		author = source.Author
	}
	if payload.Quote.Author != author {
		return errors.New("quoted author disagrees with original Room message")
	}
	merged := append([]model.Attachment(nil), explicit...)
	seen := map[string]bool{}
	for _, a := range merged {
		seen[a.ID] = true
	}
	for _, a := range source.Attachments {
		if !seen[a.ID] {
			merged = append(merged, a)
			seen[a.ID] = true
		}
	}
	if !sameAttachmentMetadata(payload.Attachments, merged) {
		return errors.New("quoted attachment union differs from original Room message")
	}
	return nil
}

func (s *Store) validateHistoricalHumanQuote(ctx context.Context, tx pgx.Tx, p opPlan, fact model.Event, payload humanSendPayload) error {
	// Only facts preceding the accepted publication may supply the source. A
	// later injected message cannot retroactively make a forged quote valid.
	current := RoomSnapshot{RoomID: p.key.RoomID, DeploymentID: p.key.Deployment}
	if err := tx.QueryRow(ctx, `SELECT host_mode,current_seq,revision,org_id FROM `+s.table("rooms")+` WHERE room_id=$1`, p.key.RoomID).Scan(&current.HostMode, &current.Seq, &current.Revision, &current.OrgID); err != nil {
		return err
	}
	snapshot, err := s.readRoomEvents(ctx, tx, current)
	if err != nil {
		return err
	}
	if fact.Seq > 0 {
		end := 0
		for _, event := range snapshot.Events {
			if event.Seq >= fact.Seq {
				break
			}
			end++
		}
		snapshot.Events = snapshot.Events[:end]
		snapshot.Seq = uint64(end)
	}
	source, err := quotedMessage(toRelaySnapshot(snapshot), p.human.QuoteID)
	if err != nil {
		return &storedFactCorruption{err: err}
	}
	ids := p.human.AttachmentIDs.Slice()
	if len(payload.Attachments) < len(ids) {
		return &storedFactCorruption{err: errors.New("explicit attachments missing from quoted result")}
	}
	if err := matchQuotedHuman(payload, source, payload.Attachments[:len(ids)]); err != nil {
		return &storedFactCorruption{err: err}
	}
	return nil
}

func (s *Store) writeHumanMessageReferences(ctx context.Context, tx pgx.Tx, p opPlan, stored model.Event, d dependencyState) error {
	payload, err := decodeHumanSendPayload(stored.Data)
	if err != nil {
		return err
	}
	if err := s.writeAttachmentReferences(ctx, tx, p.key.RoomID, payload.ID, stored.Seq, d.attachmentRefs); err != nil {
		return err
	}
	if p.human.QuoteID == "" {
		return nil
	}
	return s.copyQuotedReferences(ctx, tx, p.key.RoomID, p.human.QuoteID, payload.ID, stored.Seq, payload.Attachments)
}

// Quoted objects already have an immutable reference in this Room. They do
// not become drafts again, acquire a new expiry, or charge quota twice.
func (s *Store) copyQuotedReferences(ctx context.Context, tx pgx.Tx, room, sourceID, messageID string, seq uint64, merged []model.Attachment) error {
	for _, meta := range merged {
		var sourceRef, existing bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("attachment_references")+` WHERE room_id=$1 AND message_id=$2 AND object_id=$3),EXISTS(SELECT 1 FROM `+s.table("attachment_references")+` WHERE room_id=$1 AND message_id=$4 AND object_id=$3 AND first_seq=$5)`, room, sourceID, meta.ID, messageID, int64(seq)).Scan(&sourceRef, &existing); err != nil {
			return err
		}
		if existing {
			continue
		}
		a, err := s.readAttachmentState(ctx, tx, meta.ID, lockNone)
		if err != nil {
			return err
		}
		if !sourceRef || a.missing || a.roomID != room || a.state != "referenced" || !a.meta.EqualMetadata(meta) {
			return &storedFactCorruption{err: errors.New("quoted attachment lacks its original immutable Room reference")}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("attachment_references")+`(room_id,object_id,message_id,first_seq) VALUES($1,$2,$3,$4)`, room, meta.ID, messageID, int64(seq)); err != nil {
			return err
		}
	}
	return nil
}
