package sqlstore

import (
	"context"
	"math"

	"github.com/sean2077/pairroom/internal/model"
)

type bindingMutation struct {
	kind, requestID string
	request         BindingRequestCommand
	renewal         BindingRenewalCommand
	approve         bool
	proofHash       [32]byte
}

func bindingReadOnly(kind string) bool {
	return kind == bindingRead || kind == bindingList || kind == bindingContextRead
}

func (s *Store) bindingPlan(auth AuthContext, roomID, clientID string, mutation bindingMutation) (opPlan, string) {
	if auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(auth.UserID) || !validRoomCreateIdentity(roomID) || !validRoomCreateIdentity(clientID) || auth.BindingGrantID != "" {
		return opPlan{}, ClassAuthorization
	}
	browser := browserAuth(auth)
	device := auth.DeviceGrantID != "" && auth.SessionID == "" && auth.BindingGrantID == ""
	if mutation.kind == bindingDecide && !browser || (mutation.kind == bindingRequest || mutation.kind == bindingComplete || mutation.kind == bindingRenew) && !device || bindingReadOnly(mutation.kind) && !browser && !device {
		return opPlan{}, ClassAuthorization
	}
	if mutation.kind != bindingList && mutation.kind != bindingContextRead && mutation.kind != bindingRenew && !validRoomCreateIdentity(mutation.requestID) {
		return opPlan{}, ClassInvalidRequest
	}
	if mutation.kind == bindingRequest {
		cmd := mutation.request
		if !validRoomCreateIdentity(cmd.AgentID) || !validRoomCreateIdentity(cmd.BindingID) || !validRoomCreateIdentity(cmd.Grant.GrantID) || !cmd.Slot.ValidParticipant() || cmd.ObservedGeneration == 0 || cmd.ObservedGeneration > math.MaxInt64 || cmd.ProofHash == ([32]byte{}) || cmd.Grant.TokenHash == ([32]byte{}) || cmd.ProofHash == cmd.Grant.TokenHash {
			return opPlan{}, ClassInvalidRequest
		}
	}
	if mutation.kind == bindingComplete && mutation.proofHash == ([32]byte{}) {
		return opPlan{}, ClassInvalidRequest
	}
	if mutation.kind == bindingRenew {
		c := mutation.renewal
		if !validRoomCreateIdentity(c.BindingID) || c.Generation == 0 || c.Generation > math.MaxInt64 || !validRoomCreateIdentity(c.Grant.GrantID) || c.Grant.TokenHash == ([32]byte{}) {
			return opPlan{}, ClassInvalidRequest
		}
	}
	key := OpKey{Namespace: "binding_authority", Deployment: auth.Deployment, RoomID: roomID, Principal: auth.UserID, OpKind: mutation.kind, Identifier: clientID}
	return opPlan{auth: auth, userID: auth.UserID, key: key, binding: &mutation}, ""
}

func (s *Store) bindingRequestPlan(cmd BindingRequestCommand) (opPlan, string) {
	return s.bindingPlan(cmd.Auth, cmd.RoomID, cmd.ClientID, bindingMutation{kind: bindingRequest, requestID: cmd.RequestID, request: cmd})
}
func (s *Store) bindingDecisionPlan(cmd BindingDecisionCommand) (opPlan, string) {
	return s.bindingPlan(cmd.Auth, cmd.RoomID, cmd.ClientID, bindingMutation{kind: bindingDecide, requestID: cmd.RequestID, approve: cmd.Approve})
}
func (s *Store) bindingCompletionPlan(cmd BindingCompletionCommand) (opPlan, string) {
	return s.bindingPlan(cmd.Auth, cmd.RoomID, cmd.ClientID, bindingMutation{kind: bindingComplete, requestID: cmd.RequestID, proofHash: cmd.ProofHash})
}
func (s *Store) CommitBindingRequest(ctx context.Context, cmd BindingRequestCommand) (Result, error) {
	plan, class := s.bindingRequestPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}
func (s *Store) ResolveBindingRequest(ctx context.Context, cmd BindingRequestCommand) (Result, error) {
	plan, class := s.bindingRequestPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}
func (s *Store) DecideBindingRequest(ctx context.Context, cmd BindingDecisionCommand) (Result, error) {
	plan, class := s.bindingDecisionPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}
func (s *Store) ResolveBindingDecision(ctx context.Context, cmd BindingDecisionCommand) (Result, error) {
	plan, class := s.bindingDecisionPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}
func (s *Store) CompleteBindingRequest(ctx context.Context, cmd BindingCompletionCommand) (Result, error) {
	plan, class := s.bindingCompletionPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}
func (s *Store) ResolveBindingCompletion(ctx context.Context, cmd BindingCompletionCommand) (Result, error) {
	plan, class := s.bindingCompletionPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}
func (s *Store) ReadBindingRequest(ctx context.Context, auth AuthContext, roomID, requestID string) (BindingRequestInfo, error) {
	plan, class := s.bindingPlan(auth, roomID, requestID, bindingMutation{kind: bindingRead, requestID: requestID})
	if class != "" {
		return BindingRequestInfo{}, refusedError(class, nil)
	}
	result, err := s.resolveWithFallback(ctx, plan)
	if err != nil {
		return BindingRequestInfo{}, err
	}
	if result.BindingAuthority == nil || result.BindingAuthority.Request == nil {
		return BindingRequestInfo{}, refusedError(ClassStateInvalid, nil)
	}
	return *result.BindingAuthority.Request, nil
}
func (s *Store) ListBindingRequests(ctx context.Context, auth AuthContext, roomID string) ([]BindingRequestInfo, error) {
	plan, class := s.bindingPlan(auth, roomID, "list", bindingMutation{kind: bindingList})
	if class != "" {
		return nil, refusedError(class, nil)
	}
	result, err := s.resolveWithFallback(ctx, plan)
	return result.bindingRequests, err
}

func (s *Store) ReadBindingContext(ctx context.Context, auth AuthContext, roomID string) (BindingContext, error) {
	plan, class := s.bindingPlan(auth, roomID, "context", bindingMutation{kind: bindingContextRead})
	if class != "" {
		return BindingContext{}, refusedError(class, nil)
	}
	result, err := s.resolveWithFallback(ctx, plan)
	if err != nil {
		return BindingContext{}, err
	}
	if result.bindingContext == nil {
		return BindingContext{}, refusedError(ClassStateInvalid, nil)
	}
	return *result.bindingContext, nil
}

func (s *Store) bindingRenewalPlan(cmd BindingRenewalCommand) (opPlan, string) {
	return s.bindingPlan(cmd.Auth, cmd.RoomID, cmd.ClientID, bindingMutation{kind: bindingRenew, renewal: cmd})
}
func (s *Store) RenewBindingCredential(ctx context.Context, cmd BindingRenewalCommand) (Result, error) {
	plan, class := s.bindingRenewalPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}
func (s *Store) ResolveBindingRenewal(ctx context.Context, cmd BindingRenewalCommand) (Result, error) {
	plan, class := s.bindingRenewalPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}

// Credential rotation does not change a Device family's operation identity.
// The fenced transaction supplies that family; no request body may select it.
func bindingIntentHash(plan opPlan, d *bindingDependencies) [32]byte {
	m := plan.binding
	if m.kind == bindingRenew {
		c := m.renewal
		return controlHash(plan.key, struct {
			BindingID      string
			Generation     uint64
			GrantID        string
			Token          [32]byte
			Device, Family string
		}{c.BindingID, c.Generation, c.Grant.GrantID, c.Grant.TokenHash, d.credential.deviceID, d.credential.familyID})
	}
	if m.kind == bindingRequest {
		c := m.request
		c.Auth = AuthContext{}
		c.ClientID = ""
		return controlHash(plan.key, struct {
			Request        BindingRequestCommand
			Device, Family string
			Proof, Token   [32]byte
		}{c, d.credential.deviceID, d.credential.familyID, c.ProofHash, c.Grant.TokenHash})
	}
	return controlHash(plan.key, struct {
		RequestID      string
		Approve        bool
		Proof          [32]byte
		Device, Family string
	}{m.requestID, m.approve, m.proofHash, d.credential.deviceID, d.credential.familyID})
}
