package enterpriseclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
)

const stateVersion = 1
const maxRetainedOperations = 16
const maxStateBytes = 64 << 10

type credential struct {
	GrantID          string    `json:"grant_id"`
	Access           secret    `json:"access"`
	Refresh          secret    `json:"refresh"`
	ExpiresAt        time.Time `json:"expires_at"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
}
type pending struct {
	Kind           string     `json:"kind"`
	Phase          string     `json:"phase"`
	RequestID      string     `json:"request_id,omitempty"`
	ClientID       string     `json:"client_id"`
	DeviceID       string     `json:"device_id"`
	UserID         string     `json:"user_id,omitempty"`
	Label          string     `json:"label,omitempty"`
	CallbackURI    string     `json:"callback_uri,omitempty"`
	ClientState    secret     `json:"client_state,omitempty"`
	Proof          secret     `json:"proof"`
	CompletionCode secret     `json:"completion_code,omitempty"`
	CurrentGrantID string     `json:"current_grant_id,omitempty"`
	CurrentRefresh secret     `json:"current_refresh,omitempty"`
	Next           credential `json:"next"`
	CreatedAt      time.Time  `json:"created_at"`
	IntentHash     string     `json:"intent_hash"`
	CompletionHash string     `json:"completion_hash,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at,omitzero"`
}

type retainedOperation struct {
	Origin       string    `json:"origin"`
	DeploymentID string    `json:"deployment_id"`
	RetainedAt   time.Time `json:"retained_at"`
	LogoutAt     time.Time `json:"logout_at"`
	Resolution   string    `json:"resolution"`
	Operation    pending   `json:"operation"`
}
type RetainedOperationInfo struct {
	OperationID string    `json:"operation_id"`
	Kind        string    `json:"kind"`
	Phase       string    `json:"phase"`
	DeviceID    string    `json:"device_id"`
	UserID      string    `json:"user_id,omitempty"`
	IntentHash  string    `json:"intent_hash"`
	RetainedAt  time.Time `json:"retained_at"`
	Resolution  string    `json:"resolution"`
}

func (p pending) intentHash(origin, deployment string) string {
	return digest(struct{ Origin, Deployment, Kind, RequestID, ClientID, DeviceID, UserID, Label, CallbackURI, State, Proof, CurrentGrantID, CurrentRefresh, NextGrantID, NextAccess, NextRefresh string }{origin, deployment, p.Kind, p.RequestID, p.ClientID, p.DeviceID, p.UserID, p.Label, p.CallbackURI, verifier(p.ClientState), verifier(p.Proof), p.CurrentGrantID, verifier(p.CurrentRefresh), p.Next.GrantID, verifier(p.Next.Access), verifier(p.Next.Refresh)})
}
func (p pending) completionHash() string {
	return digest(struct{ RequestID, ClientID, Proof, Code string }{p.RequestID, p.ClientID, verifier(p.Proof), verifier(p.CompletionCode)})
}

type state struct {
	Version      int                 `json:"version"`
	Origin       string              `json:"origin"`
	DeploymentID string              `json:"deployment_id"`
	Revision     uint64              `json:"revision"`
	DeviceID     string              `json:"device_id,omitempty"`
	UserID       string              `json:"user_id,omitempty"`
	Proof        secret              `json:"proof,omitempty"`
	Active       *credential         `json:"active,omitempty"`
	Pending      *pending            `json:"pending,omitempty"`
	LoggedOut    bool                `json:"logged_out,omitempty"`
	LogoutAt     time.Time           `json:"logout_at,omitzero"`
	Retained     []retainedOperation `json:"retained_operations,omitempty"`
}

type Status struct {
	Scope              string                  `json:"scope"`
	Origin             string                  `json:"origin"`
	DeploymentID       string                  `json:"deployment_id"`
	DeviceID           string                  `json:"device_id,omitempty"`
	UserID             string                  `json:"user_id,omitempty"`
	State              string                  `json:"state"`
	PendingKind        string                  `json:"pending_kind,omitempty"`
	PendingOperationID string                  `json:"pending_operation_id,omitempty"`
	ExpiresAt          time.Time               `json:"expires_at,omitzero"`
	RefreshExpiresAt   time.Time               `json:"refresh_expires_at,omitzero"`
	Revision           uint64                  `json:"revision"`
	RetainedCount      int                     `json:"retained_count"`
	RetainedOperations []RetainedOperationInfo `json:"retained_operations,omitempty"`
	Notice             string                  `json:"notice,omitempty"`
}

func (s state) status() Status {
	result := Status{Scope: "local", Origin: s.Origin, DeploymentID: s.DeploymentID, DeviceID: s.DeviceID, UserID: s.UserID, State: "logged_out", Revision: s.Revision}
	if s.Active != nil && !s.LoggedOut {
		result.State = "active"
		result.ExpiresAt = s.Active.ExpiresAt
		result.RefreshExpiresAt = s.Active.RefreshExpiresAt
		if !s.Active.ExpiresAt.After(time.Now()) {
			result.State = "access_expired"
		}
	}
	if s.Pending != nil {
		if s.Pending.Kind == "enrollment" {
			result.DeviceID = s.Pending.DeviceID
			result.UserID = ""
		}
		result.PendingKind = s.Pending.Kind
		result.PendingOperationID = s.Pending.ClientID
		result.State = "pending_" + s.Pending.Phase
		if s.LoggedOut {
			result.State = "logged_out_pending_reconciliation"
		}
	}
	result.RetainedCount = len(s.Retained)
	for _, entry := range s.Retained {
		p := entry.Operation
		result.RetainedOperations = append(result.RetainedOperations, RetainedOperationInfo{p.ClientID, p.Kind, p.Phase, p.DeviceID, p.UserID, p.IntentHash, entry.RetainedAt, entry.Resolution})
	}
	if result.RetainedCount > 0 {
		result.Notice = "Retained operations remain unresolved; verify server registration and Bindings, or explicitly revoke them in the browser."
	}
	return result
}

func prepareDirectory(explicit, origin, deployment string) (string, error) {
	dir := explicit
	if dir == "" {
		home, e := os.UserConfigDir()
		if e != nil {
			return "", e
		}
		base := filepath.Join(home, "PairRoomEnterprise")
		if e = privatefile.Mkdir(base); e != nil {
			return "", e
		}
		hash := sha256.Sum256([]byte(origin + "\x00" + deployment))
		dir = filepath.Join(base, hex.EncodeToString(hash[:16]))
	}
	absolute, e := filepath.Abs(dir)
	if e != nil {
		return "", e
	}
	if e = privatefile.Mkdir(absolute); e != nil {
		return "", e
	}
	return absolute, nil
}
func (c *Client) read() (state, error) {
	if e := privatefile.CheckDirectory(c.dir); e != nil {
		return state{}, ErrPrivateState
	}
	var s state
	e := privatefile.ReadJSON(filepath.Join(c.dir, "device.json"), maxStateBytes, &s)
	if errors.Is(e, os.ErrNotExist) {
		return state{Version: stateVersion, Origin: c.origin, DeploymentID: c.deployment}, nil
	}
	if e != nil {
		return state{}, ErrPrivateState
	}
	if s.Version != stateVersion || s.Origin != c.origin || s.DeploymentID != c.deployment {
		return state{}, ErrPrivateState
	}
	if len(s.Retained) > maxRetainedOperations {
		return state{}, ErrPrivateState
	}
	for _, entry := range s.Retained {
		if entry.Origin != c.origin || entry.DeploymentID != c.deployment || entry.RetainedAt.IsZero() || entry.LogoutAt.IsZero() || entry.Operation.IntentHash != entry.Operation.intentHash(c.origin, c.deployment) {
			return state{}, ErrPrivateState
		}
	}
	if s.Active != nil && (!validID(s.UserID) || !validID(s.DeviceID) || !validSecret(s.Proof) || !validID(s.Active.GrantID) || !validSecret(s.Active.Access) || !validSecret(s.Active.Refresh) || s.Active.ExpiresAt.IsZero() || s.Active.RefreshExpiresAt.IsZero()) {
		return state{}, ErrPrivateState
	}
	if p := s.Pending; p != nil {
		switch p.Phase {
		case "prepared", "awaiting_approval", "completion_ready", "begin_unknown", "completion_unknown", "rotation_unknown", "not_committed":
		default:
			return state{}, ErrPrivateState
		}
		if p.CompletionCode == "" && p.CompletionHash != "" {
			return state{}, ErrPrivateState
		}
		if (p.Kind != "enrollment" && p.Kind != "rotation") || !validID(p.ClientID) || !validID(p.DeviceID) || !validID(p.Next.GrantID) || !validSecret(p.Proof) || !validSecret(p.Next.Access) || !validSecret(p.Next.Refresh) || p.IntentHash != p.intentHash(c.origin, c.deployment) {
			return state{}, ErrPrivateState
		}
		if p.Kind == "enrollment" && (!validID(p.RequestID) || !validSecret(p.ClientState) || !validCallbackURI(p.CallbackURI)) {
			return state{}, ErrPrivateState
		}
		if p.Kind == "rotation" && (!validID(p.UserID) || !validID(p.CurrentGrantID) || !validSecret(p.CurrentRefresh)) {
			return state{}, ErrPrivateState
		}
		if p.CompletionCode != "" && (!validSecret(p.CompletionCode) || p.CompletionHash != p.completionHash()) {
			return state{}, ErrPrivateState
		}
	}
	return s, nil
}
func (c *Client) save(s *state) error {
	if s.Revision == ^uint64(0) {
		return ErrPrivateState
	}
	s.Revision++
	data, err := json.Marshal(s)
	if err != nil || len(data)+1 > maxStateBytes {
		return ErrPrivateState
	}
	if e := privatefile.WriteJSON(filepath.Join(c.dir, "device.json"), s); e != nil {
		return ErrPrivateState
	}
	return nil
}
func (c *Client) lock(ctx context.Context) (func(), error) {
	if e := privatefile.CheckDirectory(c.dir); e != nil {
		return nil, ErrPrivateState
	}
	unlock, e := privatelock.Lock(ctx, c.dir)
	if e != nil {
		return nil, e
	}
	return unlock, nil
}
func (c *Client) Status() (Status, error) {
	s, e := c.read()
	if e != nil {
		return Status{}, e
	}
	return s.status(), nil
}

// Logout forgets the active local credential and discovery authority. An
// uncertain operation's private journal remains available for explicit
// reconciliation; this action does not revoke server registration or Bindings.
func (c *Client) Logout(ctx context.Context) (Status, error) {
	unlock, e := c.lock(ctx)
	if e != nil {
		return Status{}, e
	}
	defer unlock()
	s, e := c.read()
	if e != nil {
		return Status{}, e
	}
	s.Active = nil
	s.Proof = ""
	s.LoggedOut = true
	s.LogoutAt = time.Now().UTC()
	if e = c.save(&s); e != nil {
		return Status{}, e
	}
	return s.status(), nil
}
