package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/model"
)

type MembershipAction = model.EnterpriseMembershipAction
type RoomRole = model.EnterpriseRoomRole
type RoomMembershipIntent = model.EnterpriseMembershipIntent

const (
	MembershipAdmit      = model.EnterpriseMembershipAdmit
	MembershipChangeRole = model.EnterpriseMembershipChangeRole
	MembershipRemove     = model.EnterpriseMembershipRemove
	RoomMember           = model.EnterpriseRoomMember
	RoomViewer           = model.EnterpriseRoomViewer
)

// RoomMembershipCommand changes one admitted Room's control membership. The
// actor is always the authenticated human. Ownership transfer, administrator
// authority and slot retirement cannot be expressed through this intent.
type RoomMembershipCommand struct {
	Auth             AuthContext
	RoomID           string
	ClientID         string
	ExpectedRevision uint64
	Intent           RoomMembershipIntent
}

func (s *Store) CommitRoomMembership(ctx context.Context, cmd RoomMembershipCommand) (Result, error) {
	plan, class := s.roomMembershipPlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) ResolveRoomMembership(ctx context.Context, auth AuthContext, roomID, clientID string, intent RoomMembershipIntent) (Result, error) {
	plan, class := s.roomMembershipPlan(RoomMembershipCommand{Auth: auth, RoomID: roomID, ClientID: clientID, Intent: intent})
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}

func (s *Store) roomMembershipPlan(cmd RoomMembershipCommand) (opPlan, string) {
	if cmd.Auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(cmd.Auth.Deployment) || !validRoomCreateIdentity(cmd.Auth.UserID) || !validRoomCreateIdentity(cmd.RoomID) || !validRoomCreateIdentity(cmd.ClientID) || cmd.ExpectedRevision > math.MaxInt64 {
		return opPlan{}, ClassInvalidRequest
	}
	if err := model.ValidateEnterpriseMembershipIntent(cmd.Intent); err != nil {
		return opPlan{}, ClassInvalidRequest
	}
	key := OpKey{Namespace: namespaceHuman, Deployment: cmd.Auth.Deployment, RoomID: cmd.RoomID,
		OpKind: opKindRoomMembership, Identifier: cmd.ClientID, Principal: cmd.Auth.UserID}
	return opPlan{auth: cmd.Auth, key: key, userID: cmd.Auth.UserID, expectedRevision: cmd.ExpectedRevision,
		audit: AuditRecord{Kind: "room.membership." + string(cmd.Intent.Action), Outcome: auditOutcomeAccepted},
		hash:  roomMembershipHash(key, cmd.Intent), encodingBudget: maxEncodingBytes, roomMembership: &cmd.Intent}, ""
}

func roomMembershipHash(key OpKey, intent RoomMembershipIntent) [32]byte {
	h := sha256.New()
	writePart(h, []byte("sqlstore.mutation.room_membership.v1"))
	for _, part := range []string{key.Namespace, key.Deployment, key.RoomID, key.BindingID} {
		writePart(h, []byte(part))
	}
	var generation [8]byte
	binary.BigEndian.PutUint64(generation[:], key.Generation)
	h.Write(generation[:])
	for _, part := range []string{key.OpKind, key.Identifier, key.Principal, intent.TargetUserID, string(intent.Action), string(intent.Role)} {
		writePart(h, []byte(part))
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

type membershipTargetState struct {
	userMissing, userDisabled     bool
	memberMissing, memberDisabled bool
	memberRole                    string
}

// readPlanDependencies preserves the original operation path, and expands a
// membership plan's lock set to its actor and target. Both kinds are sorted
// independently, and the target membership takes UPDATE from the outset;
// there is no later lock upgrade, including when actor and target coincide.
func (s *Store) readPlanDependencies(ctx context.Context, q rowQuerier, plan opPlan, d *dependencyState, lock string) error {
	if plan.control != nil {
		var err error
		d.control, err = s.readControlDependencies(ctx, q, plan, lock)
		return err
	}
	if plan.roomMembership == nil {
		return s.readDependencies(ctx, q, plan.key.RoomID, plan.auth, d, lock)
	}
	targetID := plan.roomMembership.TargetUserID
	ids := []string{plan.userID}
	if targetID != plan.userID {
		ids = append(ids, targetID)
		sort.Strings(ids)
	}
	for _, id := range ids {
		var disabled, missing bool
		err := q.QueryRow(ctx, `SELECT disabled FROM `+s.table("users")+` WHERE user_id = $1`+lock, id).Scan(&disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = true
		} else if err != nil {
			return err
		}
		if id == plan.userID {
			d.userMissing, d.userDisabled = missing, disabled
		}
		if id == targetID {
			d.target.userMissing, d.target.userDisabled = missing, disabled
		}
	}
	var err error
	d.credential, err = s.readCredential(ctx, q, plan.auth, lock)
	if err != nil {
		return err
	}
	for _, id := range ids {
		memberLock := lock
		if id == targetID && lock != lockNone {
			memberLock = lockExclusive
		}
		var role string
		var disabled, missing bool
		err := q.QueryRow(ctx, `SELECT role, disabled FROM `+s.table("room_members")+` WHERE room_id = $1 AND user_id = $2`+memberLock, plan.key.RoomID, id).Scan(&role, &disabled)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = true
		} else if err != nil {
			return err
		}
		if id == plan.userID {
			d.memberRole, d.memberMissing, d.memberDisabled = role, missing, disabled
		}
		if id == targetID {
			d.target.memberRole, d.target.memberMissing, d.target.memberDisabled = role, missing, disabled
		}
	}
	return nil
}

// prepareMembership performs the operation-specific state checks after live
// owner authorization, accepted-receipt lookup and the strict revision CAS.
// It produces a historical fact without mutating the control rows.
func (s *Store) prepareMembership(ctx context.Context, tx pgx.Tx, plan opPlan, locked dependencyState) (model.Event, string, error) {
	intent := *plan.roomMembership
	if intent.TargetUserID == locked.ownerUserID || locked.target.memberRole == "owner" || locked.target.userMissing {
		return model.Event{}, ClassInvalidControlState, nil
	}
	switch intent.Action {
	case MembershipAdmit:
		if locked.target.userDisabled || !locked.target.memberMissing && !locked.target.memberDisabled {
			return model.Event{}, ClassInvalidControlState, nil
		}
	case MembershipChangeRole:
		if locked.target.userDisabled || locked.target.memberMissing || locked.target.memberDisabled {
			return model.Event{}, ClassInvalidControlState, nil
		}
	case MembershipRemove:
		if locked.target.memberMissing || locked.target.memberDisabled {
			return model.Event{}, ClassInvalidControlState, nil
		}
	}
	if intent.Action == MembershipRemove || intent.Role == RoomViewer {
		// Until the full Binding/grant retirement plan is available, refuse
		// removal or viewer admission/demotion with an active Binding rather
		// than preserving a grant that a later promotion could revive.
		// The exclusive ownership guard excludes managed Binding mutations
		// throughout this read and commit. Scope is strictly this Room; the
		// same User's Bindings in other Rooms are unaffected.
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.table("bindings")+` b JOIN `+s.table("agents")+` a ON a.agent_id = b.agent_id JOIN `+s.table("devices")+` d ON d.device_id = a.device_id WHERE b.room_id = $1 AND d.user_id = $2 AND b.retired_at IS NULL)`, plan.key.RoomID, intent.TargetUserID).Scan(&active); err != nil {
			return model.Event{}, "", err
		}
		if active {
			return model.Event{}, ClassUnsupportedControlState, nil
		}
	}
	if locked.currentSeq < 1 {
		return model.Event{}, ClassStateInvalid, nil
	}
	initial, err := s.loadFact(ctx, tx, plan.key.RoomID, 1)
	if err != nil {
		if isStoredFactCorruption(err) {
			return model.Event{}, ClassStateInvalid, err
		}
		return model.Event{}, "", err
	}
	creation, err := model.DecodeEnterpriseRoomCreation(initial)
	if err != nil || creation.Enterprise.DeploymentID != plan.key.Deployment || creation.Enterprise.OrgID != locked.orgID {
		return model.Event{}, ClassStateInvalid, err
	}
	fact, err := model.NewEnterpriseMembershipChange(plan.key.RoomID, locked.currentSeq+1, intent, time.Now().UTC())
	if err != nil {
		return model.Event{}, ClassInvalidFact, err
	}
	return fact, "", nil
}

func (s *Store) writeMembership(ctx context.Context, tx pgx.Tx, plan opPlan, locked dependencyState) error {
	intent := *plan.roomMembership
	if intent.Action == MembershipAdmit && locked.target.memberMissing {
		_, err := tx.Exec(ctx, `INSERT INTO `+s.table("room_members")+` (room_id, org_id, user_id, role) VALUES ($1, $2, $3, $4)`, plan.key.RoomID, locked.orgID, intent.TargetUserID, string(intent.Role))
		return err
	}
	if intent.Action == MembershipRemove {
		_, err := tx.Exec(ctx, `UPDATE `+s.table("room_members")+` SET disabled = true WHERE room_id = $1 AND user_id = $2`, plan.key.RoomID, intent.TargetUserID)
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE `+s.table("room_members")+` SET role = $3, disabled = false WHERE room_id = $1 AND user_id = $2`, plan.key.RoomID, intent.TargetUserID, string(intent.Role))
	return err
}

func validateRoomMembershipReplay(plan opPlan, fact model.Event) error {
	change, err := model.DecodeEnterpriseMembershipChange(fact)
	if err != nil {
		return &storedFactCorruption{err: err}
	}
	if fact.RoomID != plan.key.RoomID || change.EnterpriseMembershipIntent != *plan.roomMembership {
		return &storedFactCorruption{err: fmt.Errorf("stored membership change disagrees with its accepted operation")}
	}
	return nil
}
