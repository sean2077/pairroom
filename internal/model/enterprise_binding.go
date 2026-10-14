package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	EnterpriseBindingVersion     = 1
	EnterpriseBindingUpdatedKind = "native.enterprise.binding.updated"
	// Three 256-byte IDs and one retirement reason can each expand sixfold
	// under JSON escaping. Leave room for their fixed field metadata as well.
	maxEnterpriseBindingEncodingBytes = 8 << 10
)

// EnterpriseBinding is a durable, credential-free Binding identity. Its
// presence records an accepted transition, not a grant to use that identity.
// Live User, Device and grant authorization belongs to the SQL transaction.
type EnterpriseBinding struct {
	Version    int         `json:"version"`
	RoomID     string      `json:"room_id"`
	Slot       ActorID     `json:"slot"`
	BindID     string      `json:"bind_id"`
	Generation uint64      `json:"generation"`
	AgentID    string      `json:"agent_id"`
	Runtime    RuntimeKind `json:"runtime"`
	Active     bool        `json:"active"`
	// ParkEnabled freezes the endpoint's accepted park policy. It grants no
	// delivery, wake or remote process-control authority.
	ParkEnabled   bool       `json:"park_enabled"`
	RetiredAt     *time.Time `json:"retired_at,omitempty"`
	RetiredReason string     `json:"retired_reason,omitempty"`
}

// NewEnterpriseBindingUpdate constructs a candidate without allocating its
// Room sequence. The transaction assigns the final sequence on acceptance.
func NewEnterpriseBindingUpdate(binding EnterpriseBinding, at time.Time) (Event, error) {
	if err := validateEnterpriseBinding(binding, at); err != nil {
		return Event{}, err
	}
	event, err := NewEvent(binding.RoomID, EnterpriseBindingUpdatedKind, binding.Slot, binding)
	if err != nil {
		return Event{}, err
	}
	event.CreatedAt = at.UTC()
	return event, nil
}

// DecodeEnterpriseBindingUpdate rejects unknown, duplicate and case-aliased
// JSON fields. In particular no local session, credential or LAN key can be
// smuggled into the enterprise identity codec.
func DecodeEnterpriseBindingUpdate(event Event) (EnterpriseBinding, error) {
	var binding EnterpriseBinding
	if event.Kind != EnterpriseBindingUpdatedKind || !validEnterpriseBindingID(event.ID) || !validEnterpriseBindingID(event.RoomID) || event.CreatedAt.IsZero() || len(event.Data) > maxEnterpriseBindingEncodingBytes || !utf8.Valid(event.Data) {
		return binding, errors.New("invalid enterprise Binding event")
	}
	if err := rejectDuplicateEnterpriseJSONKeys(event.Data); err != nil {
		return binding, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(event.Data, &fields); err != nil {
		return binding, err
	}
	for _, field := range []string{"active", "park_enabled"} {
		value := string(bytes.TrimSpace(fields[field]))
		if value != "true" && value != "false" {
			return binding, fmt.Errorf("enterprise Binding requires an explicit %s boolean", field)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(event.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return binding, fmt.Errorf("decode enterprise Binding: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return binding, errors.New("enterprise Binding has trailing JSON")
	}
	if binding.RoomID != event.RoomID || event.Actor != binding.Slot {
		return binding, errors.New("enterprise Binding identity disagrees with its event")
	}
	if err := validateEnterpriseBinding(binding, event.CreatedAt); err != nil {
		return binding, err
	}
	return binding, nil
}

func validEnterpriseBindingID(value string) bool {
	return validEnterpriseIdentity(value) && !strings.ContainsAny(value, "/\\")
}

func validateEnterpriseBinding(binding EnterpriseBinding, at time.Time) error {
	if binding.Version != EnterpriseBindingVersion || !validEnterpriseBindingID(binding.RoomID) || !binding.Slot.ValidParticipant() || !validEnterpriseBindingID(binding.BindID) || !validEnterpriseBindingID(binding.AgentID) || binding.Generation == 0 || binding.Generation > math.MaxInt64 || !binding.Runtime.Valid() || binding.Runtime != binding.Runtime.Canonical() || at.IsZero() {
		return errors.New("invalid enterprise Binding identity")
	}
	if binding.Active {
		if binding.RetiredAt != nil || binding.RetiredReason != "" {
			return errors.New("active enterprise Binding cannot carry retirement metadata")
		}
		return nil
	}
	if binding.RetiredAt == nil || binding.RetiredAt.IsZero() || !binding.RetiredAt.Equal(at) || strings.TrimSpace(binding.RetiredReason) == "" || len(binding.RetiredReason) > 256 || !utf8.ValidString(binding.RetiredReason) || strings.ContainsFunc(binding.RetiredReason, unicode.IsControl) {
		return errors.New("retired enterprise Binding requires its exact transition time and a bounded reason")
	}
	return nil
}
