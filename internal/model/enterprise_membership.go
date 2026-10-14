package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	EnterpriseMembershipVersion = 1
	EnterpriseMembershipKind    = "room.membership.changed"
)

type EnterpriseMembershipAction string
type EnterpriseRoomRole string

const (
	EnterpriseMembershipAdmit      EnterpriseMembershipAction = "admit"
	EnterpriseMembershipChangeRole EnterpriseMembershipAction = "change_role"
	EnterpriseMembershipRemove     EnterpriseMembershipAction = "remove"
	EnterpriseRoomMember           EnterpriseRoomRole         = "member"
	EnterpriseRoomViewer           EnterpriseRoomRole         = "viewer"
)

// EnterpriseMembershipIntent is the complete frozen, human-controlled intent.
// Ownership transfer and administrator powers are deliberately not roles this
// command can grant. A removal has no role; no input is silently normalized.
type EnterpriseMembershipIntent struct {
	TargetUserID string                     `json:"target_user_id"`
	Action       EnterpriseMembershipAction `json:"action"`
	Role         EnterpriseRoomRole         `json:"role,omitempty"`
}

func ValidateEnterpriseMembershipIntent(intent EnterpriseMembershipIntent) error {
	if !validEnterpriseIdentity(intent.TargetUserID) || strings.ContainsAny(intent.TargetUserID, `/\`) {
		return errors.New("invalid enterprise membership target")
	}
	switch intent.Action {
	case EnterpriseMembershipAdmit, EnterpriseMembershipChangeRole:
		if intent.Role != EnterpriseRoomMember && intent.Role != EnterpriseRoomViewer {
			return errors.New("membership admission and role changes require member or viewer")
		}
	case EnterpriseMembershipRemove:
		if intent.Role != "" {
			return errors.New("membership removal cannot grant a role")
		}
	default:
		return errors.New("unsupported enterprise membership action")
	}
	return nil
}

// EnterpriseMembershipChange records the accepted action for historical
// display and receipt verification. Replaying it must never recreate ACLs:
// the current control rows remain authorization authority.
type EnterpriseMembershipChange struct {
	Version int `json:"version"`
	EnterpriseMembershipIntent
}

func NewEnterpriseMembershipChange(roomID string, seq uint64, intent EnterpriseMembershipIntent, at time.Time) (Event, error) {
	if err := ValidateEnterpriseMembershipIntent(intent); err != nil {
		return Event{}, err
	}
	fact, err := NewEvent(roomID, EnterpriseMembershipKind, ActorSystem, EnterpriseMembershipChange{Version: EnterpriseMembershipVersion, EnterpriseMembershipIntent: intent})
	if err != nil {
		return Event{}, err
	}
	fact.Seq, fact.CreatedAt = seq, at.UTC()
	if _, err := DecodeEnterpriseMembershipChange(fact); err != nil {
		return Event{}, err
	}
	return fact, nil
}

func DecodeEnterpriseMembershipChange(fact Event) (EnterpriseMembershipChange, error) {
	var change EnterpriseMembershipChange
	if fact.Kind != EnterpriseMembershipKind || fact.Actor != ActorSystem || fact.Seq < 2 || !validEnterpriseIdentity(fact.ID) || !validEnterpriseIdentity(fact.RoomID) || fact.CreatedAt.IsZero() || !utf8.Valid(fact.Data) {
		return change, errors.New("invalid enterprise membership event")
	}
	if err := rejectDuplicateEnterpriseJSONKeys(fact.Data); err != nil {
		return change, err
	}
	decoder := json.NewDecoder(bytes.NewReader(fact.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&change); err != nil {
		return change, fmt.Errorf("decode enterprise membership: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return change, errors.New("enterprise membership has trailing JSON")
	}
	if change.Version != EnterpriseMembershipVersion {
		return change, errors.New("unsupported enterprise membership version")
	}
	if err := ValidateEnterpriseMembershipIntent(change.EnterpriseMembershipIntent); err != nil {
		return change, err
	}
	return change, nil
}
