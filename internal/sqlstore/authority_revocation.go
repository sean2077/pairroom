package sqlstore

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// A Room limit is mandatory for member removal/demotion. Device/User authority
// revocation omits it and therefore includes archived Rooms. Neither form can
// revoke another User merely because they share a Room.
type authorityRevocationScope struct{ UserID, DeviceID, RoomID, AgentID string }

func (s *Store) authorityBindings(ctx context.Context, q rowQuerier, scope authorityRevocationScope, active bool) ([]string, error) {
	if scope.UserID == "" && scope.DeviceID == "" && scope.AgentID == "" {
		return nil, errors.New("revocation requires a User or Device")
	}
	var ids []string
	e := q.QueryRow(ctx, `SELECT COALESCE(array_agg(binding_id ORDER BY binding_id),ARRAY[]::text[]) FROM (SELECT b.binding_id FROM `+s.table("bindings")+` b JOIN `+s.table("agents")+` a USING(agent_id) JOIN `+s.table("devices")+` d USING(device_id) WHERE ($1='' OR d.user_id=$1) AND ($2='' OR d.device_id=$2) AND ($3='' OR b.room_id=$3) AND (NOT $4 OR b.retired_at IS NULL) AND ($5='' OR b.agent_id=$5) ORDER BY b.binding_id LIMIT 10001) selected`, scope.UserID, scope.DeviceID, scope.RoomID, active, scope.AgentID).Scan(&ids)
	if len(ids) > 10000 {
		return nil, errors.New("revocation scope exceeds bounded transaction")
	}
	return ids, e
}
func (s *Store) readAuthorityRevocation(ctx context.Context, q rowQuerier, scope authorityRevocationScope, lock string) ([32]byte, error) {
	var zero [32]byte
	bindingIDs, e := s.authorityBindings(ctx, q, scope, false)
	if e != nil {
		return zero, e
	}
	h := sha256.New()
	selected := []struct {
		table, key, query string
		args              []any
	}{
		{"binding_requests", "request_id", `SELECT request_id FROM ` + s.table("binding_requests") + ` WHERE status='approved' AND ($3='' OR room_id=$3) AND ($4='' OR agent_id=$4) AND (($1<>'' AND (user_id=$1 OR ($3='' AND approved_by=$1))) OR ($2<>'' AND device_id=$2) OR ($4<>'' AND agent_id=$4))`, []any{scope.UserID, scope.DeviceID, scope.RoomID, scope.AgentID}},
		{"devices", "device_id", `SELECT device_id FROM ` + s.table("devices") + ` d WHERE ($1='' OR user_id=$1) AND ($2='' OR device_id=$2) AND ($3='' OR EXISTS(SELECT 1 FROM ` + s.table("agents") + ` a JOIN ` + s.table("bindings") + ` b USING(agent_id) WHERE a.device_id=d.device_id AND b.room_id=$3)) AND ($4='' OR EXISTS(SELECT 1 FROM ` + s.table("agents") + ` a WHERE a.agent_id=$4 AND a.device_id=d.device_id))`, []any{scope.UserID, scope.DeviceID, scope.RoomID, scope.AgentID}},
		{"agents", "agent_id", `SELECT a.agent_id FROM ` + s.table("agents") + ` a JOIN ` + s.table("devices") + ` d USING(device_id) WHERE ($1='' OR d.user_id=$1) AND ($2='' OR d.device_id=$2) AND ($3='' OR EXISTS(SELECT 1 FROM ` + s.table("bindings") + ` b WHERE b.agent_id=a.agent_id AND b.room_id=$3)) AND ($4='' OR a.agent_id=$4)`, []any{scope.UserID, scope.DeviceID, scope.RoomID, scope.AgentID}},
		{"device_families", "family_id", `SELECT f.family_id FROM ` + s.table("device_families") + ` f WHERE $3='' AND $4='' AND ($1='' OR f.user_id=$1) AND ($2='' OR f.device_id=$2)`, []any{scope.UserID, scope.DeviceID, scope.RoomID, scope.AgentID}},
		{"device_grants", "grant_id", `SELECT g.grant_id FROM ` + s.table("device_grants") + ` g JOIN ` + s.table("device_families") + ` f USING(family_id) WHERE $3='' AND $4='' AND ($1='' OR f.user_id=$1) AND ($2='' OR f.device_id=$2)`, []any{scope.UserID, scope.DeviceID, scope.RoomID, scope.AgentID}},
		{"binding_grants", "grant_id", `SELECT grant_id FROM ` + s.table("binding_grants") + ` WHERE binding_id=ANY($1::text[])`, []any{bindingIDs}},
		{"bindings", "binding_id", `SELECT binding_id FROM ` + s.table("bindings") + ` WHERE binding_id=ANY($1::text[])`, []any{bindingIDs}},
	}
	for _, set := range selected {
		var ids []string
		e = q.QueryRow(ctx, `SELECT COALESCE(array_agg(`+set.key+` ORDER BY `+set.key+`),ARRAY[]::text[]) FROM (`+set.query+` ORDER BY `+set.key+` LIMIT 10001) selected`, set.args...).Scan(&ids)
		if e != nil {
			return zero, e
		}
		if len(ids) > 10000 {
			return zero, errors.New("revocation scope exceeds bounded transaction")
		}
		for _, id := range ids {
			var state string
			clause := lockNone
			if lock != "" {
				clause = lockExclusive
			}
			e = q.QueryRow(ctx, `SELECT to_jsonb(t)::text FROM `+s.table(set.table)+` t WHERE `+set.key+`=$1`+clause, id).Scan(&state)
			if e != nil {
				return zero, e
			}
			writePart(h, []byte(set.table))
			writePart(h, []byte(state))
		}
	}
	var rooms []string
	e = q.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT room_id ORDER BY room_id),ARRAY[]::text[]) FROM `+s.table("bindings")+` WHERE binding_id=ANY($1::text[]) AND retired_at IS NULL`, bindingIDs).Scan(&rooms)
	if e != nil {
		return zero, e
	}
	for _, roomID := range rooms {
		var slots []string
		e = q.QueryRow(ctx, `SELECT COALESCE(array_agg(slot ORDER BY slot),ARRAY[]::text[]) FROM `+s.table("room_slots")+` WHERE room_id=$1`, roomID).Scan(&slots)
		if e != nil {
			return zero, e
		}
		for _, slot := range slots {
			var state string
			clause := lockNone
			if lock != "" {
				clause = lockExclusive
			}
			if e = q.QueryRow(ctx, `SELECT to_jsonb(t)::text FROM `+s.table("room_slots")+` t WHERE room_id=$1 AND slot=$2`+clause, roomID, slot).Scan(&state); e != nil {
				return zero, e
			}
			writePart(h, []byte(state))
		}
	}
	for _, roomID := range rooms {
		var state string
		clause := lockNone
		if lock != "" {
			clause = lockExclusive
		}
		if e = q.QueryRow(ctx, `SELECT to_jsonb(t)::text FROM `+s.table("rooms")+` t WHERE room_id=$1`+clause, roomID).Scan(&state); e != nil {
			return zero, e
		}
		writePart(h, []byte(state))
	}
	copy(zero[:], h.Sum(nil))
	return zero, nil
}

// retireAuthorityBindings is only a write body inside the caller's fenced TX.
// The shared Relay planner validates full history and reuses the existing
// Binding projection for cancellation; this layer does not invent a queue.
func (s *Store) retireAuthorityBindings(ctx context.Context, tx pgx.Tx, scope authorityRevocationScope, actor string) error {
	ids, e := s.authorityBindings(ctx, tx, scope, true)
	if e != nil {
		return e
	}
	rooms := map[string]uint64{}
	for _, id := range ids {
		var roomID, agentID, runtime, slot string
		var generation uint64
		if e = tx.QueryRow(ctx, `SELECT b.room_id,b.agent_id,COALESCE(a.runtime_kind,''),b.slot,b.generation FROM `+s.table("bindings")+` b JOIN `+s.table("agents")+` a USING(agent_id) WHERE b.binding_id=$1`, id).Scan(&roomID, &agentID, &runtime, &slot, &generation); e != nil {
			return e
		}
		if !model.RuntimeKind(runtime).Valid() {
			return &storedFactCorruption{err: errors.New("active Binding has no recorded Runtime")}
		}
		var seq, revision uint64
		var org, mode string
		if e = tx.QueryRow(ctx, `SELECT current_seq,revision,org_id,host_mode FROM `+s.table("rooms")+` WHERE room_id=$1`, roomID).Scan(&seq, &revision, &org, &mode); e != nil {
			return e
		}
		if seq >= math.MaxInt64 || revision >= math.MaxInt64 {
			return errors.New("room revocation counter exhausted")
		}
		snap, e := s.readRoomEvents(ctx, tx, RoomSnapshot{RoomID: roomID, DeploymentID: s.cfg.DeploymentID, OrgID: org, HostMode: mode, Seq: seq, Revision: revision})
		if e != nil {
			return e
		}
		reason := "user.revoked"
		if scope.DeviceID != "" {
			reason = "device.revoked"
		}
		if scope.AgentID != "" {
			reason = "agent.revoked"
		}
		if scope.RoomID != "" {
			reason = "membership.revoked"
		}
		eventID, e := controlID("event")
		if e != nil {
			return e
		}
		prepare, e := relay.PrepareEnterpriseBindingRetirement(toRelaySnapshot(snap), id, reason, eventID)
		if e != nil {
			return &storedFactCorruption{err: e}
		}
		at := time.Now().UTC()
		fact, e := prepare(at)
		if e != nil {
			return &storedFactCorruption{err: e}
		}
		identity, e := model.DecodeEnterpriseBindingUpdate(fact)
		if e != nil {
			return e
		}
		if identity.BindID != id || identity.AgentID != agentID || string(identity.Runtime) != runtime || string(identity.Slot) != slot || identity.Generation != generation || identity.Active {
			return &storedFactCorruption{err: errors.New("binding authority disagrees with original fact")}
		}
		fact.Seq = seq + 1
		if _, e = tx.Exec(ctx, `UPDATE `+s.table("bindings")+` SET retired_at=$2,retired_reason=$3 WHERE binding_id=$1 AND retired_at IS NULL`, id, at, reason); e != nil {
			return e
		}
		if e := s.writeStoredRoomEvent(ctx, tx, fact, userFactOrigin(actor)); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `INSERT INTO `+s.table("audit")+`(deployment_id,room_id,user_id,kind,outcome,at) VALUES($1,$2,$3,'binding.retire','accepted',$4)`, s.cfg.DeploymentID, roomID, actor, at); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET current_seq=$2 WHERE room_id=$1`, roomID, int64(fact.Seq)); e != nil {
			return e
		}
		rooms[roomID] = revision
	}
	if scope.RoomID == "" {
		for roomID, revision := range rooms {
			if _, e = tx.Exec(ctx, `UPDATE `+s.table("rooms")+` SET revision=$2 WHERE room_id=$1`, roomID, int64(revision+1)); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *Store) revokeNativeCredentials(ctx context.Context, tx pgx.Tx, scope authorityRevocationScope) error {
	if scope.UserID == "" && scope.DeviceID == "" && scope.AgentID == "" {
		return errors.New("revocation requires identity scope")
	}
	// A revoked authorization must not return through an ABA of a User,
	// Owner or membership flag. Preserve the original approval/receipt as
	// history, while making every still-unconsumed affected request terminal.
	if _, e := tx.Exec(ctx, `UPDATE `+s.table("binding_requests")+` SET status='denied' WHERE status='approved' AND ($3='' OR room_id=$3) AND ($4='' OR agent_id=$4) AND (($1<>'' AND (user_id=$1 OR ($3='' AND approved_by=$1))) OR ($2<>'' AND device_id=$2) OR ($4<>'' AND agent_id=$4))`, scope.UserID, scope.DeviceID, scope.RoomID, scope.AgentID); e != nil {
		return e
	}
	if scope.RoomID == "" && scope.AgentID == "" {
		if _, e := tx.Exec(ctx, `UPDATE `+s.table("device_families")+` SET revoked_at=COALESCE(revoked_at,pg_catalog.clock_timestamp()) WHERE ($1='' OR user_id=$1) AND ($2='' OR device_id=$2)`, scope.UserID, scope.DeviceID); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `UPDATE `+s.table("device_grants")+` g SET revoked_at=COALESCE(g.revoked_at,pg_catalog.clock_timestamp()) FROM `+s.table("device_families")+` f WHERE f.family_id=g.family_id AND ($1='' OR f.user_id=$1) AND ($2='' OR f.device_id=$2)`, scope.UserID, scope.DeviceID); e != nil {
			return e
		}
	}
	_, e := tx.Exec(ctx, `UPDATE `+s.table("binding_grants")+` g SET revoked_at=COALESCE(g.revoked_at,pg_catalog.clock_timestamp()) FROM `+s.table("bindings")+` b,`+s.table("agents")+` a,`+s.table("devices")+` d WHERE g.binding_id=b.binding_id AND a.agent_id=b.agent_id AND d.device_id=a.device_id AND ($1='' OR d.user_id=$1) AND ($2='' OR d.device_id=$2) AND ($3='' OR b.room_id=$3) AND ($4='' OR b.agent_id=$4)`, scope.UserID, scope.DeviceID, scope.RoomID, scope.AgentID)
	return e
}
