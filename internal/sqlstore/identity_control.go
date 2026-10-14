package sqlstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/model"
)

const (
	controlConfigure    = "identity.configure"
	controlLogin        = "identity.login"
	controlRevoke       = "session.revoke"
	controlAuthenticate = "session.authenticate"
	controlReadiness    = "identity.readiness"
	controlUserStatus   = "user.status"
	controlAdminSet     = "administrator.set"
	controlAdmission    = "identity.admission"
)

type controlMutation struct {
	kind                 string
	config               *IdentityConfig
	login                *IdentityLoginCommand
	tokenHash            [32]byte
	newUserID            string
	targetUser           string
	enabled              bool
	connectionID, issuer string
	expectedOrgID        string
	identity             *enterpriseauth.VerifiedIdentity
	device               *deviceMutation
	administration       *AdministrationQuery
}

type controlDependencies struct {
	missing, orgDisabled                       bool
	orgID, restoreEpoch                        string
	revision                                   uint64
	configHash                                 [32]byte
	bootstrap                                  enterpriseauth.VerifiedIdentity
	consumedBy                                 string
	identityUser                               string
	identityRetired                            bool
	admitted, connectionKnown                  bool
	actorMissing, actorDisabled, actorAdmin    bool
	targetMissing, targetDisabled, targetAdmin bool
	credential                                 sessionDependency
	candidate                                  sessionDependency
	device                                     deviceDependencies
	revocationDigest                           [32]byte
}

func equalControlDependencies(a, b *controlDependencies) bool {
	if a == nil || b == nil {
		return a == b
	}
	left, right := *a, *b
	left.revision, right.revision = 0, 0
	return left == right
}

// InitializeIdentity is an operator-only startup operation. It is deliberately
// not an authenticated management route. An identical replay returns its old
// receipt before any setup writes, so later revocations cannot be undone.
func (s *Store) InitializeIdentity(ctx context.Context, config IdentityConfig) (Result, error) {
	frozen, err := freezeIdentityConfig(config)
	if err != nil {
		return refused(ClassInvalidRequest, err)
	}
	plan := opPlan{key: OpKey{Namespace: "operator", Deployment: s.cfg.DeploymentID, OpKind: controlConfigure, Identifier: "identity-configuration-v1", Principal: "configured-operator"}, control: &controlMutation{kind: controlConfigure, config: &frozen}}
	plan.hash = controlHash(plan.key, frozen)
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) AdmitIdentity(ctx context.Context, cmd IdentityLoginCommand) (Result, error) {
	plan, class := s.loginPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) ResolveIdentityLogin(ctx context.Context, cmd IdentityLoginCommand) (Result, error) {
	plan, class := s.loginPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}

func (s *Store) loginPlan(cmd IdentityLoginCommand) (opPlan, string) {
	if !validRoomCreateIdentity(cmd.ClientID) || !validIdentity(cmd.Completion.Identity) || cmd.Completion.Purpose != enterpriseauth.PurposeLogin || cmd.Completion.FreshReauthentication || cmd.Completion.CorrelationID != "" || !validRoomCreateIdentity(cmd.Session.SessionID) || cmd.Session.TokenHash == ([32]byte{}) || cmd.Session.CSRFHash == ([32]byte{}) || cmd.Session.ExpiresAt.IsZero() || cmd.Session.ExpiresAt.Year() < 1970 || cmd.Session.ExpiresAt.Year() > 9999 || (!cmd.Completion.Identity.AuthTime.IsZero() && (cmd.Completion.Identity.AuthTime.Year() < 1970 || cmd.Completion.Identity.AuthTime.Year() > 9999)) {
		return opPlan{}, ClassInvalidRequest
	}
	identity := identityTuple(cmd.Completion.Identity)
	principal := sha256.Sum256([]byte(identity.ConnectionID + "\x00" + identity.Issuer + "\x00" + identity.Subject))
	uid, err := controlID("user")
	if err != nil {
		return opPlan{}, ClassStateInvalid
	}
	plan := opPlan{key: OpKey{Namespace: "oidc", Deployment: s.cfg.DeploymentID, OpKind: controlLogin, Identifier: cmd.ClientID, Principal: hex.EncodeToString(principal[:])}, control: &controlMutation{kind: controlLogin, login: &cmd, newUserID: uid}}
	// Hash verifiers directly; BrowserSessionCandidate deliberately omits them
	// from JSON to prevent accidental diagnostic or receipt serialization.
	plan.hash = controlHash(plan.key, struct {
		Identity            enterpriseauth.VerifiedIdentity
		SessionID           string
		TokenHash, CSRFHash [32]byte
		ExpiresAt           time.Time
	}{cmd.Completion.Identity, cmd.Session.SessionID, cmd.Session.TokenHash, cmd.Session.CSRFHash, cmd.Session.ExpiresAt.UTC()})
	return plan, ""
}

func (s *Store) RevokeSession(ctx context.Context, auth AuthContext, clientID string) (Result, error) {
	plan, class := s.humanControlPlan(auth, clientID, controlRevoke, auth.UserID, false)
	if class != "" || auth.SessionID == "" {
		return refused(ClassInvalidRequest, nil)
	}
	plan.hash = controlHash(plan.key, auth.SessionID)
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) SetUserEnabled(ctx context.Context, auth AuthContext, clientID, targetUserID string, enabled bool) (Result, error) {
	plan, class := s.humanControlPlan(auth, clientID, controlUserStatus, targetUserID, enabled)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) SetAdministrator(ctx context.Context, auth AuthContext, clientID, targetUserID string, enabled bool) (Result, error) {
	plan, class := s.humanControlPlan(auth, clientID, controlAdminSet, targetUserID, enabled)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

// SetIdentityAdmission changes the explicit login-admission list. It grants
// no administrator role or Room access, and does not change an existing
// User's status. SetUserEnabled is the separate immediate revocation action.
func (s *Store) SetIdentityAdmission(ctx context.Context, auth AuthContext, clientID string, identity enterpriseauth.VerifiedIdentity, enabled bool) (Result, error) {
	plan, class := s.identityAdmissionPlan(auth, clientID, identity, enabled)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) identityAdmissionPlan(auth AuthContext, clientID string, identity enterpriseauth.VerifiedIdentity, enabled bool) (opPlan, string) {
	if !validIdentity(identity) {
		return opPlan{}, ClassInvalidRequest
	}
	plan, class := s.humanControlPlan(auth, clientID, controlAdmission, auth.UserID, enabled)
	if class != "" {
		return opPlan{}, class
	}
	identity = identityTuple(identity)
	plan.control.identity = &identity
	plan.control.targetUser = ""
	plan.hash = controlHash(plan.key, struct {
		Identity enterpriseauth.VerifiedIdentity
		Enabled  bool
	}{identity, enabled})
	return plan, ""
}

func (s *Store) humanControlPlan(auth AuthContext, clientID, kind, target string, enabled bool) (opPlan, string) {
	if auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(auth.UserID) || !validRoomCreateIdentity(clientID) || !validRoomCreateIdentity(target) {
		return opPlan{}, ClassInvalidRequest
	}
	plan := opPlan{auth: auth, userID: auth.UserID, key: OpKey{Namespace: namespaceHuman, Deployment: s.cfg.DeploymentID, OpKind: kind, Identifier: clientID, Principal: auth.UserID}, control: &controlMutation{kind: kind, targetUser: target, enabled: enabled}}
	plan.hash = controlHash(plan.key, struct {
		Target  string
		Enabled bool
	}{target, enabled})
	return plan, ""
}

func identityTuple(identity enterpriseauth.VerifiedIdentity) enterpriseauth.VerifiedIdentity {
	identity.AuthTime = time.Time{}
	return identity
}

// ValidateIdentityConfig performs the same pure validation used by operator
// initialization, allowing CLI policy failures to be rejected before DDL.
func ValidateIdentityConfig(config IdentityConfig) error {
	_, err := freezeIdentityConfig(config)
	return err
}

func validIdentity(identity enterpriseauth.VerifiedIdentity) bool {
	if !validRoomCreateIdentity(identity.ConnectionID) || len(identity.ConnectionID) > 128 || identity.Subject == "" || len(identity.Subject) > 1024 || !utf8.ValidString(identity.Subject) || strings.ContainsFunc(identity.Subject, unicode.IsControl) {
		return false
	}
	u, err := url.Parse(identity.Issuer)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && len(identity.Issuer) <= 2048 && utf8.ValidString(identity.Issuer)
}

func freezeIdentityConfig(config IdentityConfig) (IdentityConfig, error) {
	if len(config.Connections) == 0 || len(config.Connections) > 16 || len(config.Admitted) > 1000 || !validIdentity(config.Bootstrap) {
		return IdentityConfig{}, errors.New("explicit trust connections and bootstrap identity are required")
	}
	config.Connections = append([]TrustConnectionConfig(nil), config.Connections...)
	config.Admitted = append([]enterpriseauth.VerifiedIdentity(nil), config.Admitted...)
	config.Bootstrap = identityTuple(config.Bootstrap)
	sort.Slice(config.Connections, func(i, j int) bool { return config.Connections[i].ConnectionID < config.Connections[j].ConnectionID })
	connections := make(map[string]string, len(config.Connections))
	for _, connection := range config.Connections {
		if !validIdentity(enterpriseauth.VerifiedIdentity{ConnectionID: connection.ConnectionID, Issuer: connection.Issuer, Subject: "validation"}) || connections[connection.ConnectionID] != "" {
			return IdentityConfig{}, errors.New("invalid or duplicate trust connection")
		}
		connections[connection.ConnectionID] = connection.Issuer
	}
	if connections[config.Bootstrap.ConnectionID] != config.Bootstrap.Issuer {
		return IdentityConfig{}, errors.New("bootstrap identity does not match an explicit trust connection")
	}
	for i, identity := range config.Admitted {
		if !validIdentity(identity) || connections[identity.ConnectionID] != identity.Issuer {
			return IdentityConfig{}, errors.New("admitted identity does not match an explicit trust connection")
		}
		config.Admitted[i] = identityTuple(identity)
	}
	sort.Slice(config.Admitted, func(i, j int) bool {
		a, b := config.Admitted[i], config.Admitted[j]
		return a.ConnectionID+"\x00"+a.Issuer+"\x00"+a.Subject < b.ConnectionID+"\x00"+b.Issuer+"\x00"+b.Subject
	})
	return config, nil
}

func controlHash(key OpKey, intent any) [32]byte {
	h := sha256.New()
	for _, part := range []string{"sqlstore.control.v1", key.Namespace, key.Deployment, key.OpKind, key.Identifier, key.Principal} {
		writePart(h, []byte(part))
	}
	encoded, _ := json.Marshal(intent)
	writePart(h, encoded)
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func controlID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(random[:]), nil
}

func (s *Store) readControlDependencies(ctx context.Context, q rowQuerier, plan opPlan, lock string) (*controlDependencies, error) {
	d := &controlDependencies{}
	var configHash []byte
	err := q.QueryRow(ctx, `SELECT config_hash, bootstrap_connection, bootstrap_issuer, bootstrap_subject, COALESCE(bootstrap_consumed_by,''),restore_epoch,revision FROM `+s.table("identity_policy")+` WHERE id`).Scan(&configHash, &d.bootstrap.ConnectionID, &d.bootstrap.Issuer, &d.bootstrap.Subject, &d.consumedBy, &d.restoreEpoch, &d.revision)
	if errors.Is(err, pgx.ErrNoRows) {
		d.missing = true
		err = nil
	} else if err != nil {
		return nil, err
	} else {
		if len(configHash) != 32 {
			return nil, errors.New("invalid identity policy hash")
		}
		copy(d.configHash[:], configHash)
	}
	if err := q.QueryRow(ctx, `SELECT d.org_id,o.disabled FROM `+s.table("deployment")+` d JOIN `+s.table("organizations")+` o USING(org_id) WHERE d.deployment_id=$1`, s.cfg.DeploymentID).Scan(&d.orgID, &d.orgDisabled); err != nil {
		return nil, err
	}
	if plan.control.device != nil {
		return s.readDeviceControlDependencies(ctx, q, plan, lock, d)
	}
	if plan.control.kind == controlReadiness {
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("trust_connections")+` WHERE connection_id=$1 AND issuer=$2)`, plan.control.connectionID, plan.control.issuer).Scan(&d.connectionKnown); err != nil {
			return nil, err
		}
	}
	if plan.control.kind == controlAdmission {
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("trust_connections")+` WHERE connection_id=$1 AND issuer=$2)`, plan.control.identity.ConnectionID, plan.control.identity.Issuer).Scan(&d.connectionKnown); err != nil {
			return nil, err
		}
		identity := plan.control.identity
		err := q.QueryRow(ctx, `SELECT user_id FROM `+s.table("external_identities")+` WHERE connection_id=$1 AND issuer=$2 AND subject=$3 AND retired_at IS NULL`, identity.ConnectionID, identity.Issuer, identity.Subject).Scan(&d.identityUser)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	actor := plan.auth.UserID
	if plan.control.kind == controlLogin {
		identity := plan.control.login.Completion.Identity
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+s.table("trust_connections")+` WHERE connection_id=$1 AND issuer=$2), EXISTS(SELECT 1 FROM `+s.table("identity_admissions")+` WHERE connection_id=$1 AND issuer=$2 AND subject=$3 AND enabled)`, identity.ConnectionID, identity.Issuer, identity.Subject).Scan(&d.connectionKnown, &d.admitted); err != nil {
			return nil, err
		}
		err := q.QueryRow(ctx, `SELECT user_id,retired_at IS NOT NULL FROM `+s.table("external_identities")+` WHERE connection_id=$1 AND issuer=$2 AND subject=$3`, identity.ConnectionID, identity.Issuer, identity.Subject).Scan(&d.identityUser, &d.identityRetired)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		actor = d.identityUser
	}
	if plan.control.kind == controlAuthenticate {
		err := q.QueryRow(ctx, `SELECT session_id,user_id FROM `+s.table("browser_sessions")+` WHERE token_hash=$1`, plan.control.tokenHash[:]).Scan(&d.credential.sessionID, &actor)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	target := plan.control.targetUser
	if plan.control.kind == controlAdmission {
		target = d.identityUser
	}
	ids := []string{}
	if actor != "" {
		ids = append(ids, actor)
	}
	if target != "" && target != actor {
		ids = append(ids, target)
	}
	sort.Strings(ids)
	for _, id := range ids {
		userLock := lock
		if lock != "" && id == plan.control.targetUser && (plan.control.kind == controlUserStatus || plan.control.kind == controlAdminSet) {
			userLock = lockExclusive
		}
		var disabled bool
		err := q.QueryRow(ctx, `SELECT disabled FROM `+s.table("users")+` WHERE user_id=$1`+userLock, id).Scan(&disabled)
		missing := errors.Is(err, pgx.ErrNoRows)
		if err != nil && !missing {
			return nil, err
		}
		if id == actor {
			d.actorMissing, d.actorDisabled = missing, disabled
		}
		if id == target {
			d.targetMissing, d.targetDisabled = missing, disabled
		}
	}
	if actor == "" {
		d.actorMissing = true
	}
	for _, id := range ids {
		var enabled bool
		roleLock := lock
		if lock != "" && id == plan.control.targetUser && plan.control.kind == controlAdminSet {
			roleLock = lockExclusive
		}
		err := q.QueryRow(ctx, `SELECT enabled FROM `+s.table("organization_admins")+` WHERE user_id=$1`+roleLock, id).Scan(&enabled)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if id == actor {
			d.actorAdmin = enabled
		}
		if id == target {
			d.targetAdmin = enabled
		}
	}
	if plan.control.kind == controlAuthenticate {
		d.credential, err = s.readCredential(ctx, q, AuthContext{Deployment: s.cfg.DeploymentID, UserID: actor, SessionID: d.credential.sessionID}, lock)
		// Authentication always requires a credential, even on a trusted-only
		// Store whose legacy internal Room fixtures omit SessionID.
		d.credential.required = true
	} else if plan.control.kind == controlLogin {
		d.candidate, err = s.readCredential(ctx, q, AuthContext{Deployment: s.cfg.DeploymentID, UserID: actor, SessionID: plan.control.login.Session.SessionID}, lock)
	} else if plan.control.kind != controlConfigure && plan.control.kind != controlReadiness {
		if lock != "" {
			// Pre-lock every affected session at its strongest mode, sorted by
			// ID. No status/role mutation upgrades its caller credential or
			// discovers more target session locks after the consistency guard.
			if err := s.lockControlSessions(ctx, q, plan); err != nil {
				return nil, err
			}
		}
		d.credential, err = s.readCredential(ctx, q, plan.auth, lockNone)
	}
	if err != nil {
		return nil, err
	}
	if plan.control.kind == controlAdmission {
		admissionLock := lockNone
		if lock != lockNone {
			admissionLock = lockExclusive
		}
		identity := plan.control.identity
		err := q.QueryRow(ctx, `SELECT enabled FROM `+s.table("identity_admissions")+` WHERE connection_id=$1 AND issuer=$2 AND subject=$3`+admissionLock, identity.ConnectionID, identity.Issuer, identity.Subject).Scan(&d.admitted)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	if plan.control.kind == controlUserStatus && !plan.control.enabled {
		d.revocationDigest, err = s.readAuthorityRevocation(ctx, q, authorityRevocationScope{UserID: plan.control.targetUser}, lock)
		if err != nil {
			return nil, err
		}
	}
	if lock != "" && !d.missing {
		stateLock := lockExclusive
		if controlReadOnly(plan.control.kind) {
			stateLock = lockShare
		}
		// Global control's consistency row occupies the same phase as the Room
		// consistency guard. The ownership lock already excludes phantoms.
		if err := q.QueryRow(ctx, `SELECT revision FROM `+s.table("identity_policy")+` WHERE id`+stateLock).Scan(&d.revision); err != nil {
			return nil, err
		}
	}
	return d, nil
}

func (s *Store) controlReadAllowed(plan opPlan, d *controlDependencies) bool {
	if plan.control.device != nil {
		return s.deviceReadAllowed(plan, d)
	}
	if plan.auth.DeviceGrantID != "" || plan.auth.BindingGrantID != "" {
		return false
	}
	switch plan.control.kind {
	case controlConfigure:
		return true
	case controlReadiness:
		return !d.missing && !d.orgDisabled && d.connectionKnown && d.orgID == plan.control.expectedOrgID
	case controlLogin:
		return !d.missing && !d.orgDisabled && d.connectionKnown && d.admitted && !d.identityRetired && (d.identityUser == "" || !d.actorMissing && !d.actorDisabled)
	case controlUserStatus, controlAdminSet, controlAdmission, controlAdministrationRead:
		return !d.missing && !d.orgDisabled && !d.actorMissing && !d.actorDisabled && d.actorAdmin && d.credential.allowed()
	default:
		return !d.missing && !d.orgDisabled && !d.actorMissing && !d.actorDisabled && d.credential.allowed()
	}
}

func (s *Store) lookupControl(ctx context.Context, q rowQuerier, plan opPlan) (Result, bool, error) {
	var hash, resultHash, encoding []byte
	var seq uint64
	var version int
	var kind string
	err := q.QueryRow(ctx, `SELECT o.intent_hash,o.result_hash,e.seq,e.codec_version,e.kind,e.receipt_encoding FROM `+s.table("control_operations")+` o JOIN `+s.table("control_events")+` e ON e.seq=o.result_seq WHERE o.op_namespace=$1 AND o.deployment_id=$2 AND o.op_kind=$3 AND o.identifier=$4 AND o.principal=$5`, plan.key.Namespace, plan.key.Deployment, plan.key.OpKind, plan.key.Identifier, plan.key.Principal).Scan(&hash, &resultHash, &seq, &version, &kind, &encoding)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(hash, plan.hash[:]) {
		result, err := controlConflict()
		return result, true, err
	}
	var receipt ControlReceipt
	decoder := json.NewDecoder(bytes.NewReader(encoding))
	decoder.DisallowUnknownFields()
	digest := sha256.Sum256(encoding)
	if version != 1 || !bytes.Equal(digest[:], resultHash) || decoder.Decode(&receipt) != nil || receipt.Version != 1 || receipt.Seq != seq || receipt.Kind != kind || kind != plan.control.kind {
		return Result{}, true, &storedFactCorruption{err: errors.New("invalid stored control receipt")}
	}
	canonical, encodeErr := json.Marshal(receipt)
	if encodeErr != nil || !bytes.Equal(canonical, encoding) {
		// The trusted producer writes this canonical closed struct. This also
		// rejects duplicate/case-aliased fields and trailing JSON on replay.
		return Result{}, true, &storedFactCorruption{err: errors.New("noncanonical stored control receipt")}
	}
	if plan.control.kind == controlLogin && (receipt.SessionID != plan.control.login.Session.SessionID || !receipt.ExpiresAt.Equal(plan.control.login.Session.ExpiresAt)) {
		return Result{}, true, &storedFactCorruption{err: errors.New("stored login receipt disagrees with its candidate")}
	}
	return Result{Outcome: OutcomeReplayed, Control: &receipt}, true, nil
}

func controlConflict() (Result, error) {
	err := &KernelError{Outcome: OutcomeConflict, Class: ClassPayloadConflict}
	return Result{Outcome: OutcomeConflict, Class: ClassPayloadConflict, Err: err}, err
}

func (s *Store) resolveControlPlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *controlDependencies) (Result, error) {
	if !s.controlReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	if plan.control.administration != nil {
		return s.resolveAdministrationDirectory(ctx, tx, plan, d)
	}
	if plan.control.device != nil {
		return s.resolveDeviceControl(ctx, tx, plan, d)
	}
	if plan.control.kind == controlReadiness {
		return Result{Outcome: OutcomeReplayed}, nil
	}
	if plan.control.kind == controlAuthenticate {
		return Result{Outcome: OutcomeReplayed, session: &BrowserSession{Auth: AuthContext{Deployment: s.cfg.DeploymentID, UserID: d.credential.userID, SessionID: d.credential.sessionID}, CSRFHash: d.credential.csrfHash, ExpiresAt: d.credential.expiresAt, Administrator: d.actorAdmin}}, nil
	}
	result, found, err := s.lookupControl(ctx, tx, plan)
	if err != nil {
		if isStoredFactCorruption(err) {
			return refused(ClassStoredFactUnreadable, err)
		}
		var kerr *KernelError
		if errors.As(err, &kerr) {
			return result, err
		}
		return resolveIncomplete(ctx, err)
	}
	if !found {
		return Result{Outcome: OutcomeNotCommitted}, nil
	}
	if plan.control.kind == controlLogin && !d.candidate.allowed() {
		return refused(ClassAuthorization, nil)
	}
	return result, nil
}

func (s *Store) applyControlPlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *controlDependencies) (Result, error) {
	if !s.controlReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	if plan.control.device != nil {
		return s.applyDeviceControl(ctx, tx, plan, d)
	}
	result, found, err := s.lookupControl(ctx, tx, plan)
	if err != nil {
		if isStoredFactCorruption(err) {
			return refused(ClassStoredFactUnreadable, err)
		}
		var kerr *KernelError
		if errors.As(err, &kerr) {
			return result, err
		}
		return controlStatementFailure(s, ctx, err)
	}
	if found {
		if plan.control.kind == controlLogin && !d.candidate.allowed() {
			return refused(ClassAuthorization, nil)
		}
		return result, nil
	}
	if d.revision >= math.MaxInt64 {
		return refused(ClassRangeExceeded, nil)
	}
	receipt := ControlReceipt{Version: 1, Kind: plan.control.kind, Seq: d.revision + 1}
	actor := plan.auth.UserID
	switch plan.control.kind {
	case controlConfigure:
		if !d.missing {
			return refused(ClassStateInvalid, errors.New("identity policy already exists without its setup receipt"))
		}
		if err := s.writeIdentityConfiguration(ctx, tx, plan, d); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
	case controlLogin:
		now := time.Now().UTC()
		candidate := plan.control.login.Session
		if !candidate.ExpiresAt.After(now) || candidate.ExpiresAt.After(now.Add(24*time.Hour)) || !d.candidate.missing {
			return refused(ClassInvalidRequest, nil)
		}
		identity := plan.control.login.Completion.Identity
		actor = d.identityUser
		additional := int64(1)
		if actor == "" {
			additional++
		}
		if e := s.checkActionableAdmission(ctx, tx, additional); e != nil {
			return resourceAdmissionFailure(s, ctx, e)
		}
		if actor == "" {
			if e := s.checkPersistentAdmission(ctx, tx, "users", 1); e != nil {
				return resourceAdmissionFailure(s, ctx, e)
			}
			actor = plan.control.newUserID
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("users")+`(user_id,org_id) VALUES($1,$2)`, actor, d.orgID); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
			identityID, err := controlID("identity")
			if err != nil {
				return refused(ClassStateInvalid, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("external_identities")+`(identity_id,org_id,connection_id,issuer,subject,user_id) VALUES($1,$2,$3,$4,$5,$6)`, identityID, d.orgID, identity.ConnectionID, identity.Issuer, identity.Subject, actor); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
		}
		administrator := d.actorAdmin
		if d.consumedBy == "" && identityTuple(identity) == d.bootstrap {
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("organization_admins")+`(user_id) VALUES($1)`, actor); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
			if _, err := tx.Exec(ctx, `UPDATE `+s.table("identity_policy")+` SET bootstrap_consumed_by=$1 WHERE id`, actor); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
			administrator = true
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("browser_sessions")+`(session_id,user_id,token_hash,csrf_hash,restore_epoch,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, candidate.SessionID, actor, candidate.TokenHash[:], candidate.CSRFHash[:], d.restoreEpoch, candidate.ExpiresAt, now); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		receipt.UserID, receipt.SessionID, receipt.ExpiresAt, receipt.Administrator = actor, candidate.SessionID, candidate.ExpiresAt.UTC(), administrator
	case controlRevoke:
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("browser_sessions")+` SET revoked_at=COALESCE(revoked_at,pg_catalog.clock_timestamp()) WHERE session_id=$1`, plan.auth.SessionID); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		receipt.UserID, receipt.SessionID = actor, plan.auth.SessionID
	case controlAdmission:
		if !d.connectionKnown {
			return refused(ClassInvalidRequest, nil)
		}
		identity := plan.control.identity
		if !plan.control.enabled && d.targetAdmin && !d.targetDisabled {
			remaining, err := s.countLoginEligibleAdmins(ctx, tx, "", identity)
			if err != nil {
				return controlStatementFailure(s, ctx, err)
			}
			if remaining == 0 {
				return refused(ClassLastAdministrator, nil)
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("identity_admissions")+`(connection_id,issuer,subject,enabled) VALUES($1,$2,$3,$4) ON CONFLICT(connection_id,issuer,subject) DO UPDATE SET enabled=EXCLUDED.enabled`, identity.ConnectionID, identity.Issuer, identity.Subject, plan.control.enabled); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		hash := sha256.Sum256([]byte(identity.ConnectionID + "\x00" + identity.Issuer + "\x00" + identity.Subject))
		receipt.UserID, receipt.IdentityHash, receipt.Enabled = actor, hex.EncodeToString(hash[:]), plan.control.enabled
	case controlUserStatus, controlAdminSet:
		if !d.actorAdmin || d.targetMissing {
			return refused(ClassAuthorization, nil)
		}
		if plan.control.kind == controlUserStatus && plan.control.enabled && d.targetDisabled {
			if e := s.checkActionableAdmission(ctx, tx, 1); e != nil {
				return resourceAdmissionFailure(s, ctx, e)
			}
		}
		if d.targetAdmin && !d.targetDisabled && !plan.control.enabled {
			admins, err := s.countLoginEligibleAdmins(ctx, tx, plan.control.targetUser, nil)
			if err != nil {
				return controlStatementFailure(s, ctx, err)
			}
			if admins == 0 {
				return refused(ClassLastAdministrator, nil)
			}
		}
		if plan.control.kind == controlUserStatus && !plan.control.enabled {
			scope := authorityRevocationScope{UserID: plan.control.targetUser}
			if err := s.retireAuthorityBindings(ctx, tx, scope, actor); err != nil {
				if isStoredFactCorruption(err) {
					return refused(ClassStoredFactUnreadable, err)
				}
				return controlStatementFailure(s, ctx, err)
			}
			count, err := s.revokeNativeCredentials(ctx, tx, scope)
			if err != nil {
				return controlStatementFailure(s, ctx, err)
			}
			setAuditInvalidations(plan, count)
		}
		if plan.control.kind == controlUserStatus {
			if _, err := tx.Exec(ctx, `UPDATE `+s.table("users")+` SET disabled=$2 WHERE user_id=$1`, plan.control.targetUser, !plan.control.enabled); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
		} else {
			if d.targetDisabled && plan.control.enabled {
				return refused(ClassInvalidControlState, nil)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("organization_admins")+`(user_id,enabled) VALUES($1,$2) ON CONFLICT(user_id) DO UPDATE SET enabled=EXCLUDED.enabled`, plan.control.targetUser, plan.control.enabled); err != nil {
				return controlStatementFailure(s, ctx, err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE `+s.table("browser_sessions")+` SET revoked_at=pg_catalog.clock_timestamp() WHERE user_id=$1 AND revoked_at IS NULL AND expires_at>pg_catalog.clock_timestamp()`, plan.control.targetUser); err != nil {
			return controlStatementFailure(s, ctx, err)
		}
		receipt.UserID, receipt.TargetUserID, receipt.Enabled = actor, plan.control.targetUser, plan.control.enabled
	default:
		return refused(ClassUnsupportedOp, nil)
	}
	if err := s.appendControlResult(ctx, tx, plan, receipt, actor); err != nil {
		return controlStatementFailure(s, ctx, err)
	}
	return Result{Outcome: OutcomeCommitted, Control: &receipt}, nil
}

func (s *Store) countLoginEligibleAdmins(ctx context.Context, q rowQuerier, excludeUser string, excludeIdentity *enterpriseauth.VerifiedIdentity) (int, error) {
	var connection, issuer, subject string
	if excludeIdentity != nil {
		connection, issuer, subject = excludeIdentity.ConnectionID, excludeIdentity.Issuer, excludeIdentity.Subject
	}
	var count int
	err := q.QueryRow(ctx, `SELECT count(*) FROM `+s.table("organization_admins")+` a JOIN `+s.table("users")+` u USING(user_id) WHERE a.enabled AND NOT u.disabled AND u.user_id<>$1 AND EXISTS(SELECT 1 FROM `+s.table("external_identities")+` i JOIN `+s.table("identity_admissions")+` x USING(connection_id,issuer,subject) WHERE i.user_id=u.user_id AND i.retired_at IS NULL AND x.enabled AND NOT (i.connection_id=$2 AND i.issuer=$3 AND i.subject=$4))`, excludeUser, connection, issuer, subject).Scan(&count)
	return count, err
}

func controlStatementFailure(s *Store, ctx context.Context, err error) (Result, error) {
	result, classified, _ := s.postBeginFailure(ctx, err)
	return result, classified
}

func (s *Store) writeIdentityConfiguration(ctx context.Context, tx pgx.Tx, plan opPlan, d *controlDependencies) error {
	config := plan.control.config
	for _, connection := range config.Connections {
		var issuer string
		err := tx.QueryRow(ctx, `SELECT issuer FROM `+s.table("trust_connections")+` WHERE connection_id=$1`, connection.ConnectionID).Scan(&issuer)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("trust_connections")+`(connection_id,org_id,issuer) VALUES($1,$2,$3)`, connection.ConnectionID, d.orgID, connection.Issuer); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if issuer != connection.Issuer {
			return errors.New("trust connection issuer is immutable")
		}
	}
	epoch, err := controlID("restore")
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("identity_policy")+`(config_hash,bootstrap_connection,bootstrap_issuer,bootstrap_subject,restore_epoch) VALUES($1,$2,$3,$4,$5)`, plan.hash[:], config.Bootstrap.ConnectionID, config.Bootstrap.Issuer, config.Bootstrap.Subject, epoch); err != nil {
		return err
	}
	identities := append(append([]enterpriseauth.VerifiedIdentity(nil), config.Admitted...), config.Bootstrap)
	for _, identity := range identities {
		if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("identity_admissions")+`(connection_id,issuer,subject) VALUES($1,$2,$3) ON CONFLICT(connection_id,issuer,subject) DO NOTHING`, identity.ConnectionID, identity.Issuer, identity.Subject); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appendControlResult(ctx context.Context, tx pgx.Tx, plan opPlan, receipt ControlReceipt, actor string) error {
	encoding, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	id, err := controlID("control")
	if err != nil {
		return err
	}
	var actorValue any = actor
	actorKind := "user"
	if actor == "" {
		actorValue = nil
		actorKind = "operator"
		if plan.control.device != nil {
			actorKind = "client"
		}
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("control_events")+`(seq,event_id,codec_version,kind,actor_user_id,receipt_encoding,created_at) VALUES($1,$2,1,$3,$4,$5,$6)`, int64(receipt.Seq), id, receipt.Kind, actorValue, encoding, now); err != nil {
		return err
	}
	resultHash := sha256.Sum256(encoding)
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("control_operations")+`(op_namespace,deployment_id,op_kind,identifier,principal,intent_hash,result_hash,result_seq,committed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, plan.key.Namespace, plan.key.Deployment, plan.key.OpKind, plan.key.Identifier, plan.key.Principal, plan.hash[:], resultHash[:], int64(receipt.Seq), now); err != nil {
		return err
	}
	if err := s.writeMutationAudit(ctx, tx, plan, "", actor, actorKind, receipt.Kind, "accepted", "", controlAuditTarget(receipt), auditCount(plan), now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE `+s.table("identity_policy")+` SET revision=$1 WHERE id`, int64(receipt.Seq))
	return err
}

func (s *Store) lockControlSessions(ctx context.Context, q rowQuerier, plan opPlan) error {
	locks := make(map[string]string)
	if plan.auth.SessionID != "" {
		locks[plan.auth.SessionID] = lockShare
	}
	if plan.control.kind == controlRevoke {
		locks[plan.auth.SessionID] = lockExclusive
	}
	if plan.control.kind == controlUserStatus || plan.control.kind == controlAdminSet {
		var sessions []string
		if err := q.QueryRow(ctx, `SELECT COALESCE(array_agg(session_id ORDER BY session_id),ARRAY[]::text[]) FROM `+s.table("browser_sessions")+` WHERE user_id=$1 AND revoked_at IS NULL AND expires_at>pg_catalog.clock_timestamp()`, plan.control.targetUser).Scan(&sessions); err != nil {
			return err
		}
		for _, session := range sessions {
			locks[session] = lockExclusive
		}
	}
	ids := make([]string, 0, len(locks))
	for id := range locks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		var found string
		err := q.QueryRow(ctx, `SELECT session_id FROM `+s.table("browser_sessions")+` WHERE session_id=$1`+locks[id], id).Scan(&found)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	return nil
}
