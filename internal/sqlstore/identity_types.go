package sqlstore

import (
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

// BrowserSessionCandidate is minted by the trusted HTTP boundary. Only its
// hashes are persisted; neither hash nor the underlying secrets belongs in an
// accepted operation receipt. A candidate must remain fixed for one ClientID.
type BrowserSessionCandidate struct {
	SessionID string
	TokenHash [32]byte `json:"-"`
	CSRFHash  [32]byte `json:"-"`
	ExpiresAt time.Time
}

type IdentityLoginCommand struct {
	ClientID   string
	Completion enterpriseauth.Completion
	Session    BrowserSessionCandidate
}

// BrowserSession is returned only after fenced, live credential validation.
// Public adapters pass Auth unchanged into each transaction and keep CSRFHash
// private while checking their request's CSRF proof and Origin.
type BrowserSession struct {
	Auth          AuthContext
	CSRFHash      [32]byte `json:"-"`
	ExpiresAt     time.Time
	Administrator bool
}

type TrustConnectionConfig struct {
	ConnectionID string
	Issuer       string
}

// IdentityConfig is an explicit operator selection, never an HTTP request.
// Bootstrap is the exact selected external identity; Admitted identities may
// log in but acquire no administrative role. Replaying configuration does not
// restore any later-disabled User, administrator role or revoked session.
type IdentityConfig struct {
	Connections []TrustConnectionConfig
	Bootstrap   enterpriseauth.VerifiedIdentity
	Admitted    []enterpriseauth.VerifiedIdentity
}

// ControlReceipt is the closed redacted result of a deployment-scoped
// operation. It is not a Room fact and never contains application credentials,
// CSRF verifiers, upstream tokens, Room contents or arbitrary caller detail.
type ControlReceipt struct {
	Version          int       `json:"version"`
	Kind             string    `json:"kind"`
	Seq              uint64    `json:"seq"`
	UserID           string    `json:"user_id,omitempty"`
	SessionID        string    `json:"session_id,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	Administrator    bool      `json:"administrator,omitempty"`
	TargetUserID     string    `json:"target_user_id,omitempty"`
	Enabled          bool      `json:"enabled,omitempty"`
	IdentityHash     string    `json:"identity_hash,omitempty"`
	RequestID        string    `json:"request_id,omitempty"`
	DeviceID         string    `json:"device_id,omitempty"`
	AgentID          string    `json:"agent_id,omitempty"`
	GrantID          string    `json:"grant_id,omitempty"`
	Status           string    `json:"status,omitempty"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at,omitzero"`
}

// RequiresSessionAuth lets a public adapter reject a Store configured only
// for trusted internal fixtures. Public service construction must require it.
func (s *Store) RequiresSessionAuth() bool { return s != nil && s.cfg.RequireSessionAuth }
