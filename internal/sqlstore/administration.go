package sqlstore

import (
	"context"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

const controlAdministrationRead = "administration.read"

func administrationIdentity(key AdministrationIdentity) enterpriseauth.VerifiedIdentity {
	return enterpriseauth.VerifiedIdentity{ConnectionID: key.ConnectionID, Issuer: key.Issuer, Subject: key.Subject}
}

func validateAdministrationIntent(intent AdministrationIntent) bool {
	switch intent.Kind {
	case controlUserStatus, controlAdminSet:
		return validRoomCreateIdentity(intent.TargetUserID) && intent.Identity == (AdministrationIdentity{})
	case controlAdmission:
		return intent.TargetUserID == "" && validIdentity(administrationIdentity(intent.Identity))
	}
	return false
}

func (s *Store) administrationCommandAllowed(cmd AdministrationCommand) bool {
	return browserAuth(cmd.Auth) && cmd.Auth.Deployment == s.cfg.DeploymentID && validRoomCreateIdentity(cmd.Auth.UserID) && validRoomCreateIdentity(cmd.Auth.SessionID) && validRoomCreateIdentity(cmd.ClientID) && validateAdministrationIntent(cmd.Intent)
}

// CommitAdministration exposes existing typed operations; it neither adds an
// idempotency namespace nor begins a transaction around those operations.
func (s *Store) CommitAdministration(ctx context.Context, cmd AdministrationCommand) (Result, error) {
	if !s.administrationCommandAllowed(cmd) {
		return refused(ClassInvalidRequest, nil)
	}
	switch cmd.Intent.Kind {
	case controlUserStatus:
		return s.SetUserEnabled(ctx, cmd.Auth, cmd.ClientID, cmd.Intent.TargetUserID, cmd.Intent.Enabled)
	case controlAdminSet:
		return s.SetAdministrator(ctx, cmd.Auth, cmd.ClientID, cmd.Intent.TargetUserID, cmd.Intent.Enabled)
	case controlAdmission:
		return s.SetIdentityAdmission(ctx, cmd.Auth, cmd.ClientID, administrationIdentity(cmd.Intent.Identity), cmd.Intent.Enabled)
	}
	return refused(ClassUnsupportedOp, nil)
}

// ResolveAdministration reuses the original key and canonical builder. An
// unverified result never retries an administrator mutation.
func (s *Store) ResolveAdministration(ctx context.Context, cmd AdministrationCommand) (Result, error) {
	if !s.administrationCommandAllowed(cmd) {
		return refused(ClassInvalidRequest, nil)
	}
	var plan opPlan
	var class string
	if cmd.Intent.Kind == controlAdmission {
		plan, class = s.identityAdmissionPlan(cmd.Auth, cmd.ClientID, administrationIdentity(cmd.Intent.Identity), cmd.Intent.Enabled)
	} else {
		plan, class = s.humanControlPlan(cmd.Auth, cmd.ClientID, cmd.Intent.Kind, cmd.Intent.TargetUserID, cmd.Intent.Enabled)
	}
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}

func (s *Store) ReadAdministration(ctx context.Context, auth AuthContext, query AdministrationQuery) (AdministrationPage, error) {
	if !browserAuth(auth) || auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(auth.UserID) || !validRoomCreateIdentity(auth.SessionID) {
		return AdministrationPage{}, refusedError(ClassAuthorization, nil)
	}
	if query.Limit == 0 {
		query.Limit = 25
	}
	if _, err := decodeAdministrationCursor(query); err != nil {
		return AdministrationPage{}, refusedError(ClassInvalidRequest, err)
	}
	plan := opPlan{auth: auth, userID: auth.UserID, control: &controlMutation{kind: controlAdministrationRead, administration: &query}}
	result, err := s.resolveWithFallback(ctx, plan)
	if err != nil {
		return AdministrationPage{}, err
	}
	if result.administrationPage == nil {
		return AdministrationPage{}, refusedError(ClassStateInvalid, nil)
	}
	return *result.administrationPage, nil
}
