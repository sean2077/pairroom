package sqlstore

import "time"

// GovernanceIntent fixes the complete target of one freshly reauthenticated
// ownership recovery. A missing Owner membership is not a disabled Owner.
type GovernanceIntent struct {
	RoomID              string `json:"room_id"`
	TargetUserID        string `json:"target_user_id"`
	ExpectedOwnerUserID string `json:"expected_owner_user_id"`
	ExpectedRevision    uint64 `json:"expected_revision"`
}

// GovernanceAction is a public reference bound to one exact browser session,
// not a bearer capability. Consuming it still checks the live administrator,
// disabled current Owner, eligible target, expected revision and fresh proof.
type GovernanceAction struct {
	ID        string           `json:"id"`
	Intent    GovernanceIntent `json:"intent"`
	ExpiresAt time.Time        `json:"expires_at"`
	Ready     bool             `json:"ready"`
}

type OwnershipTransferCommand struct {
	Auth               AuthContext
	ClientID           string
	Intent             GovernanceIntent
	GovernanceActionID string
}

// GovernanceReceipt is a closed metadata-only control result. It never embeds
// a content snapshot, browser credential or the reauthentication evidence.
type GovernanceReceipt struct {
	Version     int               `json:"version"`
	Kind        string            `json:"kind"`
	Action      *GovernanceAction `json:"action,omitempty"`
	RoomID      string            `json:"room_id,omitempty"`
	OwnerUserID string            `json:"owner_user_id,omitempty"`
	Seq         uint64            `json:"seq,omitempty"`
	Revision    uint64            `json:"revision,omitempty"`
}
