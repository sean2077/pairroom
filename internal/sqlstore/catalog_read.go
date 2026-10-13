package sqlstore

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Store) readCatalogPage(ctx context.Context, tx pgx.Tx, plan opPlan) (CatalogPage, error) {
	query := plan.catalog.query
	page := CatalogPage{Resource: query.Resource}
	args := []any{plan.userID, query.After, query.Limit + 1}
	var statement string
	switch query.Resource {
	case "room", "rooms":
		statement = `SELECT r.room_id,r.display_name,m.role,r.owner_user_id,CASE WHEN pm.user_id IS NOT NULL THEN COALESCE(r.project_id,'') ELSE '' END,CASE WHEN pm.user_id IS NOT NULL THEN COALESCE(p.display_name,'') ELSE '' END,r.archived_at IS NOT NULL,o.disabled OR o.tombstoned_at IS NOT NULL,r.current_seq,r.revision FROM ` + s.table("rooms") + ` r JOIN ` + s.table("room_members") + ` m ON m.room_id=r.room_id AND m.user_id=$1 AND NOT m.disabled JOIN ` + s.table("users") + ` o ON o.user_id=r.owner_user_id LEFT JOIN ` + s.table("projects") + ` p ON p.project_id=r.project_id LEFT JOIN ` + s.table("project_members") + ` pm ON pm.project_id=p.project_id AND pm.user_id=$1 AND NOT pm.disabled WHERE r.room_id>$2 AND ($4='' OR r.project_id=$4) AND ($5='all' OR (r.archived_at IS NOT NULL)=($5='archived')) ORDER BY r.room_id LIMIT $3`
		args = append(args, query.ProjectID, query.State)
		if query.Resource == "room" {
			statement = strings.Replace(statement, "r.room_id>$2 AND", "r.room_id=$6 AND r.room_id>$2 AND", 1)
			args = append(args, query.RoomID)
			args[4] = "all"
		}
	case "projects":
		statement = `SELECT p.project_id,p.display_name,p.owner_user_id,m.role,p.archived_at IS NOT NULL,p.revision FROM ` + s.table("projects") + ` p JOIN ` + s.table("project_members") + ` m ON m.project_id=p.project_id AND m.user_id=$1 AND NOT m.disabled WHERE p.project_id>$2 AND p.owner_user_id IS NOT NULL AND ($4='all' OR (p.archived_at IS NOT NULL)=($4='archived')) ORDER BY p.project_id LIMIT $3`
		args = append(args, query.State)
	case "room-members":
		statement = `SELECT m.user_id,m.role,m.disabled OR u.disabled OR u.tombstoned_at IS NOT NULL FROM ` + s.table("room_members") + ` m JOIN ` + s.table("users") + ` u USING(user_id) WHERE m.user_id>$2 AND m.room_id=$4 AND EXISTS(SELECT 1 FROM ` + s.table("room_members") + ` own WHERE own.room_id=m.room_id AND own.user_id=$1 AND NOT own.disabled) ORDER BY m.user_id LIMIT $3`
		args = append(args, query.RoomID)
	case "project-members":
		statement = `SELECT m.user_id,m.role,m.disabled OR u.disabled OR u.tombstoned_at IS NOT NULL FROM ` + s.table("project_members") + ` m JOIN ` + s.table("users") + ` u USING(user_id) WHERE m.user_id>$2 AND m.project_id=$4 AND EXISTS(SELECT 1 FROM ` + s.table("project_members") + ` own WHERE own.project_id=m.project_id AND own.user_id=$1 AND NOT own.disabled) ORDER BY m.user_id LIMIT $3`
		args = append(args, query.ProjectID)
	case "devices":
		statement = `SELECT d.device_id,COALESCE(e.label,d.device_id),CASE WHEN d.retired_at IS NOT NULL OR d.disabled THEN 'retired' WHEN f.family_id IS NULL OR f.revoked_at IS NOT NULL OR f.restore_epoch<>p.restore_epoch OR f.expires_at<=pg_catalog.clock_timestamp() THEN 'expired' ELSE 'active' END,d.created_at,COALESCE(f.expires_at,d.created_at) FROM ` + s.table("devices") + ` d LEFT JOIN ` + s.table("device_enrollments") + ` e ON e.device_id=d.device_id AND e.status='completed' LEFT JOIN ` + s.table("device_families") + ` f ON f.family_id=e.request_id CROSS JOIN ` + s.table("identity_policy") + ` p WHERE d.user_id=$1 AND d.device_id>$2 ORDER BY d.device_id LIMIT $3`
	case "agents":
		statement = `SELECT a.agent_id,a.device_id,COALESCE(a.runtime_kind,''),CASE WHEN a.retired_at IS NOT NULL OR d.retired_at IS NOT NULL OR d.disabled THEN 'retired' ELSE 'registered' END,a.created_at FROM ` + s.table("agents") + ` a JOIN ` + s.table("devices") + ` d USING(device_id) WHERE d.user_id=$1 AND a.agent_id>$2 ORDER BY a.agent_id LIMIT $3`
	case "sessions":
		statement = `SELECT b.session_id,b.session_id=$4,CASE WHEN b.revoked_at IS NOT NULL THEN 'revoked' WHEN b.expires_at<=pg_catalog.clock_timestamp() OR b.restore_epoch<>p.restore_epoch THEN 'expired' ELSE 'active' END,b.created_at,b.expires_at FROM ` + s.table("browser_sessions") + ` b CROSS JOIN ` + s.table("identity_policy") + ` p WHERE b.user_id=$1 AND b.session_id>$2 ORDER BY b.session_id LIMIT $3`
		args = append(args, plan.auth.SessionID)
	}
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return CatalogPage{}, err
	}
	defer rows.Close()
	count, last := 0, ""
	for rows.Next() {
		if count == query.Limit {
			page.Next = last
			break
		}
		switch query.Resource {
		case "room", "rooms":
			var value CatalogRoom
			err = rows.Scan(&value.RoomID, &value.Name, &value.Role, &value.OwnerUserID, &value.ProjectID, &value.ProjectName, &value.Archived, &value.OwnershipPendingRecovery, &value.Seq, &value.Revision)
			last = value.RoomID
			page.Rooms = append(page.Rooms, value)
		case "projects":
			var value CatalogProject
			err = rows.Scan(&value.ProjectID, &value.Name, &value.OwnerUserID, &value.Role, &value.Archived, &value.Revision)
			last = value.ProjectID
			page.Projects = append(page.Projects, value)
		case "room-members", "project-members":
			var value CatalogMember
			err = rows.Scan(&value.UserID, &value.Role, &value.Disabled)
			last = value.UserID
			page.Members = append(page.Members, value)
		case "devices":
			var value CatalogDevice
			err = rows.Scan(&value.DeviceID, &value.Label, &value.Status, &value.CreatedAt, &value.ExpiresAt)
			last = value.DeviceID
			value.CreatedAt = value.CreatedAt.UTC()
			value.ExpiresAt = value.ExpiresAt.UTC()
			page.Devices = append(page.Devices, value)
		case "agents":
			var value CatalogAgent
			err = rows.Scan(&value.AgentID, &value.DeviceID, &value.Runtime, &value.Status, &value.CreatedAt)
			last = value.AgentID
			value.CreatedAt = value.CreatedAt.UTC()
			page.Agents = append(page.Agents, value)
		case "sessions":
			var value CatalogSession
			err = rows.Scan(&value.SessionID, &value.Current, &value.Status, &value.CreatedAt, &value.ExpiresAt)
			last = value.SessionID
			value.CreatedAt = value.CreatedAt.UTC()
			value.ExpiresAt = value.ExpiresAt.UTC()
			page.Sessions = append(page.Sessions, value)
		}
		if err != nil {
			return CatalogPage{}, err
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return CatalogPage{}, err
	}
	return page, nil
}
