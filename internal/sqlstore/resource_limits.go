package sqlstore

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
	"math"
	"strings"
	"time"
)

const ClassResourceLimit = "resource_limit"

func ValidateResourceLimits(l ResourceLimits) error {
	if !l.valid() {
		return errors.New("enterprise resource limits invalid")
	}
	return nil
}

func (l ResourceLimits) Normalized() (ResourceLimits, error) {
	if err := ValidateResourceLimits(l); err != nil {
		return ResourceLimits{}, err
	}
	return l.effective(), nil
}

// ResourceLimits are operator-selected engineering bounds, not a measured
// throughput promise. Zero fields select defaults; no field disables a bound.
// The actionable cap cannot exceed 5000: composite revocation still has its
// independent fail-closed 10000 dependency-set boundary.
type ResourceLimits struct {
	ReplayEvents         int64 `json:"replay_events"`
	ReplayBytes          int64 `json:"replay_bytes"`
	NewWorkEvents        int64 `json:"new_work_events"`
	NewWorkBytes         int64 `json:"new_work_bytes"`
	PendingMessages      int64 `json:"pending_messages"`
	ActionableIdentities int64 `json:"actionable_identities"`
	PersistentRooms      int64 `json:"persistent_rooms"`
	ObjectRecords        int64 `json:"object_records"`
	PersistentProjects   int64 `json:"persistent_projects"`
	PersistentUsers      int64 `json:"persistent_users"`
	MembershipRecords    int64 `json:"membership_records"`
	MembersPerResource   int64 `json:"members_per_resource"`
}

func (l ResourceLimits) effective() ResourceLimits {
	if l.ReplayEvents == 0 {
		l.ReplayEvents = 65536
	}
	if l.ReplayBytes == 0 {
		l.ReplayBytes = 64 << 20
	}
	if l.NewWorkEvents == 0 {
		l.NewWorkEvents = 32768
	}
	if l.NewWorkBytes == 0 {
		l.NewWorkBytes = 32 << 20
	}
	if l.PendingMessages == 0 {
		l.PendingMessages = 256
	}
	if l.ActionableIdentities == 0 {
		l.ActionableIdentities = 5000
	}
	if l.PersistentRooms == 0 {
		l.PersistentRooms = 5000
	}
	if l.ObjectRecords == 0 {
		l.ObjectRecords = 16384
	}
	if l.PersistentProjects == 0 {
		l.PersistentProjects = 1000
	}
	if l.PersistentUsers == 0 {
		l.PersistentUsers = 5000
	}
	if l.MembershipRecords == 0 {
		l.MembershipRecords = 50000
	}
	if l.MembersPerResource == 0 {
		l.MembersPerResource = 1000
	}
	return l
}

func (l ResourceLimits) valid() bool {
	l = l.effective()
	return l.ReplayEvents > 0 && l.ReplayEvents <= 65536 && l.ReplayBytes > 0 && l.ReplayBytes <= 64<<20 && l.NewWorkEvents > 0 && l.NewWorkEvents < l.ReplayEvents && l.NewWorkBytes > 0 && l.NewWorkBytes < l.ReplayBytes && l.PendingMessages > 0 && l.PendingMessages <= 256 && l.ActionableIdentities > 0 && l.ActionableIdentities <= 5000 && l.PersistentRooms > 0 && l.PersistentRooms <= 5000 && l.ObjectRecords > 0 && l.ObjectRecords <= 16384 && l.PersistentProjects > 0 && l.PersistentProjects <= 1000 && l.PersistentUsers > 0 && l.PersistentUsers <= 5000 && l.MembershipRecords > 0 && l.MembershipRecords <= 50000 && l.MembersPerResource > 0 && l.MembersPerResource <= 1000
}

// checkPersistentAdmission counts every record, including archived Rooms and
// object tombstones. GC and archive cannot recycle this metadata allowance.
// The caller holds the exclusive deployment fence and has recovered receipts.
func (s *Store) checkPersistentAdmission(ctx context.Context, tx pgx.Tx, resource string, additional int64) error {
	if additional == 0 {
		return nil
	}
	if additional < 0 {
		return refusedError(ClassStateInvalid, nil)
	}
	l := s.cfg.ResourceLimits.effective()
	var table string
	var limit int64
	switch resource {
	case "rooms":
		table, limit = "rooms", l.PersistentRooms
	case "objects":
		table, limit = "attachments", l.ObjectRecords
	case "projects":
		table, limit = "projects", l.PersistentProjects
	case "users":
		table, limit = "users", l.PersistentUsers
	default:
		return refusedError(ClassStateInvalid, nil)
	}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+s.table(table)).Scan(&current); err != nil {
		return incompleteError(ctx, err)
	}
	if current > limit || additional > limit-current {
		return refusedError(ClassResourceLimit, nil)
	}
	return nil
}

func (s *Store) checkMembershipAdmission(ctx context.Context, tx pgx.Tx, resource, resourceID string, additional int64) error {
	if additional == 0 {
		return nil
	}
	if additional < 0 {
		return refusedError(ClassStateInvalid, nil)
	}
	var table, key string
	switch resource {
	case "room":
		table, key = "room_members", "room_id"
	case "project":
		table, key = "project_members", "project_id"
	default:
		return refusedError(ClassStateInvalid, nil)
	}
	var total, scoped int64
	if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+s.table("room_members")+`)+(SELECT count(*) FROM `+s.table("project_members")+`),(SELECT count(*) FROM `+s.table(table)+` WHERE `+key+`=$1)`, resourceID).Scan(&total, &scoped); err != nil {
		return incompleteError(ctx, err)
	}
	l := s.cfg.ResourceLimits.effective()
	if total > l.MembershipRecords || additional > l.MembershipRecords-total || scoped > l.MembersPerResource || additional > l.MembersPerResource-scoped {
		return refusedError(ClassResourceLimit, nil)
	}
	return nil
}

// checkRoomFactAdmission measures the resulting shared Relay projection's
// canonical completion candidates. This function never changes a state. Its
// caller selects only optional new effects; mandatory settlement bypasses it.
func (s *Store) checkRoomFactAdmission(ctx context.Context, tx pgx.Tx, plan opPlan, facts []model.Event, snapshot RoomSnapshot) error {
	projection := toRelaySnapshot(snapshot)
	projection.Events = append(append([]model.Event(nil), projection.Events...), facts...)
	var addedBytes int64
	for i := range facts {
		fact := facts[i]
		if fact.ID == "" {
			fact.ID = strings.Repeat("<", humanSendMaxTokenBytes)
		}
		fact.Seq = snapshot.Seq + uint64(i) + 1
		projection.Events[len(snapshot.Events)+i] = fact
		origin := userFactOrigin(plan.userID)
		if fact.Kind == relay.EventMessage && fact.Actor == model.ActorSystem {
			origin = operatorFactOrigin()
		}
		encoding, err := encodeStoredFact(fact, origin)
		if err != nil {
			return refusedError(ClassInvalidFact, err)
		}
		addedBytes += int64(len(encoding))
	}
	projection.Seq = snapshot.Seq + uint64(len(facts))
	projection.Revision = snapshot.Revision + 1
	// The trusted primitive store seam predates typed Room creation and is used
	// by storage equivalence fixtures with arbitrary facts. It has no Relay
	// queue. Public session-authenticated Rooms always take the strict path.
	if !s.cfg.RequireSessionAuth && plan.human == nil && plan.roomCreate == nil && plan.roomMembership == nil && plan.binding == nil && plan.relay == nil {
		return s.checkRoomNewWorkBudget(ctx, tx, snapshot.RoomID, int64(len(facts)), addedBytes, 0, 0)
	}
	reserve, err := relay.EnterpriseResourceReserve(projection)
	if err != nil {
		return refusedError(ClassStateInvalid, err)
	}
	if int64(reserve.Pending) > s.cfg.ResourceLimits.effective().PendingMessages {
		return refusedError(ClassResourceLimit, nil)
	}
	var terminalBytes int64
	worstUser := strings.Repeat("<", humanSendMaxTokenBytes)
	// Preserve canonical metadata space for each current non-Owner membership
	// retirement, ownership recovery, and Room archive after admission closes.
	// These are typed existing fact codecs, not a second transition planner.
	var memberRetirements int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+s.table("room_members")+` WHERE room_id=$1 AND NOT disabled AND role<>'owner'`, snapshot.RoomID).Scan(&memberRetirements); err != nil {
		return incompleteError(ctx, err)
	}
	if plan.roomMembership != nil && plan.roomMembership.Action == model.EnterpriseMembershipAdmit {
		memberRetirements++
	}
	if memberRetirements > s.cfg.ResourceLimits.effective().ReplayEvents {
		return refusedError(ClassResourceLimit, nil)
	}
	mandatory := []struct {
		kind    string
		payload any
	}{
		{model.EnterpriseMembershipKind, model.EnterpriseMembershipChange{Version: 1, EnterpriseMembershipIntent: model.EnterpriseMembershipIntent{TargetUserID: worstUser, Action: model.EnterpriseMembershipChangeRole, Role: model.EnterpriseRoomViewer}}},
		{model.EnterpriseOwnershipKind, model.EnterpriseOwnershipChange{Version: 1, FromUserID: worstUser, ToUserID: strings.Repeat(">", humanSendMaxTokenBytes), Governance: true}},
		{model.EnterpriseRoomMetadataKind, model.EnterpriseRoomMetadataChange{Version: 1, Action: "archive"}},
	}
	for _, metadata := range mandatory {
		data, err := json.Marshal(metadata.payload)
		if err != nil {
			return refusedError(ClassStateInvalid, err)
		}
		reserve.TerminalFacts = append(reserve.TerminalFacts, model.Event{RoomID: snapshot.RoomID, Kind: metadata.kind, Actor: model.ActorSystem, CreatedAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), Data: data})
	}
	terminalEvents := int64(len(reserve.TerminalFacts)) - 1 + memberRetirements
	for _, candidate := range reserve.TerminalFacts {
		candidate.ID = strings.Repeat("<", humanSendMaxTokenBytes)
		candidate.Seq = math.MaxInt64
		// The state candidates already came from the strict shared planner. The
		// wrapper here reserves the largest allowed causal User representation,
		// rather than substituting it as the actual author of a future event.
		encoding, err := json.Marshal(storedEventRecord{CodecVersion: factCodecVersion, OriginKind: "user", OriginUser: &worstUser, Event: candidate})
		if err != nil {
			return refusedError(ClassStateInvalid, err)
		}
		multiplicity := int64(1)
		if candidate.Kind == model.EnterpriseMembershipKind {
			multiplicity = memberRetirements
		}
		if multiplicity > 0 && int64(len(encoding)) > math.MaxInt64/multiplicity {
			return refusedError(ClassResourceLimit, nil)
		}
		terminalBytes += int64(len(encoding)) * multiplicity
	}
	return s.checkRoomNewWorkBudget(ctx, tx, snapshot.RoomID, int64(len(facts)), addedBytes, terminalEvents, terminalBytes)
}

type roomResourceUsage struct{ events, bytes int64 }

// readRoomResourceUsage is a metadata-only preflight. Its caller already holds
// the Room and ownership guards, so count/bytes cannot race the subsequent read.
func (s *Store) readRoomResourceUsage(ctx context.Context, tx pgx.Tx, roomID string) (roomResourceUsage, error) {
	var u roomResourceUsage
	err := tx.QueryRow(ctx, `SELECT count(*)::bigint, COALESCE(sum(octet_length(event_encoding)),0)::bigint FROM `+s.table("room_events")+` WHERE room_id=$1`, roomID).Scan(&u.events, &u.bytes)
	if err != nil {
		return roomResourceUsage{}, incompleteError(ctx, err)
	}
	return u, nil
}

func (s *Store) checkRoomReplayBudget(ctx context.Context, tx pgx.Tx, roomID string, seq uint64) error {
	l := s.cfg.ResourceLimits.effective()
	u, err := s.readRoomResourceUsage(ctx, tx, roomID)
	if err != nil {
		return err
	}
	// Check the authoritative counter against metadata before allocation or
	// decoding. A corrupt counter is not a legitimate capacity refusal.
	if u.events < 0 || uint64(u.events) != seq {
		return refusedError(ClassStateInvalid, nil)
	}
	if u.events > l.ReplayEvents || u.bytes > l.ReplayBytes {
		return refusedError(ClassResourceLimit, nil)
	}
	return nil
}

// checkRoomNewWorkBudget is called only for a new optional effect after live
// authorization and accepted-result recovery. The Relay planner supplies exact
// encoded terminal liability for the resulting unsettled set. ACK, cancellation,
// recovery and required revocation never use the soft admission boundary.
func (s *Store) checkRoomNewWorkBudget(ctx context.Context, tx pgx.Tx, roomID string, addedEvents, addedBytes, terminalEvents, terminalBytes int64) error {
	if addedEvents < 0 || addedBytes < 0 || terminalEvents < 0 || terminalBytes < 0 {
		return refusedError(ClassStateInvalid, nil)
	}
	l := s.cfg.ResourceLimits.effective()
	u, err := s.readRoomResourceUsage(ctx, tx, roomID)
	if err != nil {
		return err
	}
	// Subtraction avoids overflow even for hostile arguments. Reserve is a hard
	// bound in addition to the soft new-work threshold, never a guessed constant.
	if u.events > l.NewWorkEvents || addedEvents > l.NewWorkEvents-u.events || u.bytes > l.NewWorkBytes || addedBytes > l.NewWorkBytes-u.bytes || terminalEvents > l.ReplayEvents-u.events-addedEvents || terminalBytes > l.ReplayBytes-u.bytes-addedBytes {
		return refusedError(ClassResourceLimit, nil)
	}
	return nil
}

// checkActionableAdmission is called under the exclusive deployment fence
// before creating/re-enabling authority. Retired history does not consume this
// budget. A family reserves its current and recoverable predecessor as three
// units independent of access-token expiry or credential rotation history.
func (s *Store) checkActionableAdmission(ctx context.Context, tx pgx.Tx, additional int64) error {
	if additional == 0 {
		return nil
	}
	if additional < 0 {
		return refusedError(ClassStateInvalid, nil)
	}
	var current int64
	query := `SELECT (SELECT count(*) FROM ` + s.table("users") + ` WHERE NOT disabled)+(SELECT count(*) FROM ` + s.table("devices") + ` WHERE retired_at IS NULL)+(SELECT 3*count(*) FROM ` + s.table("device_families") + ` WHERE revoked_at IS NULL AND expires_at>pg_catalog.clock_timestamp())+(SELECT count(*) FROM ` + s.table("agents") + ` a JOIN ` + s.table("devices") + ` d ON d.device_id=a.device_id WHERE a.retired_at IS NULL AND d.retired_at IS NULL)+(SELECT 2*count(*) FROM ` + s.table("bindings") + ` WHERE retired_at IS NULL)+(SELECT count(*) FROM ` + s.table("browser_sessions") + ` WHERE revoked_at IS NULL AND expires_at>pg_catalog.clock_timestamp())+(SELECT count(*) FROM ` + s.table("device_enrollments") + ` WHERE status IN ('pending','approved') AND expires_at>pg_catalog.clock_timestamp())+(SELECT count(*) FROM ` + s.table("binding_requests") + ` WHERE status IN ('pending','approved') AND expires_at>pg_catalog.clock_timestamp())`
	if err := tx.QueryRow(ctx, query).Scan(&current); err != nil {
		return incompleteError(ctx, err)
	}
	limit := s.cfg.ResourceLimits.effective().ActionableIdentities
	if current > limit || additional > limit-current {
		return refusedError(ClassResourceLimit, nil)
	}
	return nil
}
