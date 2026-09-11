// Package relay implements Native-hosted Rooms without spawning a vendor process.
// The Room Event Log is the sole source of binding, publication and inbox facts.
package relay

import (
	"errors"
	"time"

	"github.com/sean2077/pairroom/internal/model"
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
)

var (
	ErrAuth     = errors.New("relay authentication failed: binding, generation and associated session must match")
	ErrOccupied = errors.New("slot is occupied; resume the same session with --continue or explicitly --replace (cannot stop native work)")
	ErrNonce    = errors.New("binding nonce is missing, invalid or already consumed")
	ErrClosed   = errors.New("native relay is closed or draining")
	ErrUnknown  = errors.New("publication result unknown; inspect status before explicitly deciding recovery")
)

type Binding struct {
	Slot           model.ActorID `json:"slot"`
	BindID         string        `json:"bind_id"`
	Generation     uint64        `json:"generation"`
	Active         bool          `json:"active"`
	SessionID      string        `json:"session_id,omitempty"`
	TranscriptPath string        `json:"transcript_path,omitempty"`
	ParkEnabled    bool          `json:"park_enabled"`
	LastActivity   time.Time     `json:"last_activity,omitempty"`
}

type bindingFact struct {
	Binding
	CredentialHash string `json:"credential_hash"`
	NonceHash      string `json:"nonce_hash,omitempty"`
}

type BindRequest struct {
	BindID         string `json:"bind_id"`
	CredentialHash string `json:"credential_hash"`
	NonceHash      string `json:"nonce_hash"`
	SessionID      string `json:"session_id,omitempty"`
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
	ID            string        `json:"id"`
	Text          string        `json:"text"`
	To            model.ActorID `json:"to,omitempty"`
	AttachmentIDs []string      `json:"attachment_ids,omitempty"`
	QuoteID       string        `json:"quote_id,omitempty"`
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
