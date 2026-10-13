package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const EnterpriseRoomMetadataKind = "room.metadata.changed"

// EnterpriseRoomMetadataChange records an explicit metadata change. These
// facts confer no Project membership and never retire or stop a native Agent.
type EnterpriseRoomMetadataChange struct {
	Version   int    `json:"version"`
	Action    string `json:"action"`
	Name      string `json:"name,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

func NewEnterpriseRoomMetadata(roomID string, seq uint64, change EnterpriseRoomMetadataChange, at time.Time) (Event, error) {
	fact, err := NewEvent(roomID, EnterpriseRoomMetadataKind, ActorSystem, change)
	if err != nil {
		return Event{}, err
	}
	fact.Seq, fact.CreatedAt = seq, at.UTC()
	if _, err = DecodeEnterpriseRoomMetadata(fact); err != nil {
		return Event{}, err
	}
	return fact, nil
}

func DecodeEnterpriseRoomMetadata(fact Event) (EnterpriseRoomMetadataChange, error) {
	var change EnterpriseRoomMetadataChange
	if fact.Kind != EnterpriseRoomMetadataKind || fact.Actor != ActorSystem || fact.Seq < 2 || !validEnterpriseIdentity(fact.ID) || !validEnterpriseIdentity(fact.RoomID) || fact.CreatedAt.IsZero() || !utf8.Valid(fact.Data) {
		return change, errors.New("invalid enterprise Room metadata event")
	}
	if err := rejectDuplicateEnterpriseJSONKeys(fact.Data); err != nil {
		return change, err
	}
	decoder := json.NewDecoder(bytes.NewReader(fact.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		return change, err
	}
	if decoder.Decode(new(any)) != io.EOF || change.Version != 1 {
		return change, errors.New("invalid enterprise Room metadata version")
	}
	valid := false
	switch change.Action {
	case "rename":
		valid = change.ProjectID == "" && change.Name != "" && len(change.Name) <= MaxEnterpriseRoomNameBytes && strings.TrimSpace(change.Name) == change.Name && !strings.ContainsFunc(change.Name, unicode.IsControl)
	case "archive", "restore":
		valid = change.Name == "" && change.ProjectID == ""
	case "project":
		valid = change.Name == "" && (change.ProjectID == "" || validEnterpriseIdentity(change.ProjectID) && !strings.ContainsAny(change.ProjectID, `/\`))
	}
	if !valid {
		return change, errors.New("invalid enterprise Room metadata action")
	}
	// Canonical encoding rejects irrelevant null/empty members instead of
	// allowing one stored intent to carry two interpretations.
	canonical, err := json.Marshal(change)
	if err != nil || !bytes.Equal(canonical, fact.Data) {
		return change, errors.New("noncanonical enterprise Room metadata")
	}
	return change, nil
}
