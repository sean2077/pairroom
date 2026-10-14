package sqlstore

import (
	"context"
	"encoding/hex"
	"github.com/sean2077/pairroom/internal/model"
	"strings"
)

func validHeadlessRequestID(id string) bool {
	if !strings.HasPrefix(id, "request-") || len(id) != 40 {
		return false
	}
	b, e := hex.DecodeString(id[8:])
	return e == nil && len(b) == 16 && hex.EncodeToString(b) == id[8:]
}
func (s *Store) DecideHeadlessDeviceEnrollment(ctx context.Context, c HeadlessEnrollmentDecision) (Result, error) {
	if !validHeadlessRequestID(c.RequestID) || !validRoomCreateIdentity(c.ClientID) || c.Auth.Deployment != s.cfg.DeploymentID || !browserAuth(c.Auth) || c.Approve && c.ConfirmationHash == ([32]byte{}) || !c.Approve && c.ConfirmationHash != ([32]byte{}) {
		return refused(ClassInvalidRequest, nil)
	}
	p := s.deviceControlPlan(ControlHeadlessDecision, c.ClientID, c.Auth.UserID, &deviceMutation{headlessDecision: &c, requestID: c.RequestID})
	p.auth = c.Auth
	p.hash = controlHash(p.key, struct {
		RequestID    string
		Approve      bool
		Confirmation [32]byte
	}{c.RequestID, c.Approve, c.ConfirmationHash})
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) CompleteHeadlessDeviceEnrollment(ctx context.Context, c HeadlessEnrollmentCompletion) (Result, error) {
	if !validHeadlessRequestID(c.RequestID) || !validRoomCreateIdentity(c.ClientID) || !validDeviceOrigin(c.Origin) || c.ProofHash == ([32]byte{}) || c.ConfirmationHash == ([32]byte{}) {
		return refused(ClassInvalidRequest, nil)
	}
	old := DeviceEnrollmentCompletion{RequestID: c.RequestID, ClientID: c.ClientID, Origin: c.Origin, ProofHash: c.ProofHash, CodeHash: c.ConfirmationHash}
	p := s.deviceControlPlan(ControlHeadlessComplete, c.ClientID, hex.EncodeToString(c.ProofHash[:]), &deviceMutation{completion: &old, headlessComplete: true, requestID: c.RequestID})
	p.hash = controlHash(p.key, struct {
		RequestID, Origin   string
		Proof, Confirmation [32]byte
	}{c.RequestID, c.Origin, c.ProofHash, c.ConfirmationHash})
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) headlessPollPlan(c HeadlessEnrollmentPoll) (opPlan, string) {
	if !validHeadlessRequestID(c.RequestID) || !validDeviceOrigin(c.Origin) || c.ProofHash == ([32]byte{}) || c.Sequence == 0 || c.Sequence > 120 {
		return opPlan{}, ClassInvalidRequest
	}
	p := s.deviceControlPlan(controlHeadlessPoll, c.RequestID, hex.EncodeToString(c.ProofHash[:]), &deviceMutation{poll: &c, requestID: c.RequestID})
	p.hash = controlHash(p.key, struct {
		RequestID, Origin string
		Proof             [32]byte
		Sequence          uint64
	}{c.RequestID, c.Origin, c.ProofHash, c.Sequence})
	return p, ""
}
func (s *Store) PollHeadlessDeviceEnrollment(ctx context.Context, c HeadlessEnrollmentPoll) (Result, error) {
	p, k := s.headlessPollPlan(c)
	if k != "" {
		return refused(k, nil)
	}
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) ResolveHeadlessDeviceEnrollmentPoll(ctx context.Context, c HeadlessEnrollmentPoll) (Result, error) {
	p, k := s.headlessPollPlan(c)
	if k != "" {
		return refused(k, nil)
	}
	return s.resolveWithFallback(ctx, p)
}
