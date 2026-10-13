package enterpriseclient

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// CheckStatus checks a confirmed local credential against the live service.
// Pending or logged-out state remains explicitly local and sends no bearer.
func (c *Client) CheckStatus(ctx context.Context) (Status, error) {
	unlock, e := c.lock(ctx)
	if e != nil {
		return Status{}, e
	}
	defer unlock()
	s, e := c.read()
	if e != nil {
		return Status{}, e
	}
	status := s.status()
	if s.Pending != nil || s.Active == nil || s.LoggedOut {
		return status, nil
	}
	statusCode, data, e := c.request(ctx, http.MethodGet, devicePath+"/session", nil, s.Active.Access, 64<<10)
	if e != nil {
		return status, e
	}
	var info struct {
		DeploymentID     string    `json:"deployment_id"`
		Origin           string    `json:"origin"`
		UserID           string    `json:"user_id"`
		DeviceID         string    `json:"device_id"`
		GrantID          string    `json:"grant_id"`
		ExpiresAt        time.Time `json:"expires_at"`
		RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	}
	if statusCode != http.StatusOK {
		return status, &Error{Outcome: "refused", Class: "device_session_unavailable"}
	}
	if decodeBounded(strings.NewReader(string(data)), &info) != nil || info.DeploymentID != c.deployment || info.Origin != c.origin || info.UserID != s.UserID || info.DeviceID != s.DeviceID || info.GrantID != s.Active.GrantID || !info.ExpiresAt.Equal(s.Active.ExpiresAt) || !info.RefreshExpiresAt.Equal(s.Active.RefreshExpiresAt) || !info.ExpiresAt.After(time.Now()) {
		return status, unknown("status_identity_mismatch")
	}
	status.Scope, status.State = "server", "authenticated"
	return status, nil
}
