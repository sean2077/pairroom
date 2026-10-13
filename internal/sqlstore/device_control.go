package sqlstore

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
)

type deviceMutation struct {
	enrollment                                      *DeviceEnrollmentCommand
	decision                                        *DeviceEnrollmentDecision
	completion                                      *DeviceEnrollmentCompletion
	rotation                                        *DeviceRotationCommand
	requestID, deviceID, origin, sourceKey, agentID string
	runtime                                         model.RuntimeKind
	tokenHash                                       [32]byte
}

func controlReadOnly(kind string) bool {
	switch kind {
	case controlAuthenticate, controlReadiness, controlDeviceAuthenticate, controlBindingAuthenticate, controlEnrollmentRead, controlAdministrationRead:
		return true
	}
	return false
}
func validDeviceOrigin(origin string) bool {
	u, e := url.Parse(origin)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Opaque == "" && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && strings.ToLower(u.Host) == u.Host && origin == "https://"+u.Host && len(origin) <= 2048
}
func validDeviceCandidate(c DeviceCredentialCandidate) bool {
	return validRoomCreateIdentity(c.GrantID) && c.AccessHash != ([32]byte{}) && c.RefreshHash != ([32]byte{}) && c.AccessHash != c.RefreshHash
}
func (s *Store) deviceControlPlan(kind, id, principal string, m *deviceMutation) opPlan {
	return opPlan{key: OpKey{Namespace: "device", Deployment: s.cfg.DeploymentID, OpKind: kind, Identifier: id, Principal: principal}, control: &controlMutation{kind: kind, device: m}}
}
func (s *Store) BeginDeviceEnrollment(ctx context.Context, cmd DeviceEnrollmentCommand) (Result, error) {
	if !validRoomCreateIdentity(cmd.RequestID) || !validRoomCreateIdentity(cmd.DeviceID) || !validDeviceOrigin(cmd.Origin) || cmd.ProofHash == ([32]byte{}) || !validDeviceCandidate(cmd.Credential) || len(cmd.Label) == 0 || len(cmd.Label) > 128 || !utf8.ValidString(cmd.Label) || strings.ContainsFunc(cmd.Label, unicode.IsControl) || enterpriseauth.ValidateDeviceCallback(cmd.CallbackURI, cmd.ClientState) != nil {
		return refused(ClassInvalidRequest, nil)
	}
	p := s.deviceControlPlan(ControlDeviceEnrollment, cmd.RequestID, hex.EncodeToString(cmd.ProofHash[:]), &deviceMutation{enrollment: &cmd, requestID: cmd.RequestID})
	p.hash = controlHash(p.key, struct {
		Command                DeviceEnrollmentCommand
		Proof, Access, Refresh [32]byte
	}{cmd, cmd.ProofHash, cmd.Credential.AccessHash, cmd.Credential.RefreshHash})
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) GetDeviceEnrollment(ctx context.Context, auth AuthContext, requestID string) (DeviceEnrollmentInfo, error) {
	if !validRoomCreateIdentity(requestID) || auth.Deployment != s.cfg.DeploymentID || !browserAuth(auth) {
		return DeviceEnrollmentInfo{}, refusedError(ClassAuthorization, nil)
	}
	p := opPlan{auth: auth, control: &controlMutation{kind: controlEnrollmentRead, device: &deviceMutation{requestID: requestID}}}
	r, e := s.resolveWithFallback(ctx, p)
	if e != nil {
		return DeviceEnrollmentInfo{}, e
	}
	if r.enrollment == nil {
		return DeviceEnrollmentInfo{}, refusedError(ClassStateInvalid, nil)
	}
	return *r.enrollment, nil
}
func browserAuth(auth AuthContext) bool {
	return auth.SessionID != "" && auth.DeviceGrantID == "" && auth.BindingGrantID == ""
}
func (s *Store) DecideDeviceEnrollment(ctx context.Context, cmd DeviceEnrollmentDecision) (Result, error) {
	if !validRoomCreateIdentity(cmd.RequestID) || !validRoomCreateIdentity(cmd.ClientID) || cmd.Auth.Deployment != s.cfg.DeploymentID || !browserAuth(cmd.Auth) || cmd.Approve && cmd.CodeHash == ([32]byte{}) || !cmd.Approve && cmd.CodeHash != ([32]byte{}) {
		return refused(ClassInvalidRequest, nil)
	}
	p := s.deviceControlPlan(ControlDeviceDecision, cmd.ClientID, cmd.Auth.UserID, &deviceMutation{decision: &cmd, requestID: cmd.RequestID})
	p.auth = cmd.Auth
	p.hash = controlHash(p.key, struct {
		RequestID string
		Approve   bool
		CodeHash  [32]byte
	}{cmd.RequestID, cmd.Approve, cmd.CodeHash})
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) CompleteDeviceEnrollment(ctx context.Context, cmd DeviceEnrollmentCompletion) (Result, error) {
	if !validRoomCreateIdentity(cmd.RequestID) || !validRoomCreateIdentity(cmd.ClientID) || !validDeviceOrigin(cmd.Origin) || cmd.ProofHash == ([32]byte{}) || cmd.CodeHash == ([32]byte{}) {
		return refused(ClassInvalidRequest, nil)
	}
	p := s.deviceControlPlan(ControlDeviceComplete, cmd.ClientID, hex.EncodeToString(cmd.ProofHash[:]), &deviceMutation{completion: &cmd, requestID: cmd.RequestID})
	p.hash = controlHash(p.key, struct {
		RequestID, Origin string
		Proof, Code       [32]byte
	}{cmd.RequestID, cmd.Origin, cmd.ProofHash, cmd.CodeHash})
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) rotationPlan(cmd DeviceRotationCommand, recover bool) (opPlan, string) {
	if !validRoomCreateIdentity(cmd.DeviceID) || !validRoomCreateIdentity(cmd.CurrentGrantID) || !validRoomCreateIdentity(cmd.RotationID) || !validDeviceOrigin(cmd.Origin) || cmd.ClientProofHash == ([32]byte{}) || cmd.CurrentRefreshHash == ([32]byte{}) || !validDeviceCandidate(cmd.Next) || cmd.Next.GrantID == cmd.CurrentGrantID || cmd.Next.RefreshHash == cmd.CurrentRefreshHash {
		return opPlan{}, ClassInvalidRequest
	}
	kind := ControlDeviceRotate
	if recover {
		kind = ControlDeviceRecover
	}
	p := s.deviceControlPlan(kind, cmd.RotationID, cmd.DeviceID, &deviceMutation{rotation: &cmd, deviceID: cmd.DeviceID, origin: cmd.Origin})
	// Recover compares exactly the original rotation hash, with only kind fixed
	// back to rotate. Credential secrets are explicitly hashed, never JSON-logged.
	key := p.key
	key.OpKind = ControlDeviceRotate
	p.hash = controlHash(key, struct {
		DeviceID, CurrentGrantID, RotationID, Origin, NextGrantID string
		Proof, Refresh, NextAccess, NextRefresh                   [32]byte
	}{cmd.DeviceID, cmd.CurrentGrantID, cmd.RotationID, cmd.Origin, cmd.Next.GrantID, cmd.ClientProofHash, cmd.CurrentRefreshHash, cmd.Next.AccessHash, cmd.Next.RefreshHash})
	return p, ""
}
func (s *Store) RotateDeviceCredential(ctx context.Context, cmd DeviceRotationCommand) (Result, error) {
	p, c := s.rotationPlan(cmd, false)
	if c != "" {
		return refused(c, nil)
	}
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) RecoverDeviceRotation(ctx context.Context, cmd DeviceRotationCommand) (Result, error) {
	p, c := s.rotationPlan(cmd, true)
	if c != "" {
		return refused(c, nil)
	}
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) RegisterAgent(ctx context.Context, auth AuthContext, clientID, sourceKey string, runtime model.RuntimeKind) (Result, error) {
	raw, e := base64.RawURLEncoding.DecodeString(sourceKey)
	if auth.Deployment != s.cfg.DeploymentID || auth.DeviceGrantID == "" || auth.BindingGrantID != "" || auth.SessionID != "" || !validRoomCreateIdentity(clientID) || e != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != sourceKey || !runtime.Valid() || runtime != runtime.Canonical() {
		return refused(ClassInvalidRequest, nil)
	}
	id, e := controlID("agent")
	if e != nil {
		return refused(ClassStateInvalid, e)
	}
	p := s.deviceControlPlan(ControlAgentRegister, clientID, auth.UserID, &deviceMutation{sourceKey: sourceKey, runtime: runtime, agentID: id})
	p.auth = auth
	p.hash = controlHash(p.key, struct {
		DeviceGrantID, SourceKey string
		Runtime                  model.RuntimeKind
	}{auth.DeviceGrantID, sourceKey, runtime})
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) deviceRevocationPlan(auth AuthContext, clientID, deviceID string) (opPlan, string) {
	if !browserAuth(auth) || auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(clientID) || !validRoomCreateIdentity(deviceID) {
		return opPlan{}, ClassInvalidRequest
	}
	p := s.deviceControlPlan(ControlDeviceRevoke, clientID, auth.UserID, &deviceMutation{deviceID: deviceID})
	p.auth = auth
	p.hash = controlHash(p.key, deviceID)
	return p, ""
}
func (s *Store) RevokeDevice(ctx context.Context, auth AuthContext, clientID, deviceID string) (Result, error) {
	p, c := s.deviceRevocationPlan(auth, clientID, deviceID)
	if c != "" {
		return refused(c, nil)
	}
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) ResolveDeviceRevocation(ctx context.Context, auth AuthContext, clientID, deviceID string) (Result, error) {
	p, c := s.deviceRevocationPlan(auth, clientID, deviceID)
	if c != "" {
		return refused(c, nil)
	}
	return s.resolveWithFallback(ctx, p)
}
