package service

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	EventRoomProvisioned         = "service.room.provisioned"
	EventRoomRenamed             = "service.room.renamed"
	EventRoomArchived            = "service.room.archived"
	EventRoomRestored            = "service.room.restored"
	EventRoomBindingMaterialized = "service.room.binding.materialized"
)

const TranscriptBoundaryNotice = "This PairRoom timeline starts at the binding boundary. Existing native Runtime context may be resumed, but earlier native conversation history is not imported, copied, summarized, searched, or displayed."

type BindingMode string

const (
	BindingNew      BindingMode = "new"
	BindingExisting BindingMode = "existing"
)

func (m BindingMode) Valid() bool { return m == BindingNew || m == BindingExisting }

type RoomLifecycle string

const (
	RoomActive   RoomLifecycle = "active"
	RoomArchived RoomLifecycle = "archived"
)

func (s RoomLifecycle) Valid() bool { return s == RoomActive || s == RoomArchived }

type BindingSpec struct {
	Mode      BindingMode `json:"mode"`
	SessionID string      `json:"session_id,omitempty"`
}

func (s BindingSpec) Validate() error {
	if !s.Mode.Valid() {
		return fmt.Errorf("invalid binding mode %q", s.Mode)
	}
	sessionID := strings.TrimSpace(s.SessionID)
	switch s.Mode {
	case BindingNew:
		if sessionID != "" {
			return errors.New("new binding must not include a session ID")
		}
	case BindingExisting:
		if sessionID == "" {
			return errors.New("existing binding requires a session ID")
		}
		if len(sessionID) > 512 {
			return errors.New("session ID exceeds 512 bytes")
		}
		if strings.ContainsAny(sessionID, "\r\n\x00") {
			return errors.New("session ID contains control characters")
		}
	}
	return nil
}

type Binding struct {
	Agent     model.ActorID `json:"agent"`
	Mode      BindingMode   `json:"mode"`
	SessionID string        `json:"session_id,omitempty"`
	Pending   bool          `json:"pending,omitempty"`
	BoundAt   time.Time     `json:"bound_at"`
}

func (b Binding) Key() BindingKey {
	return BindingKey{Agent: b.Agent, SessionID: b.SessionID}
}

func (b Binding) OwnsIdentity() bool {
	return !b.Pending && strings.TrimSpace(b.SessionID) != ""
}

func (b Binding) Validate() error {
	if !b.Agent.ValidParticipant() {
		return fmt.Errorf("invalid binding agent %q", b.Agent)
	}
	if !b.Mode.Valid() {
		return fmt.Errorf("invalid binding mode %q", b.Mode)
	}
	if b.Pending {
		if b.Mode != BindingNew {
			return errors.New("only new bindings may be pending")
		}
		if strings.TrimSpace(b.SessionID) != "" {
			return errors.New("pending binding must not include a session ID")
		}
		return nil
	}
	trimmed := strings.TrimSpace(b.SessionID)
	if trimmed == "" {
		return errors.New("binding session ID is required")
	}
	if b.SessionID != trimmed {
		return errors.New("binding session ID must not contain leading or trailing whitespace")
	}
	if len(b.SessionID) > 512 {
		return errors.New("binding session ID exceeds 512 bytes")
	}
	if strings.ContainsAny(b.SessionID, "\r\n\x00") {
		return errors.New("session ID contains control characters")
	}
	if b.BoundAt.IsZero() {
		return errors.New("binding time is required")
	}
	return nil
}

type BindingKey struct {
	Agent     model.ActorID `json:"agent"`
	SessionID string        `json:"session_id"`
}

func (k BindingKey) String() string {
	return string(k.Agent) + "\x00" + k.SessionID
}

type Project struct {
	ID         string    `json:"id"`
	Root       string    `json:"root"`
	Available  bool      `json:"available"`
	Diagnostic string    `json:"diagnostic,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type Room struct {
	RuntimeNames             map[model.ActorID]string               `json:"runtime_names,omitempty"`
	Collaboration            *model.Collaboration                   `json:"collaboration,omitempty"`
	ID                       string                                 `json:"id"`
	ProjectID                string                                 `json:"project_id"`
	Name                     string                                 `json:"name"`
	DataDir                  string                                 `json:"data_dir"`
	Lifecycle                RoomLifecycle                          `json:"lifecycle"`
	Bindings                 map[model.ActorID]Binding              `json:"bindings"`
	Agents                   map[model.ActorID]model.AgentSelection `json:"agents,omitempty"`
	TranscriptBoundaryNotice string                                 `json:"transcript_boundary_notice"`
	CreatedAt                time.Time                              `json:"created_at"`
	UpdatedAt                time.Time                              `json:"updated_at"`
}

func (r Room) Archived() bool { return r.Lifecycle == RoomArchived }

func (r Room) HasPendingBindings() bool {
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		if binding, ok := r.Bindings[actor]; !ok || binding.Pending {
			return true
		}
	}
	return false
}

// HasBlockingPendingBindings rejects incomplete selections without blocking
// new native bindings, whose identities materialize after the first input.
func (r Room) HasBlockingPendingBindings() bool {
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		binding, ok := r.Bindings[actor]
		if !ok || (binding.Pending && binding.Mode != BindingNew) {
			return true
		}
	}
	return false
}

func (r Room) Validate() error {
	if r.Collaboration == nil {
		return errors.New("Room collaboration instructions are required")
	}
	if err := r.Collaboration.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("room ID is required")
	}
	if r.ID == "." || r.ID == ".." || len(r.ID) > 256 || strings.ContainsAny(r.ID, "/\\\r\n\x00") {
		return errors.New("room ID is not a safe durable identifier")
	}
	if strings.TrimSpace(r.ProjectID) == "" {
		return errors.New("project ID is required")
	}
	if err := validateRoomName(r.Name); err != nil {
		return err
	}
	if !r.Lifecycle.Valid() {
		return fmt.Errorf("invalid room lifecycle %q", r.Lifecycle)
	}
	if r.TranscriptBoundaryNotice != TranscriptBoundaryNotice {
		return errors.New("room transcript boundary policy is missing or invalid")
	}
	if len(r.Bindings) != 2 {
		return fmt.Errorf("room must contain exactly two agent bindings; got %d", len(r.Bindings))
	}
	for actor := range r.Bindings {
		if actor != model.ActorClaude && actor != model.ActorCodex {
			return fmt.Errorf("room contains unexpected binding agent %q", actor)
		}
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		binding, ok := r.Bindings[actor]
		if !ok {
			return fmt.Errorf("room is missing %s binding", actor)
		}
		if binding.Agent != actor {
			return fmt.Errorf("%s binding identifies agent %s", actor, binding.Agent)
		}
		if err := binding.Validate(); err != nil {
			return fmt.Errorf("%s binding: %w", actor, err)
		}
	}
	if _, err := validateAgentSelections(r.Agents); err != nil {
		return fmt.Errorf("Room Agent selections: %w", err)
	}
	return nil
}

type ProvisionRequest struct {
	AgentPairProfileID string                                 `json:"agent_pair_profile_id,omitempty"`
	Collaboration      *model.Collaboration                   `json:"collaboration,omitempty"`
	ProjectID          string                                 `json:"project_id"`
	Name               string                                 `json:"name"`
	Bindings           map[model.ActorID]BindingSpec          `json:"bindings"`
	Agents             map[model.ActorID]model.AgentSelection `json:"agents,omitempty"`
}

func (r ProvisionRequest) Validate() error {
	if r.Collaboration != nil {
		if _, err := r.Collaboration.ForCreation(); err != nil {
			return err
		}
	}
	if strings.TrimSpace(r.ProjectID) == "" {
		return errors.New("project_id is required")
	}
	if err := validateSubmittedRoomName(r.Name, true); err != nil {
		return err
	}
	if len(r.Bindings) != 2 {
		return fmt.Errorf("exactly two agent bindings are required; got %d", len(r.Bindings))
	}
	for actor := range r.Bindings {
		if actor != model.ActorClaude && actor != model.ActorCodex {
			return fmt.Errorf("unexpected binding agent %q", actor)
		}
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		spec, ok := r.Bindings[actor]
		if !ok {
			return fmt.Errorf("%s binding is required", actor)
		}
		if err := spec.Validate(); err != nil {
			return fmt.Errorf("%s binding: %w", actor, err)
		}
	}
	if r.Agents != nil {
		if _, err := validateAgentSelections(r.Agents); err != nil {
			return err
		}
	}
	return nil
}

type roomProvisionedPayload struct {
	Collaboration            *model.Collaboration                   `json:"collaboration,omitempty"`
	Schema                   int                                    `json:"schema"`
	Project                  Project                                `json:"project"`
	RoomID                   string                                 `json:"room_id"`
	Name                     string                                 `json:"name"`
	Lifecycle                RoomLifecycle                          `json:"lifecycle"`
	Bindings                 map[model.ActorID]Binding              `json:"bindings"`
	Agents                   map[model.ActorID]model.AgentSelection `json:"agents,omitempty"`
	TranscriptBoundaryNotice string                                 `json:"transcript_boundary_notice"`
	CreatedAt                time.Time                              `json:"created_at"`
}

type roomRenamedPayload struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

type roomLifecyclePayload struct {
	Lifecycle RoomLifecycle `json:"lifecycle"`
	UpdatedAt time.Time     `json:"updated_at"`
}

type roomBindingMaterializedPayload struct {
	Binding   Binding   `json:"binding"`
	UpdatedAt time.Time `json:"updated_at"`
}

type RegistrySnapshot struct {
	Schema      int       `json:"schema"`
	GeneratedAt time.Time `json:"generated_at"`
	Projects    []Project `json:"projects"`
	Rooms       []Room    `json:"rooms"`
}

func (s RegistrySnapshot) Sorted() RegistrySnapshot {
	out := s
	out.Projects = append([]Project(nil), s.Projects...)
	out.Rooms = append([]Room(nil), s.Rooms...)
	sort.Slice(out.Projects, func(i, j int) bool {
		if out.Projects[i].CreatedAt.Equal(out.Projects[j].CreatedAt) {
			return out.Projects[i].ID < out.Projects[j].ID
		}
		return out.Projects[i].CreatedAt.Before(out.Projects[j].CreatedAt)
	})
	sort.Slice(out.Rooms, func(i, j int) bool {
		if out.Rooms[i].CreatedAt.Equal(out.Rooms[j].CreatedAt) {
			return out.Rooms[i].ID < out.Rooms[j].ID
		}
		return out.Rooms[i].CreatedAt.Before(out.Rooms[j].CreatedAt)
	})
	return out
}

func validateRoomName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("room name is required")
	}
	if len(name) > 160 {
		return errors.New("room name exceeds 160 bytes")
	}
	if strings.ContainsAny(name, "\r\n\x00") {
		return errors.New("room name contains control characters")
	}
	return nil
}

var ErrInvalidRoomName = errors.New("invalid room name")

// Stronger validation applies to new submissions, not historical Room facts.
func validateSubmittedRoomName(name string, optional bool) error {
	if !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("%w: must be valid UTF-8 without control characters", ErrInvalidRoomName)
	}
	if optional && strings.TrimSpace(name) == "" {
		return nil
	}
	if err := validateRoomName(name); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRoomName, err)
	}
	return nil
}
