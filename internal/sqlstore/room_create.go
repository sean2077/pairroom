package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/model"
)

// RoomCreateCommand creates a Native Room under the authenticated human's
// ownership. Its intent is already frozen: defaults must be prepared before
// submitting or resolving the command. Organization and creation metadata
// come from the configured deployment and the transaction, never the caller.
type RoomCreateCommand struct {
	Auth     AuthContext
	RoomID   string
	ClientID string
	Intent   model.EnterpriseRoomIntent
}

// CommitRoomCreate executes room_create through the same transaction state
// machine as the existing append operations. It never retries an effect.
func (s *Store) CommitRoomCreate(ctx context.Context, cmd RoomCreateCommand) (Result, error) {
	plan, class := s.roomCreatePlan(cmd)
	if class != "" {
		return refused(class, nil)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

// ResolveRoomCreate proves the operation's accepted result or absence under
// an exclusive ownership-row guard. In particular, absence of a Room row
// cannot let this check overtake a creation with an uncertain commit result.
func (s *Store) ResolveRoomCreate(ctx context.Context, auth AuthContext, roomID, clientID string, intent model.EnterpriseRoomIntent) (Result, error) {
	plan, class := s.roomCreatePlan(RoomCreateCommand{Auth: auth, RoomID: roomID, ClientID: clientID, Intent: intent})
	if class != "" {
		return refused(class, nil)
	}
	return s.resolveWithFallback(ctx, plan)
}

func (s *Store) roomCreatePlan(cmd RoomCreateCommand) (opPlan, string) {
	if cmd.Auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(cmd.Auth.Deployment) || !validRoomCreateIdentity(cmd.Auth.UserID) || !validRoomCreateIdentity(cmd.RoomID) || !validRoomCreateIdentity(cmd.ClientID) {
		return opPlan{}, ClassInvalidRequest
	}
	if err := model.ValidateEnterpriseRoomIntent(cmd.Intent); err != nil {
		return opPlan{}, ClassInvalidRequest
	}
	key := OpKey{Namespace: namespaceHuman, Deployment: cmd.Auth.Deployment, RoomID: cmd.RoomID,
		OpKind: opKindRoomCreate, Identifier: cmd.ClientID, Principal: cmd.Auth.UserID}
	return opPlan{key: key, userID: cmd.Auth.UserID, audit: AuditRecord{Kind: "room.create", Outcome: auditOutcomeAccepted},
		hash: roomCreateHash(key, cmd.Intent), encodingBudget: maxEncodingBytes, roomCreate: &cmd.Intent}, ""
}

func validRoomCreateIdentity(value string) bool {
	return validToken(value) && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}

// roomCreateHash deliberately excludes generated identity, timestamps and
// metadata derived from the configured deployment. The complete human key
// and each field of the frozen intent have a versioned, unambiguous encoding.
func roomCreateHash(key OpKey, intent model.EnterpriseRoomIntent) [32]byte {
	h := sha256.New()
	writePart(h, []byte("sqlstore.mutation.room_create.v1"))
	writePart(h, []byte(key.Namespace))
	writePart(h, []byte(key.Deployment))
	writePart(h, []byte(key.RoomID))
	writePart(h, []byte(key.BindingID))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], key.Generation)
	h.Write(number[:])
	writePart(h, []byte(key.OpKind))
	writePart(h, []byte(key.Identifier))
	writePart(h, []byte(key.Principal))
	writePart(h, []byte(intent.Name))
	binary.BigEndian.PutUint64(number[:], uint64(intent.Collaboration.Version))
	h.Write(number[:])
	writePart(h, []byte(intent.Collaboration.Mode))
	writePart(h, []byte(intent.Collaboration.Instructions))
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// creationFact obtains the immutable organization from the configured
// deployment. The caller has already taken the exclusive ownership guard and
// verified the live admitted User, fence and absence of this Room.
func (s *Store) creationFact(ctx context.Context, tx pgx.Tx, plan opPlan) (model.Event, string, error) {
	var orgID string
	if err := tx.QueryRow(ctx, `SELECT org_id FROM `+s.table("deployment")+` WHERE deployment_id = $1`, s.cfg.DeploymentID).Scan(&orgID); err != nil {
		return model.Event{}, "", err
	}
	fact, err := model.NewEnterpriseRoomCreation(plan.key.RoomID, s.cfg.DeploymentID, orgID, plan.userID, *plan.roomCreate, time.Now().UTC())
	return fact, orgID, err
}

// validateRoomCreateReplay checks the authoritative original creation, not
// merely its event envelope. Corrupt-but-valid JSON must not be disclosed as
// an accepted typed result. The configured organization is immutable, while
// the Room's current owner may legitimately change after this birth record.
func (s *Store) validateRoomCreateReplay(ctx context.Context, tx pgx.Tx, plan opPlan, fact model.Event) error {
	creation, err := model.DecodeEnterpriseRoomCreation(fact)
	if err != nil {
		return &storedFactCorruption{err: err}
	}
	var orgID string
	if err := tx.QueryRow(ctx, `SELECT org_id FROM `+s.table("deployment")+` WHERE deployment_id = $1`, s.cfg.DeploymentID).Scan(&orgID); err != nil {
		return err
	}
	if fact.RoomID != plan.key.RoomID || creation.Enterprise.DeploymentID != plan.key.Deployment ||
		creation.Enterprise.OrgID != orgID || creation.Enterprise.InitialOwnerUserID != plan.key.Principal ||
		creation.Name != plan.roomCreate.Name || *creation.Collaboration != plan.roomCreate.Collaboration {
		return &storedFactCorruption{err: fmt.Errorf("stored Room creation disagrees with its accepted operation or configured organization")}
	}
	return nil
}

// insertCreatedRoom writes only the initial control rows. The shared commit
// path appends the creation event, accepted operation and audit and advances
// sequence/revision in the same transaction. Slots are vacant: no Runtime or
// Binding is selected by creating a Room.
func (s *Store) insertCreatedRoom(ctx context.Context, tx pgx.Tx, plan opPlan, orgID string, createdAt time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("rooms")+` (room_id, org_id, host_mode, owner_user_id, current_seq, revision, created_at) VALUES ($1, $2, 'native', $3, 0, 0, $4)`, plan.key.RoomID, orgID, plan.userID, createdAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("room_members")+` (room_id, org_id, user_id, role) VALUES ($1, $2, $3, 'owner')`, plan.key.RoomID, orgID, plan.userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+s.table("slot_generations")+` (org_id, room_id, slot, generation, created_at) VALUES ($1, $2, 'slot1', 1, $3), ($1, $2, 'slot2', 1, $3)`, orgID, plan.key.RoomID, createdAt); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+s.table("room_slots")+` (room_id, slot, current_generation) VALUES ($1, 'slot1', 1), ($1, 'slot2', 1)`, plan.key.RoomID)
	return err
}
