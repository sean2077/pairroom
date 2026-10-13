package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	EnterpriseRoomCreationVersion = 1
	EnterpriseRoomProtocolVersion = "pairroom-protocol/v9"
	EnterpriseRoomCreatedKind     = "room.created"
	MaxEnterpriseRoomNameBytes    = 160
)

// EnterpriseRoomIntent is the frozen user-controlled creation intent. It has
// no Room identity, clock, owner, deployment, Runtime or Binding selections.
type EnterpriseRoomIntent struct {
	Name          string        `json:"name"`
	Collaboration Collaboration `json:"collaboration"`
}

// PrepareEnterpriseRoomIntent normalizes the creation input once. Persisted
// or replayed intents must instead pass ValidateEnterpriseRoomIntent unchanged.
func PrepareEnterpriseRoomIntent(name string, collaboration Collaboration) (EnterpriseRoomIntent, error) {
	frozen, err := collaboration.ForCreation()
	if err != nil {
		return EnterpriseRoomIntent{}, err
	}
	intent := EnterpriseRoomIntent{Name: strings.TrimSpace(name), Collaboration: frozen}
	if err := ValidateEnterpriseRoomIntent(intent); err != nil {
		return EnterpriseRoomIntent{}, err
	}
	return intent, nil
}

func ValidateEnterpriseRoomIntent(intent EnterpriseRoomIntent) error {
	if intent.Name == "" || strings.TrimSpace(intent.Name) != intent.Name || len(intent.Name) > MaxEnterpriseRoomNameBytes || !utf8.ValidString(intent.Name) || strings.ContainsFunc(intent.Name, unicode.IsControl) {
		return errors.New("enterprise Room name must be frozen UTF-8 without controls and contain 1 to 160 bytes")
	}
	if err := intent.Collaboration.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(intent.Collaboration.Instructions) != intent.Collaboration.Instructions {
		return errors.New("enterprise Room collaboration must be frozen before persistence")
	}
	return nil
}

// EnterpriseInitialSlot records a vacant initial slot, not a Binding.
type EnterpriseInitialSlot struct {
	Slot       ActorID `json:"slot"`
	Generation uint64  `json:"generation"`
	State      string  `json:"state"`
}

type EnterpriseRoomOrigin struct {
	Version            int                     `json:"version"`
	DeploymentID       string                  `json:"deployment_id"`
	OrgID              string                  `json:"org_id"`
	HostMode           HostMode                `json:"host_mode"`
	ProtocolVersion    string                  `json:"protocol_version"`
	InitialOwnerUserID string                  `json:"initial_owner_user_id"`
	InitialSlots       []EnterpriseInitialSlot `json:"initial_slots"`
}

// EnterpriseRoomCreation preserves the established RoomMeta JSON fields and
// adds the immutable enterprise origin needed for strict replay.
type EnterpriseRoomCreation struct {
	RoomMeta
	Enterprise EnterpriseRoomOrigin `json:"enterprise"`
}

func NewEnterpriseRoomCreation(roomID, deploymentID, orgID, ownerID string, intent EnterpriseRoomIntent, createdAt time.Time) (Event, error) {
	if err := ValidateEnterpriseRoomIntent(intent); err != nil {
		return Event{}, err
	}
	collaboration := intent.Collaboration
	creation := EnterpriseRoomCreation{
		RoomMeta: RoomMeta{ID: roomID, Name: intent.Name, CreatedAt: createdAt.UTC(), Collaboration: &collaboration},
		Enterprise: EnterpriseRoomOrigin{
			Version: EnterpriseRoomCreationVersion, DeploymentID: deploymentID, OrgID: orgID,
			HostMode: HostNative, ProtocolVersion: EnterpriseRoomProtocolVersion, InitialOwnerUserID: ownerID,
			InitialSlots: []EnterpriseInitialSlot{{Slot: ActorSlot1, Generation: 1, State: "vacant"}, {Slot: ActorSlot2, Generation: 1, State: "vacant"}},
		},
	}
	event, err := NewEvent(roomID, EnterpriseRoomCreatedKind, ActorSystem, creation)
	if err != nil {
		return Event{}, err
	}
	event.Seq, event.CreatedAt = 1, creation.CreatedAt
	if _, err := DecodeEnterpriseRoomCreation(event); err != nil {
		return Event{}, err
	}
	return event, nil
}

// DecodeEnterpriseRoomCreation validates stored facts without normalizing or
// filling missing historical values from current defaults.
func DecodeEnterpriseRoomCreation(event Event) (EnterpriseRoomCreation, error) {
	var creation EnterpriseRoomCreation
	if event.Seq != 1 || event.Kind != EnterpriseRoomCreatedKind || event.Actor != ActorSystem || !validEnterpriseIdentity(event.ID) || !validEnterpriseIdentity(event.RoomID) || event.CreatedAt.IsZero() || !utf8.Valid(event.Data) {
		return creation, errors.New("invalid enterprise Room creation event")
	}
	if err := rejectDuplicateEnterpriseJSONKeys(event.Data); err != nil {
		return creation, err
	}
	decoder := json.NewDecoder(bytes.NewReader(event.Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&creation); err != nil {
		return creation, fmt.Errorf("decode enterprise Room creation: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return creation, errors.New("enterprise Room creation has trailing JSON")
	}
	origin := creation.Enterprise
	if creation.ID != event.RoomID || creation.Repo != "" || !creation.CreatedAt.Equal(event.CreatedAt) || creation.Collaboration == nil {
		return creation, errors.New("enterprise Room creation metadata disagrees with its event")
	}
	if err := ValidateEnterpriseRoomIntent(EnterpriseRoomIntent{Name: creation.Name, Collaboration: *creation.Collaboration}); err != nil {
		return creation, err
	}
	if origin.Version != EnterpriseRoomCreationVersion || origin.HostMode != HostNative || origin.ProtocolVersion != EnterpriseRoomProtocolVersion || !validEnterpriseIdentity(origin.DeploymentID) || !validEnterpriseIdentity(origin.OrgID) || !validEnterpriseIdentity(origin.InitialOwnerUserID) {
		return creation, errors.New("invalid enterprise Room origin")
	}
	if len(origin.InitialSlots) != 2 || origin.InitialSlots[0] != (EnterpriseInitialSlot{Slot: ActorSlot1, Generation: 1, State: "vacant"}) || origin.InitialSlots[1] != (EnterpriseInitialSlot{Slot: ActorSlot2, Generation: 1, State: "vacant"}) {
		return creation, errors.New("enterprise Room must originate with two generation-one vacant slots")
	}
	return creation, nil
}

func validEnterpriseIdentity(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

// The standard struct decoder rejects unknown fields but accepts duplicate
// keys. Reject them first so one immutable origin cannot have two meanings.
func rejectDuplicateEnterpriseJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				// All creation schema keys use lowercase ASCII. The standard
				// struct decoder also matches case-folded names, so refuse those
				// aliases instead of letting "name" and "Name" describe two values.
				if !ok || strings.ContainsFunc(name, func(r rune) bool { return r != '_' && (r < 'a' || r > 'z') }) || keys[name] {
					return errors.New("duplicate or invalid enterprise Room creation JSON key")
				}
				keys[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid enterprise Room creation JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	return value()
}
