package model

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ActorID string

const (
	ActorUser   ActorID = "user"
	ActorClaude ActorID = "claude"
	ActorCodex  ActorID = "codex"
	ActorSystem ActorID = "system"
)

func (a ActorID) ValidParticipant() bool { return a == ActorClaude || a == ActorCodex }

func (a ActorID) DisplayName() string {
	switch a {
	case ActorUser:
		return "You"
	case ActorClaude:
		return "Claude Code"
	case ActorCodex:
		return "Codex"
	case ActorSystem:
		return "PairRoom"
	default:
		return string(a)
	}
}

type ParticipantRole string

const (
	RoleDriver   ParticipantRole = "driver"
	RoleReviewer ParticipantRole = "reviewer"
	RolePeer     ParticipantRole = "peer"
)

func (r ParticipantRole) Valid() bool {
	return r == RoleDriver || r == RoleReviewer || r == RolePeer
}

type AgentState string

const (
	StateStopped  AgentState = "stopped"
	StateStarting AgentState = "starting"
	StateIdle     AgentState = "idle"
	StateWorking  AgentState = "working"
	StateWaiting  AgentState = "waiting"
	StateError    AgentState = "error"
)

type DeliveryState string

const (
	DeliveryPending    DeliveryState = "pending"
	DeliveryQueued     DeliveryState = "queued"
	DeliverySubmitting DeliveryState = "submitting"
	DeliveryStarted    DeliveryState = "started"
	DeliveryInjected   DeliveryState = "injected"
	DeliveryFailed     DeliveryState = "failed"
	DeliverySkipped    DeliveryState = "skipped"
)

func (s DeliveryState) Valid() bool {
	switch s {
	case DeliveryPending, DeliveryQueued, DeliverySubmitting, DeliveryStarted, DeliveryInjected, DeliveryFailed, DeliverySkipped:
		return true
	default:
		return false
	}
}

// ProcessingState describes what happened after a message entered a native
// harness. DeliveryState intentionally remains the transport-level result
// (started, injected, queued, ...); processing is the durable execution-level
// projection that lets the room distinguish "accepted" from "actually done".
type ProcessingState string

const (
	ProcessingWaiting   ProcessingState = "waiting"
	ProcessingWorking   ProcessingState = "working"
	ProcessingCompleted ProcessingState = "completed"
	ProcessingCancelled ProcessingState = "cancelled"
	ProcessingFailed    ProcessingState = "failed"
)

func (s ProcessingState) Valid() bool {
	switch s {
	case ProcessingWaiting, ProcessingWorking, ProcessingCompleted, ProcessingCancelled, ProcessingFailed:
		return true
	default:
		return false
	}
}

func (s ProcessingState) Terminal() bool {
	switch s {
	case ProcessingCompleted, ProcessingCancelled, ProcessingFailed:
		return true
	default:
		return false
	}
}

type MessageIntent string

const (
	IntentSteer MessageIntent = "steer"
	IntentQueue MessageIntent = "queue"
)

func (i MessageIntent) Valid() bool {
	return i == IntentSteer || i == IntentQueue
}

// Attachment is durable, presentation-safe metadata for an item attached to a
// room message. PairRoom accepts validated raster images only. Absolute host paths
// never enter the transcript or API response.
type Attachment struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Kind      string    `json:"kind"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	Width     int       `json:"width,omitempty"`
	Height    int       `json:"height,omitempty"`
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentAttachment adds the host-local path required at the native harness
// boundary. The path is explicitly excluded from JSON.
type AgentAttachment struct {
	Attachment
	Path string `json:"-"`
}

type Message struct {
	ID                      string                      `json:"id"`
	Seq                     uint64                      `json:"seq"`
	From                    ActorID                     `json:"from"`
	To                      []ActorID                   `json:"to,omitempty"`
	Text                    string                      `json:"text"`
	ReplyTo                 string                      `json:"reply_to,omitempty"`
	RetryOf                 string                      `json:"retry_of,omitempty"`
	Intent                  MessageIntent               `json:"intent,omitempty"`
	ThreadID                string                      `json:"thread_id"`
	TurnID                  string                      `json:"turn_id,omitempty"`
	CreatedAt               time.Time                   `json:"created_at"`
	Delivery                map[ActorID]DeliveryState   `json:"delivery,omitempty"`
	DeliveryDetail          map[ActorID]string          `json:"delivery_detail,omitempty"`
	Processing              map[ActorID]ProcessingState `json:"processing,omitempty"`
	ProcessingDetail        map[ActorID]string          `json:"processing_detail,omitempty"`
	ProcessingTurn          map[ActorID]string          `json:"processing_turn,omitempty"`
	ProcessingLastUpdatedAt map[ActorID]time.Time       `json:"processing_last_updated_at,omitempty"`
	Attachments             []Attachment                `json:"attachments,omitempty"`
}

type Event struct {
	Seq       uint64          `json:"seq"`
	ID        string          `json:"id"`
	RoomID    string          `json:"room_id"`
	Kind      string          `json:"kind"`
	Actor     ActorID         `json:"actor"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

func NewEvent(roomID, kind string, actor ActorID, payload any) (Event, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("marshal event payload: %w", err)
	}
	return Event{
		ID:        NewID("evt"),
		RoomID:    roomID,
		Kind:      kind,
		Actor:     actor,
		CreatedAt: time.Now().UTC(),
		Data:      data,
	}, nil
}

type DeliveryUpdate struct {
	MessageID string        `json:"message_id"`
	Target    ActorID       `json:"target"`
	State     DeliveryState `json:"state"`
	Detail    string        `json:"detail,omitempty"`
}

type ProcessingUpdate struct {
	MessageID string          `json:"message_id"`
	Target    ActorID         `json:"target"`
	State     ProcessingState `json:"state"`
	Detail    string          `json:"detail,omitempty"`
	TurnID    string          `json:"turn_id,omitempty"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type RuntimeInfo struct {
	Available      bool            `json:"available"`
	Command        string          `json:"command,omitempty"`
	Path           string          `json:"path,omitempty"`
	Protocol       string          `json:"protocol,omitempty"`
	Version        string          `json:"version,omitempty"`
	RuntimeKind    RuntimeKind     `json:"runtime_kind,omitempty"`
	Provider       string          `json:"provider,omitempty"`
	Model          string          `json:"model,omitempty"`
	Effort         string          `json:"effort,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	ApprovalPolicy string          `json:"approval_policy,omitempty"`
	Sandbox        string          `json:"sandbox,omitempty"`
	Capabilities   []string        `json:"capabilities,omitempty"`
	Warnings       []string        `json:"warnings,omitempty"`
	ProbedAt       time.Time       `json:"probed_at,omitempty"`
	Data           json.RawMessage `json:"data,omitempty"`
}

// WorkspaceBoundary describes the filesystem view assigned to a participant.
// The driver uses the live repository while the reviewer can be placed in an
// independently materialized Git snapshot.  The metadata is deliberately
// durable and visible so the UI never implies stronger isolation than the
// runtime actually provides.
type WorkspaceBoundary struct {
	Kind             string    `json:"kind"`
	Path             string    `json:"path,omitempty"`
	SourceHead       string    `json:"source_head,omitempty"`
	PatchSHA256      string    `json:"patch_sha256,omitempty"`
	Dirty            bool      `json:"dirty"`
	UntrackedCount   int       `json:"untracked_count,omitempty"`
	ReadOnly         bool      `json:"read_only"`
	ReadOnlyEnforced bool      `json:"read_only_enforced"`
	RefreshedAt      time.Time `json:"refreshed_at,omitempty"`
	Warnings         []string  `json:"warnings,omitempty"`
}

type ParticipantSnapshot struct {
	ID            ActorID           `json:"id"`
	DisplayName   string            `json:"display_name"`
	MentionHandle string            `json:"mention_handle"`
	Role          ParticipantRole   `json:"role"`
	State         AgentState        `json:"state"`
	SessionID     string            `json:"session_id,omitempty"`
	Model         string            `json:"model,omitempty"`
	RuntimeKind   RuntimeKind       `json:"runtime_kind,omitempty"`
	CurrentTurn   string            `json:"current_turn,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
	LastActivity  time.Time         `json:"last_activity,omitempty"`
	Runtime       RuntimeInfo       `json:"runtime,omitempty"`
	Workspace     WorkspaceBoundary `json:"workspace,omitempty"`
}

type RoomSettings struct {
	StallWarningSeconds int `json:"stall_warning_seconds"`
}

func DefaultRoomSettings() RoomSettings {
	return RoomSettings{StallWarningSeconds: 300}
}

type RoomMeta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Repo      string    `json:"repo"`
	CreatedAt time.Time `json:"created_at"`
}

type Approval struct {
	ID          string          `json:"id"`
	Agent       ActorID         `json:"agent"`
	Kind        string          `json:"kind"`
	Title       string          `json:"title"`
	Detail      json.RawMessage `json:"detail,omitempty"`
	Status      string          `json:"status"`
	Decision    string          `json:"decision,omitempty"`
	RequestedAt time.Time       `json:"requested_at"`
	ResolvedAt  *time.Time      `json:"resolved_at,omitempty"`
}

// ApprovalResolution is the user response sent back to a native harness.
// Most approvals need only Decision. Interactive questions may additionally
// carry Answers keyed by the exact question text emitted by the harness.
type ApprovalResolution struct {
	Decision string            `json:"decision"`
	Message  string            `json:"message,omitempty"`
	Answers  map[string]string `json:"answers,omitempty"`
}

type RuntimeEvent struct {
	Agent         ActorID         `json:"agent"`
	Kind          string          `json:"kind"`
	TurnID        string          `json:"turn_id,omitempty"`
	ItemID        string          `json:"item_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	SessionID     string          `json:"session_id,omitempty"`
	Text          string          `json:"text,omitempty"`
	Name          string          `json:"name,omitempty"`
	State         AgentState      `json:"state,omitempty"`
	Approval      *Approval       `json:"approval,omitempty"`
	Runtime       *RuntimeInfo    `json:"runtime,omitempty"`
	Data          json.RawMessage `json:"data,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

type TurnWorkItem struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	Name        string          `json:"name,omitempty"`
	Status      string          `json:"status"`
	Detail      string          `json:"detail,omitempty"`
	Data        json.RawMessage `json:"data,omitempty"`
	StartedAt   time.Time       `json:"started_at,omitempty"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
}

// TurnSummary is the durable, vendor-neutral projection shown by the Work
// Inspector. Raw vendor events remain available in the event log; this compact
// shape is deliberately bounded so long sessions stay useful after restart.
type TurnSummary struct {
	ID             string          `json:"id"`
	Agent          ActorID         `json:"agent"`
	TurnID         string          `json:"turn_id"`
	SessionID      string          `json:"session_id,omitempty"`
	MessageIDs     []string        `json:"message_ids,omitempty"`
	Status         string          `json:"status"`
	StartedAt      time.Time       `json:"started_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
	DurationMillis int64           `json:"duration_millis,omitempty"`
	Items          []TurnWorkItem  `json:"items,omitempty"`
	Plan           string          `json:"plan,omitempty"`
	Diff           string          `json:"diff,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	FinalText      string          `json:"final_text,omitempty"`
	Error          string          `json:"error,omitempty"`
}

const (
	RuntimeSession           = "session"
	RuntimeInfoUpdated       = "runtime.info"
	RuntimeState             = "state"
	RuntimeInputProcessing   = "input.processing"
	RuntimeInputCompleted    = "input.completed"
	RuntimeInputCancelled    = "input.cancelled"
	RuntimeInputFailed       = "input.failed"
	RuntimeTurnStarted       = "turn.started"
	RuntimeTurnCompleted     = "turn.completed"
	RuntimeTextDelta         = "text.delta"
	RuntimeToolStarted       = "tool.started"
	RuntimeToolCompleted     = "tool.completed"
	RuntimeCommandOutput     = "command.output"
	RuntimePlanUpdated       = "plan.updated"
	RuntimeDiffUpdated       = "diff.updated"
	RuntimeUsageUpdated      = "usage.updated"
	RuntimeApprovalRequested = "approval.requested"
	RuntimeApprovalResolved  = "approval.resolved"
	RuntimeFinal             = "final"
	RuntimeLog               = "log"
	RuntimeError             = "error"
)

type AgentInput struct {
	MessageID   string            `json:"message_id"`
	ThreadID    string            `json:"thread_id"`
	From        ActorID           `json:"from"`
	To          ActorID           `json:"to"`
	FromHandle  string            `json:"from_handle"`
	SelfHandle  string            `json:"self_handle"`
	PeerHandle  string            `json:"peer_handle"`
	Text        string            `json:"text"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Role        ParticipantRole   `json:"role"`
	Attachments []AgentAttachment `json:"attachments,omitempty"`
	Intent      MessageIntent     `json:"intent,omitempty"`
}

type SystemNotice struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

type MessageWindow struct {
	Total     int    `json:"total"`
	Loaded    int    `json:"loaded"`
	HasMore   bool   `json:"has_more"`
	OldestSeq uint64 `json:"oldest_seq,omitempty"`
}

type MessagePage struct {
	Messages  []Message `json:"messages"`
	Total     int       `json:"total"`
	HasMore   bool      `json:"has_more"`
	OldestSeq uint64    `json:"oldest_seq,omitempty"`
}

type RoomSnapshot struct {
	Meta          RoomMeta                        `json:"meta"`
	Settings      RoomSettings                    `json:"settings"`
	Participants  map[ActorID]ParticipantSnapshot `json:"participants"`
	Messages      []Message                       `json:"messages"`
	Approvals     []Approval                      `json:"approvals"`
	Turns         []TurnSummary                   `json:"turns,omitempty"`
	MessageWindow *MessageWindow                  `json:"message_window,omitempty"`
	LatestSeq     uint64                          `json:"latest_seq"`
	// Events is a bounded recent tail used by the work inspector. The complete
	// append-only history remains in events.jsonl.
	Events []Event `json:"events,omitempty"`
}

func NewID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(b[:])
}

func NormalizeActors(values []ActorID) []ActorID {
	seen := make(map[ActorID]struct{}, len(values))
	out := make([]ActorID, 0, len(values))
	for _, value := range values {
		value = ActorID(strings.ToLower(strings.TrimSpace(string(value))))
		if !value.ValidParticipant() {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func OtherParticipant(actor ActorID) ActorID {
	switch actor {
	case ActorClaude:
		return ActorCodex
	case ActorCodex:
		return ActorClaude
	default:
		return ""
	}
}
