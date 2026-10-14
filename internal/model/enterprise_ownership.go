package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode/utf8"
)

const EnterpriseOwnershipKind = "room.ownership.transferred"

// EnterpriseOwnershipChange describes history. Only live SQL control rows
// confer ownership; replaying this event must never restore an ACL or proof.
type EnterpriseOwnershipChange struct {
	Version    int    `json:"version"`
	FromUserID string `json:"from_user_id"`
	ToUserID   string `json:"to_user_id"`
	Governance bool   `json:"governance"`
}

func NewEnterpriseOwnershipChange(roomID string, seq uint64, change EnterpriseOwnershipChange, at time.Time) (Event, error) {
	fact, err := NewEvent(roomID, EnterpriseOwnershipKind, ActorSystem, change)
	if err != nil {
		return Event{}, err
	}
	fact.Seq, fact.CreatedAt = seq, at.UTC()
	if _, err := DecodeEnterpriseOwnershipChange(fact); err != nil {
		return Event{}, err
	}
	return fact, nil
}

func DecodeEnterpriseOwnershipChange(fact Event) (EnterpriseOwnershipChange, error) {
	var change EnterpriseOwnershipChange
	if fact.Kind != EnterpriseOwnershipKind || fact.Actor != ActorSystem || fact.Seq < 2 || !validEnterpriseIdentity(fact.ID) || !validEnterpriseIdentity(fact.RoomID) || fact.CreatedAt.IsZero() || !utf8.Valid(fact.Data) {
		return change, errors.New("invalid enterprise ownership event")
	}
	if err := rejectDuplicateEnterpriseJSONKeys(fact.Data); err != nil {
		return change, err
	}
	decoder := json.NewDecoder(bytes.NewReader(fact.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		return change, err
	}
	if decoder.Decode(new(any)) != io.EOF || change.Version != 1 || !validEnterpriseIdentity(change.FromUserID) || !validEnterpriseIdentity(change.ToUserID) || change.FromUserID == change.ToUserID {
		return change, errors.New("invalid enterprise ownership change")
	}
	// Governance is required even when false; omitted policy cannot silently
	// become a normal Owner transfer while replaying a future producer.
	var required map[string]json.RawMessage
	if json.Unmarshal(fact.Data, &required) != nil || required["governance"] == nil || bytes.Equal(required["governance"], []byte("null")) {
		return change, errors.New("enterprise ownership policy is required")
	}
	return change, nil
}
