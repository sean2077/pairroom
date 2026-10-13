package sqlstore

import (
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// Catalog resources are deployment-local logical records. Project IDs never
// denote Git repositories, directories, or an implicit Room authorization.
type CatalogQuery struct {
	Resource  string `json:"resource"`
	After     string `json:"after,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	RoomID    string `json:"room_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	State     string `json:"state,omitempty"`
}

type CatalogRoom struct {
	RoomID                   string `json:"room_id"`
	Name                     string `json:"name"`
	Role                     string `json:"role"`
	OwnerUserID              string `json:"owner_user_id"`
	ProjectID                string `json:"project_id,omitempty"`
	ProjectName              string `json:"project_name,omitempty"`
	Archived                 bool   `json:"archived"`
	OwnershipPendingRecovery bool   `json:"ownership_pending_recovery"`
	Seq                      uint64 `json:"seq"`
	Revision                 uint64 `json:"revision"`
}

type CatalogProject struct {
	ProjectID   string `json:"project_id"`
	Name        string `json:"name"`
	OwnerUserID string `json:"owner_user_id"`
	Role        string `json:"role"`
	Archived    bool   `json:"archived"`
	Revision    uint64 `json:"revision"`
}

type CatalogMember struct {
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
	Disabled bool   `json:"disabled"`
}

type CatalogDevice struct {
	DeviceID  string    `json:"device_id"`
	Label     string    `json:"label"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
}

type CatalogAgent struct {
	AgentID   string            `json:"agent_id"`
	DeviceID  string            `json:"device_id"`
	Runtime   model.RuntimeKind `json:"runtime"`
	Status    string            `json:"status"`
	CreatedAt time.Time         `json:"created_at"`
}

type CatalogSession struct {
	SessionID string    `json:"session_id"`
	Current   bool      `json:"current"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type CatalogPage struct {
	Resource string           `json:"resource"`
	Next     string           `json:"next,omitempty"`
	Rooms    []CatalogRoom    `json:"rooms,omitempty"`
	Projects []CatalogProject `json:"projects,omitempty"`
	Members  []CatalogMember  `json:"members,omitempty"`
	Devices  []CatalogDevice  `json:"devices,omitempty"`
	Agents   []CatalogAgent   `json:"agents,omitempty"`
	Sessions []CatalogSession `json:"sessions,omitempty"`
}

// Only the fields used by the selected Kind may be supplied. Room metadata
// changes preserve the Room ID and every Binding and append a typed fact.
type CatalogIntent struct {
	Kind         string `json:"kind"`
	RoomID       string `json:"room_id,omitempty"`
	ProjectID    string `json:"project_id,omitempty"`
	TargetUserID string `json:"target_user_id,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	AgentID      string `json:"agent_id,omitempty"`
	Name         string `json:"name,omitempty"`
}

type CatalogCommand struct {
	Auth             AuthContext
	ClientID         string
	ExpectedRevision uint64
	Intent           CatalogIntent
}

type CatalogReceipt struct {
	Version  int           `json:"version"`
	Intent   CatalogIntent `json:"intent"`
	Seq      uint64        `json:"seq,omitempty"`
	Revision uint64        `json:"revision,omitempty"`
}
