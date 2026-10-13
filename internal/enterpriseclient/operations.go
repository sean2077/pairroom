package enterpriseclient

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type candidateRequest struct {
	GrantID     string `json:"grant_id"`
	AccessHash  string `json:"access_hash"`
	RefreshHash string `json:"refresh_hash"`
}

func (c credential) candidate() candidateRequest {
	return candidateRequest{c.GrantID, verifier(c.Access), verifier(c.Refresh)}
}
func newCredential() (credential, error) {
	id, e := newID("grant")
	if e != nil {
		return credential{}, e
	}
	access, e := newSecret()
	if e != nil {
		return credential{}, e
	}
	refresh, e := newSecret()
	if e != nil {
		return credential{}, e
	}
	return credential{GrantID: id, Access: access, Refresh: refresh}, nil
}

type beginRequest struct {
	RequestID   string           `json:"request_id"`
	DeviceID    string           `json:"device_id"`
	Label       string           `json:"label"`
	CallbackURI string           `json:"callback_uri"`
	ClientState secret           `json:"client_state"`
	ProofHash   string           `json:"proof_hash"`
	Credential  candidateRequest `json:"credential"`
}
type completeRequest struct {
	ClientID       string `json:"client_id"`
	ClientProof    secret `json:"client_proof"`
	CompletionCode secret `json:"completion_code"`
}
type rotationRequest struct {
	DeviceID       string           `json:"device_id"`
	CurrentGrantID string           `json:"current_grant_id"`
	RotationID     string           `json:"rotation_id"`
	ClientProof    secret           `json:"client_proof"`
	Next           candidateRequest `json:"next"`
}

// Login is a foreground interaction. Recovery is an explicit request to
// reconcile the exact saved enrollment, never permission to mint a replacement.
func (c *Client) Login(parent context.Context, label string, recover bool) (Status, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	unlock, e := c.lock(ctx)
	if e != nil {
		return Status{}, e
	}
	defer unlock()
	s, e := c.read()
	if e != nil {
		return Status{}, e
	}
	var listener *callback
	if recover {
		if s.Pending == nil || s.Pending.Kind != "enrollment" {
			return s.status(), ErrPending
		}
		if s.Pending.CompletionCode == "" {
			listener, e = openCallback(s.Pending.CallbackURI, s.Pending.ClientState)
			if e != nil {
				return s.status(), e
			}
			defer listener.Close()
		}
	} else {
		if s.Pending != nil {
			if !s.LoggedOut || s.LogoutAt.IsZero() {
				return s.status(), ErrPending
			}
			if len(s.Retained) >= maxRetainedOperations {
				return s.status(), ErrRetainedLimit
			}
			resolution := "unresolved"
			if s.Pending.Kind == "enrollment" && !s.Pending.ExpiresAt.IsZero() && !s.Pending.ExpiresAt.After(time.Now()) {
				resolution = "expired_request_unresolved_effects"
			}
			s.Retained = append(s.Retained, retainedOperation{Origin: s.Origin, DeploymentID: s.DeploymentID, RetainedAt: time.Now().UTC(), LogoutAt: s.LogoutAt, Resolution: resolution, Operation: *s.Pending})
			s.Pending = nil
		}
		if s.Active != nil && !s.LoggedOut {
			return s.status(), ErrAlreadyLoggedIn
		}
		if label == "" || len(label) > 128 || !utf8.ValidString(label) || strings.TrimSpace(label) != label || strings.ContainsFunc(label, unicode.IsControl) {
			return s.status(), ErrConfiguration
		}
		proof, e := newSecret()
		if e != nil {
			return s.status(), e
		}
		clientState, e := newSecret()
		if e != nil {
			return s.status(), e
		}
		next, e := newCredential()
		if e != nil {
			return s.status(), e
		}
		deviceID, e := newID("device")
		if e != nil {
			return s.status(), e
		}
		requestID, e := newID("request")
		if e != nil {
			return s.status(), e
		}
		clientID, e := newID("complete")
		if e != nil {
			return s.status(), e
		}
		listener, e = openCallback("", clientState)
		if e != nil {
			return s.status(), e
		}
		defer listener.Close()
		p := &pending{Kind: "enrollment", Phase: "prepared", RequestID: requestID, ClientID: clientID, DeviceID: deviceID, Label: label, CallbackURI: listener.uri, ClientState: clientState, Proof: proof, Next: next, CreatedAt: time.Now().UTC()}
		p.IntentHash = p.intentHash(c.origin, c.deployment)
		s.Pending = p
		s.LoggedOut = false
		s.LogoutAt = time.Time{}
		if e = c.save(&s); e != nil {
			return s.status(), e
		}
	}
	p := s.Pending
	if p.CompletionCode == "" {
		r, e := c.post(ctx, devicePath+"/enrollments", beginRequest{p.RequestID, p.DeviceID, p.Label, p.CallbackURI, p.ClientState, verifier(p.Proof), p.Next.candidate()}, "")
		if e != nil {
			return c.recordFailure(&s, e, "begin_unknown")
		}
		verification := c.origin + "/enterprise/device-enrollments/" + p.RequestID
		if r.Kind != "device.enrollment.begin" || r.RequestID != p.RequestID || r.DeviceID != p.DeviceID || r.Status != "pending" || r.VerificationURL != verification || !r.ExpiresAt.After(time.Now()) {
			return c.recordFailure(&s, unknown("receipt_mismatch"), "begin_unknown")
		}
		p.Phase = "awaiting_approval"
		p.ExpiresAt = r.ExpiresAt.UTC()
		if e = c.save(&s); e != nil {
			return s.status(), e
		}
		if c.openBrowser == nil {
			return s.status(), errors.New("enterprise login browser opener is unavailable; pending request is preserved")
		}
		if e = c.openBrowser(verification); e != nil {
			return s.status(), errors.New("enterprise login browser could not be opened; pending request is preserved")
		}
		delivery, e := listener.wait(ctx)
		if e != nil {
			return s.status(), e
		}
		p.CompletionCode = delivery.code
		p.CompletionHash = p.completionHash()
		p.Phase = "completion_ready"
		if e = c.save(&s); e != nil {
			return s.status(), e
		}
		close(delivery.ack)
	}
	r, e := c.post(ctx, devicePath+"/enrollments/"+p.RequestID+"/complete", completeRequest{p.ClientID, p.Proof, p.CompletionCode}, "")
	if e != nil {
		return c.recordFailure(&s, e, "completion_unknown")
	}
	if r.Kind != "device.enrollment.complete" || r.RequestID != p.RequestID || r.DeviceID != p.DeviceID || r.GrantID != p.Next.GrantID || !validID(r.UserID) || r.Status != "completed" || !validExpiry(r) {
		return c.recordFailure(&s, unknown("receipt_mismatch"), "completion_unknown")
	}
	return c.activate(&s, r)
}
func validExpiry(r operationResponse) bool {
	now := time.Now()
	return r.ExpiresAt.After(now) && !r.RefreshExpiresAt.Before(r.ExpiresAt) && !r.ExpiresAt.After(now.Add(16*time.Minute)) && !r.RefreshExpiresAt.After(now.Add(31*24*time.Hour))
}
func (c *Client) activate(s *state, r operationResponse) (Status, error) {
	p := s.Pending
	next := p.Next
	next.ExpiresAt, next.RefreshExpiresAt = r.ExpiresAt.UTC(), r.RefreshExpiresAt.UTC()
	s.Active = &next
	s.UserID, s.DeviceID, s.Proof = r.UserID, p.DeviceID, p.Proof
	s.Pending = nil
	s.LoggedOut = false
	s.LogoutAt = time.Time{}
	if e := c.save(s); e != nil {
		return Status{}, e
	}
	return s.status(), nil
}
func (c *Client) recordFailure(s *state, err error, fallback string) (Status, error) {
	s.Pending.Phase = fallback
	var operation *Error
	if errors.As(err, &operation) && operation.Outcome == "not_committed" {
		s.Pending.Phase = "not_committed"
	}
	if e := c.save(s); e != nil {
		return s.status(), e
	}
	return s.status(), err
}

const (
	RefreshNew     = "new"
	RefreshRecover = "recover"
	RefreshRetry   = "retry"
)

// Refresh holds the cross-process private-directory lock across journal save,
// the single HTTP effect and confirmed local activation. No response failure
// changes the candidate or causes an automatic retry/recovery request.
func (c *Client) Refresh(ctx context.Context, mode string) (Status, error) {
	if mode != RefreshNew && mode != RefreshRecover && mode != RefreshRetry {
		return Status{}, ErrConfiguration
	}
	unlock, e := c.lock(ctx)
	if e != nil {
		return Status{}, e
	}
	defer unlock()
	s, e := c.read()
	if e != nil {
		return Status{}, e
	}
	if mode == RefreshNew {
		if s.Pending != nil {
			return s.status(), ErrPending
		}
		if s.Active == nil || s.LoggedOut || !s.Active.RefreshExpiresAt.After(time.Now()) {
			return s.status(), ErrNoSession
		}
		next, e := newCredential()
		if e != nil {
			return s.status(), e
		}
		id, e := newID("rotation")
		if e != nil {
			return s.status(), e
		}
		p := &pending{Kind: "rotation", Phase: "prepared", ClientID: id, DeviceID: s.DeviceID, UserID: s.UserID, Proof: s.Proof, CurrentGrantID: s.Active.GrantID, CurrentRefresh: s.Active.Refresh, Next: next, CreatedAt: time.Now().UTC()}
		p.IntentHash = p.intentHash(c.origin, c.deployment)
		s.Pending = p
		if e = c.save(&s); e != nil {
			return s.status(), e
		}
	} else if s.Pending == nil || s.Pending.Kind != "rotation" {
		return s.status(), ErrPending
	} else if mode == RefreshRetry && s.Pending.Phase != "not_committed" {
		return s.status(), ErrPending
	}
	p := s.Pending
	endpoint, kind := "/credentials/rotate", "device.rotate"
	if mode == RefreshRecover {
		endpoint, kind = "/credentials/recover", "device.recover"
	}
	r, e := c.post(ctx, devicePath+endpoint, rotationRequest{p.DeviceID, p.CurrentGrantID, p.ClientID, p.Proof, p.Next.candidate()}, p.CurrentRefresh)
	if e != nil {
		return c.recordFailure(&s, e, "rotation_unknown")
	}
	if r.Kind != kind || r.DeviceID != p.DeviceID || r.UserID != p.UserID || r.GrantID != p.Next.GrantID || !validExpiry(r) {
		return c.recordFailure(&s, unknown("receipt_mismatch"), "rotation_unknown")
	}
	return c.activate(&s, r)
}
