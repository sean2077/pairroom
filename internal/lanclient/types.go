// Package lanclient joins a Native LAN Room without a local Service. A client
// stores only its private association, transport identity and receipt journal;
// the remote Room remains the authority for messages and delivery state.
// Methods perform bounded foreground work. The package starts no listener,
// helper process or background worker.
package lanclient

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/relay"
)

var (
	ErrUnavailable          = errors.New("LAN Room unavailable or operation not confirmed; inspect status and reuse the original publication identity")
	ErrTransportUnavailable = errors.New("LAN host could not be reached or its pinned identity was not confirmed")
	ErrInactive             = errors.New("LAN membership is inactive; it cannot be silently recreated")
	ErrInvalidState         = errors.New("invalid LAN client state; restore its original identity before use")
	ErrCollectorBusy        = errors.New("a collector is already waiting for this joined Room")
)

// Error is a bounded, locally chosen failure description. Remote response
// text, transport URLs and nested TLS errors never enter native model context.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

type JoinOptions struct {
	Invite         string            `json:"invite"`
	Workspace      string            `json:"workspace"`
	Runtime        model.RuntimeKind `json:"runtime"`
	SessionID      string            `json:"session_id"`
	BindID         string            `json:"bind_id"`
	CredentialHash string            `json:"credential_hash"`
	Label          string            `json:"label,omitempty"`
	Replace        bool              `json:"replace,omitempty"`
}

type JoinResult struct {
	ID             string         `json:"room_id"`
	Status         string         `json:"status"`
	Receipt        string         `json:"receipt"`
	Binding        *relay.Binding `json:"binding,omitempty"`
	Bootstrap      string         `json:"bootstrap,omitempty"`
	Collaboration  string         `json:"collaboration,omitempty"`
	PreviousBindID string         `json:"previous_bind_id,omitempty"`
}

// Metadata is private local binding information for CLI routing and optional
// local observers. It contains no TLS key or local credential digest. Never
// serialize it into a remote request or a public Management projection.
type Metadata struct {
	ID             string
	Invite         lanshare.Invite
	Workspace      string
	Runtime        model.RuntimeKind
	SessionID      string
	BindID         string
	Generation     uint64
	Slot           model.ActorID
	Status         string
	Binding        relay.Binding
	PreviousBindID string
}

// Snapshot is the optional local UI projection. Native session identity,
// transcript paths, local credentials and original receipts are private.
type Snapshot struct {
	ID           string            `json:"id"`
	RemoteRoomID string            `json:"remote_room_id"`
	Name         string            `json:"name"`
	Workspace    string            `json:"workspace"`
	Slot         model.ActorID     `json:"slot,omitempty"`
	Runtime      model.RuntimeKind `json:"runtime"`
	Status       string            `json:"status"`
	Connected    bool              `json:"connected"`
	LastSeen     time.Time         `json:"last_seen,omitempty"`
	HostPin      string            `json:"host_pin"`
	Endpoint     string            `json:"endpoint"`
	Generation   uint64            `json:"generation,omitempty"`
}

type Store struct {
	root       string
	identities *nativeidentity.Store
	mu         sync.Mutex
	clients    map[string]*Client
}

// Client holds reusable HTTP pools, never an authoritative cached record.
// Every local operation reloads its record under a cross-process kernel lock.
type Client struct {
	store        *Store
	id           string
	dir          string
	mu           sync.Mutex
	http         *http.Client
	transportKey string
	connected    bool
	lastSeen     time.Time
}

type record struct {
	Schema         int                     `json:"schema"`
	ID             string                  `json:"id"`
	Invite         lanshare.Invite         `json:"invite"`
	RequestID      string                  `json:"request_id"`
	Identity       lanshare.Identity       `json:"identity"`
	Workspace      string                  `json:"workspace"`
	Runtime        model.RuntimeKind       `json:"runtime"`
	SessionID      string                  `json:"session_id"`
	BindID         string                  `json:"bind_id"`
	PreviousBindID string                  `json:"previous_bind_id,omitempty"`
	CredentialHash string                  `json:"credential_hash"`
	TranscriptPath string                  `json:"transcript_path,omitempty"`
	Status         string                  `json:"status"`
	Receipt        string                  `json:"receipt,omitempty"`
	Room           *lanshare.RoomInfo      `json:"room,omitempty"`
	ParkEnabled    bool                    `json:"park_enabled"`
	LastActivity   time.Time               `json:"last_activity,omitempty"`
	Spent          []relay.WakeReservation `json:"spent,omitempty"`
	Deliveries     []delivery              `json:"deliveries,omitempty"`
}

type delivery struct {
	ID         string `json:"id"`
	Receipt    string `json:"receipt"`
	Generation uint64 `json:"generation"`
	State      string `json:"state"`
}
