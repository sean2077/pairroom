package sqlstore

import "time"

type AdministrationQuery struct {
	Resource string `json:"resource"`
	After    string `json:"after,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

// Identity selectors are exact opaque keys, not email aliases or claims.
// This DTO cannot carry an upstream token, password, auth_time, or client key.
type AdministrationIdentity struct {
	ConnectionID string `json:"connection_id"`
	Issuer       string `json:"issuer"`
	Subject      string `json:"subject"`
}
type AdministrationIntent struct {
	Kind         string                 `json:"kind"`
	TargetUserID string                 `json:"target_user_id,omitempty"`
	Identity     AdministrationIdentity `json:"identity,omitzero"`
	Enabled      bool                   `json:"enabled"`
}
type AdministrationCommand struct {
	Auth     AuthContext
	ClientID string
	Intent   AdministrationIntent
}
type AdministrationUser struct {
	UserID        string    `json:"user_id"`
	Disabled      bool      `json:"disabled"`
	Tombstoned    bool      `json:"tombstoned"`
	Administrator bool      `json:"administrator"`
	CreatedAt     time.Time `json:"created_at"`
}
type AdministrationAdmission struct {
	AdministrationIdentity
	Enabled         bool   `json:"enabled"`
	UserID          string `json:"user_id,omitempty"`
	UserDisabled    bool   `json:"user_disabled"`
	IdentityRetired bool   `json:"identity_retired"`
}
type AdministrationConnection struct {
	ConnectionID string `json:"connection_id"`
	Issuer       string `json:"issuer"`
}
type AdministrationPage struct {
	Resource       string                     `json:"resource"`
	Next           string                     `json:"next,omitempty"`
	Users          []AdministrationUser       `json:"users,omitempty"`
	Administrators []AdministrationUser       `json:"administrators,omitempty"`
	Admissions     []AdministrationAdmission  `json:"admissions,omitempty"`
	Connections    []AdministrationConnection `json:"connections,omitempty"`
}
