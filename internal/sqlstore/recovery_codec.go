package sqlstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
)

type factOrigin struct {
	kind   string
	userID string
}

func userFactOrigin(id string) factOrigin { return factOrigin{kind: "user", userID: id} }
func operatorFactOrigin() factOrigin      { return factOrigin{kind: "operator"} }

type storedEventRecord struct {
	CodecVersion int         `json:"codec_version"`
	OriginKind   string      `json:"origin_kind"`
	OriginUser   *string     `json:"origin_user"`
	Event        model.Event `json:"event"`
}

func encodeStoredFact(event model.Event, origin factOrigin) ([]byte, error) {
	if origin.kind != "user" && origin.kind != "operator" || origin.kind == "user" && !validRoomCreateIdentity(origin.userID) || origin.kind == "operator" && (origin.userID != "" || event.Actor != model.ActorSystem) {
		return nil, errors.New("invalid stored event provenance")
	}
	record := storedEventRecord{CodecVersion: factCodecVersion, OriginKind: origin.kind, Event: event}
	if origin.kind == "user" {
		record.OriginUser = &origin.userID
	}
	return json.Marshal(record)
}
func (s *Store) writeStoredRoomEvent(ctx context.Context, tx pgx.Tx, event model.Event, origin factOrigin) error {
	encoding, e := encodeStoredFact(event, origin)
	if e != nil {
		return e
	}
	var user any
	if origin.kind == "user" {
		user = origin.userID
	}
	_, e = tx.Exec(ctx, `INSERT INTO `+s.table("room_events")+`(room_id,seq,event_id,fact_version,event_encoding,kind,actor,created_at,origin_kind,origin_user) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, event.RoomID, int64(event.Seq), event.ID, factCodecVersion, encoding, event.Kind, string(event.Actor), event.CreatedAt, origin.kind, user)
	return e
}

// SQL's private wrapper is versioned separately from model.Event and the
// Local store. Its provenance is authoritative and must match both indexes;
// no missing origin or older same-schema encoding is guessed during recovery.
func decodeStoredFact(roomID string, seq uint64, eventID string, factVersion int, kind, actor string, encoding []byte, originKind string, originUser *string) (model.Event, error) {
	if factVersion != factCodecVersion {
		return model.Event{}, errors.New("stored event codec version is unsupported")
	}
	var record storedEventRecord
	decoder := json.NewDecoder(bytes.NewReader(encoding))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF {
		return model.Event{}, errors.New("stored event wrapper is invalid")
	}
	canonical, e := json.Marshal(record)
	if e != nil || !bytes.Equal(canonical, encoding) {
		return model.Event{}, errors.New("stored event wrapper is noncanonical")
	}
	user := ""
	if record.OriginUser != nil {
		user = *record.OriginUser
	}
	if record.CodecVersion != factCodecVersion || record.OriginKind != originKind || (record.OriginUser == nil) != (originUser == nil) || originUser != nil && user != *originUser {
		return model.Event{}, errors.New("stored event provenance disagrees with its indexes")
	}
	if _, e = encodeStoredFact(record.Event, factOrigin{kind: record.OriginKind, userID: user}); e != nil {
		return model.Event{}, e
	}
	event := record.Event
	if event.ID != eventID || event.Seq != seq || event.RoomID != roomID || event.Kind != kind || string(event.Actor) != actor {
		return model.Event{}, errors.New("stored event indexes disagree with its authoritative encoding")
	}
	return event, nil
}
