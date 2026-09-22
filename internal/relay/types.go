// Package relay implements Native-hosted Rooms without spawning a vendor process.
// The Room Event Log is the sole source of binding, publication and inbox facts.
package relay

import (
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/review"
)

const (
	MaxBodyBytes        = 256 << 10
	DefaultPark         = 30 * time.Second
	MaxPark             = 30 * time.Second
	DeliveryLease       = 10 * time.Second
	MaxBlocks           = 8
	EventBinding        = "native.binding.updated"
	EventPublication    = "native.publication"
	EventPublicationGap = "native.publication.gap"
	EventMessage        = "native.message.updated"
	EventFailure        = "native.failure"
	EventWakeConfig     = "native.wake.updated"
	EventWakeReserved   = "native.wake.reserved"
	EventWakeAttempted  = "native.wake.attempted"
)

var (
	ErrAuth         = errors.New("relay authentication failed: binding, generation and associated session must match")
	ErrOccupied     = errors.New("slot is occupied; run bind in the original session or explicitly --replace (cannot stop native work)")
	ErrClosed       = errors.New("native relay is closed or draining")
	ErrUnknown      = errors.New("publication result unknown; inspect status before explicitly deciding recovery")
	ErrWakeReserved = errors.New("wake is already reserved for this message; no automatic retry")
	ErrWakeRoomBusy = errors.New("resolve delivering or unknown deliveries before changing wake configuration")
)

// Wake outcomes and reasons are a fixed redaction vocabulary. They never carry
// vendor thread identity, message bodies, or command output; RecordWake
// rejects anything outside these sets so a caller cannot leak them into the
// durable Event Log.
var wakeOutcomes = map[string]bool{"accepted": true, "submitted": true, "failed": true, "suppressed": true}

var wakeReasons = map[string]bool{
	"disabled":               true,
	"burst":                  true,
	"unsupported_runtime":    true,
	"unbound":                true,
	"duplicate":              true,
	"minimum_interval":       true,
	"hourly_limit":           true,
	"invalid_message":        true,
	"waiter_active":          true,
	"collected":              true,
	"audit_unavailable":      true,
	"command_unavailable":    true,
	"command_timeout":        true,
	"command_cancelled":      true,
	"command_failed":         true,
	"capability_unavailable": true,
	"socket_failed":          true,
	"socket_timeout":         true,
	"socket_cancelled":       true,
}

// WakeReservation is the durable pre-command fact for one wake attempt,
// keyed by PairRoom transport message ID. It survives Service restart so an
// interrupted wake is never automatically retried.
type WakeReservation struct {
	MessageID string        `json:"message_id"`
	Target    model.ActorID `json:"target"`
	At        time.Time     `json:"at"`
}

// WakeCandidate is the atomic point-in-time answer to "should a durable
// queued message consider waking its target". QueueStart means the message
// began a fresh pending burst for the target; WaiterActive means a
// foreground or park collector is blocked in Claim for the target right
// now; Delivering means an unacknowledged delivery to the target is in
// flight. The waker maps these facts onto its suppression vocabulary; the
// Engine never decides policy.
type WakeCandidate struct {
	BindID       string            `json:"-"`
	Generation   uint64            `json:"-"`
	MessageID    string            `json:"message_id"`
	Target       model.ActorID     `json:"target"`
	Runtime      model.RuntimeKind `json:"runtime,omitempty"`
	SessionID    string            `json:"session_id,omitempty"`
	Enabled      bool              `json:"enabled"`
	Reserved     bool              `json:"reserved"`
	QueueStart   bool              `json:"queue_start"`
	WaiterActive bool              `json:"waiter_active"`
	Delivering   bool              `json:"delivering"`
}

type Binding struct {
	Slot           model.ActorID     `json:"slot"`
	Runtime        model.RuntimeKind `json:"runtime,omitempty"`
	BindID         string            `json:"bind_id"`
	Generation     uint64            `json:"generation"`
	Active         bool              `json:"active"`
	SessionID      string            `json:"session_id,omitempty"`
	TranscriptPath string            `json:"transcript_path,omitempty"`
	ParkEnabled    bool              `json:"park_enabled"`
	LastActivity   time.Time         `json:"last_activity,omitempty"`
}

type bindingFact struct {
	Binding
	CredentialHash string `json:"credential_hash"`
}

// BindRequest associates at bind time: the native harness exposes its official
// session id to tool-call subprocesses (Claude Code: CLAUDE_CODE_SESSION_ID;
// Codex: CODEX_SESSION_ID), so the client presents it directly and no nonce
// round-trip is required. SessionID is mandatory and participates in the global
// (runtime, session) uniqueness check.
type BindRequest struct {
	BindID         string `json:"bind_id"`
	CredentialHash string `json:"credential_hash"`
	SessionID      string `json:"session_id"`
	Replace        bool   `json:"replace,omitempty"`
}

// Auth is transport-only. Never marshal it into an event, error, or response.
type Auth struct {
	Slot       model.ActorID
	BindID     string
	Generation uint64
	SessionID  string
	Secret     string
}

type Message struct {
	Review           *review.Anchor     `json:"review,omitempty"`
	ID               string             `json:"id"`
	From             model.ActorID      `json:"from"`
	To               model.ActorID      `json:"to"`
	Text             string             `json:"text"`
	Attachments      []model.Attachment `json:"attachments,omitempty"`
	Quote            *model.AgentQuote  `json:"quote,omitempty"`
	TargetGeneration uint64             `json:"target_generation,omitempty"`
	State            string             `json:"state"`
	CreatedAt        time.Time          `json:"created_at"`
	UpdatedAt        time.Time          `json:"updated_at"`
	ClaimedAt        time.Time          `json:"claimed_at,omitempty"`
	Receipt          string             `json:"-"`
	RetryOf          string             `json:"retry_of,omitempty"`
	Source           string             `json:"source"`
}

type messageFact struct {
	Message
	Receipt   string `json:"receipt,omitempty"`
	ClientKey string `json:"client_key,omitempty"`
}

type Publication struct {
	BindID     string   `json:"bind_id"`
	Generation uint64   `json:"generation"`
	ReportSeq  uint64   `json:"report_seq"`
	Message    *Message `json:"message,omitempty"`
	GapFrom    uint64   `json:"gap_from,omitempty"`
	GapTo      uint64   `json:"gap_to,omitempty"`
}

type SendRequest struct {
	Review        *review.Anchor `json:"review,omitempty"`
	ID            string         `json:"id"`
	Text          string         `json:"text"`
	To            model.ActorID  `json:"to,omitempty"`
	AttachmentIDs []string       `json:"attachment_ids,omitempty"`
	QuoteID       string         `json:"quote_id,omitempty"`
}

type Claim struct {
	ID       string `json:"id"`
	Receipt  string `json:"receipt"`
	Envelope string `json:"envelope"`
}

type Audit struct {
	Seq    uint64        `json:"seq"`
	Kind   string        `json:"kind"`
	Actor  model.ActorID `json:"actor"`
	At     time.Time     `json:"at"`
	Detail string        `json:"detail,omitempty"`
}

type Snapshot struct {
	HostMode model.HostMode            `json:"host_mode"`
	RoomID   string                    `json:"room_id"`
	Bindings map[model.ActorID]Binding `json:"bindings"`
	Messages []Message                 `json:"messages"`
	Audit    []Audit                   `json:"audit"`
	Sequence uint64                    `json:"sequence"`
	Notice   string                    `json:"notice"`
}
