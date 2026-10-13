package sqlstore

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type administrationCursor struct {
	Version  int      `json:"version"`
	Resource string   `json:"resource"`
	Keys     []string `json:"keys"`
}

func encodeAdministrationCursor(resource string, keys []string) string {
	data, _ := json.Marshal(administrationCursor{Version: 1, Resource: resource, Keys: keys})
	return base64.RawURLEncoding.EncodeToString(data)
}
func decodeAdministrationCursor(query AdministrationQuery) ([]string, error) {
	if query.Limit < 1 || query.Limit > 50 || len(query.After) > 8192 {
		return nil, errors.New("invalid administration page bounds")
	}
	var count int
	switch query.Resource {
	case "users", "administrators", "connections":
		count = 1
	case "admissions":
		count = 3
	default:
		return nil, errors.New("unsupported administration resource")
	}
	if query.After == "" {
		return make([]string, count), nil
	}
	data, err := base64.RawURLEncoding.DecodeString(query.After)
	if err != nil {
		return nil, errors.New("invalid administration cursor")
	}
	var cursor administrationCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.Version != 1 || cursor.Resource != query.Resource || len(cursor.Keys) != count {
		return nil, errors.New("administration cursor has the wrong scope")
	}
	canonical, _ := json.Marshal(cursor)
	if !bytes.Equal(canonical, data) || encodeAdministrationCursor(cursor.Resource, cursor.Keys) != query.After {
		return nil, errors.New("noncanonical administration cursor")
	}
	if count == 1 && !validRoomCreateIdentity(cursor.Keys[0]) || count == 3 && !validIdentity(administrationIdentity(AdministrationIdentity{ConnectionID: cursor.Keys[0], Issuer: cursor.Keys[1], Subject: cursor.Keys[2]})) {
		return nil, errors.New("invalid administration cursor identity")
	}
	return cursor.Keys, nil
}

// Called only by the common read runner after ownership fencing and the live
// administrator/session checks. No directory row grants Room content access.
func (s *Store) resolveAdministrationDirectory(ctx context.Context, tx pgx.Tx, plan opPlan, d *controlDependencies) (Result, error) {
	if plan.control.administration == nil || plan.control.kind != controlAdministrationRead || !s.controlReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	query := *plan.control.administration
	keys, err := decodeAdministrationCursor(query)
	if err != nil {
		return refused(ClassInvalidRequest, err)
	}
	args := []any{d.orgID, keys[0], query.Limit + 1}
	var statement string
	switch query.Resource {
	case "users", "administrators":
		statement = `SELECT u.user_id,u.disabled,u.tombstoned_at IS NOT NULL,COALESCE(a.enabled,false),u.created_at FROM ` + s.table("users") + ` u LEFT JOIN ` + s.table("organization_admins") + ` a USING(user_id) WHERE u.org_id=$1 AND u.user_id>$2`
		if query.Resource == "administrators" {
			statement += ` AND a.user_id IS NOT NULL`
		}
		statement += ` ORDER BY u.user_id LIMIT $3`
	case "connections":
		statement = `SELECT connection_id,issuer FROM ` + s.table("trust_connections") + ` WHERE org_id=$1 AND connection_id>$2 ORDER BY connection_id LIMIT $3`
	case "admissions":
		statement = `SELECT a.connection_id,a.issuer,a.subject,a.enabled,COALESCE(e.user_id,''),COALESCE(u.disabled,false),e.retired_at IS NOT NULL FROM ` + s.table("identity_admissions") + ` a JOIN ` + s.table("trust_connections") + ` c ON c.connection_id=a.connection_id AND c.issuer=a.issuer LEFT JOIN ` + s.table("external_identities") + ` e ON e.connection_id=a.connection_id AND e.issuer=a.issuer AND e.subject=a.subject LEFT JOIN ` + s.table("users") + ` u ON u.user_id=e.user_id WHERE c.org_id=$1 AND (a.connection_id,a.issuer,a.subject)>($2,$4,$5) ORDER BY a.connection_id,a.issuer,a.subject LIMIT $3`
		args = append(args, keys[1], keys[2])
	}
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return resolveIncomplete(ctx, err)
	}
	defer rows.Close()
	page := AdministrationPage{Resource: query.Resource}
	count := 0
	var last []string
	for rows.Next() {
		if count == query.Limit {
			page.Next = encodeAdministrationCursor(query.Resource, last)
			break
		}
		switch query.Resource {
		case "users", "administrators":
			var value AdministrationUser
			err = rows.Scan(&value.UserID, &value.Disabled, &value.Tombstoned, &value.Administrator, &value.CreatedAt)
			value.CreatedAt = value.CreatedAt.UTC()
			last = []string{value.UserID}
			if query.Resource == "users" {
				page.Users = append(page.Users, value)
			} else {
				page.Administrators = append(page.Administrators, value)
			}
		case "connections":
			var value AdministrationConnection
			err = rows.Scan(&value.ConnectionID, &value.Issuer)
			last = []string{value.ConnectionID}
			page.Connections = append(page.Connections, value)
		case "admissions":
			var value AdministrationAdmission
			err = rows.Scan(&value.ConnectionID, &value.Issuer, &value.Subject, &value.Enabled, &value.UserID, &value.UserDisabled, &value.IdentityRetired)
			last = []string{value.ConnectionID, value.Issuer, value.Subject}
			page.Admissions = append(page.Admissions, value)
		}
		if err != nil {
			return resolveIncomplete(ctx, err)
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return resolveIncomplete(ctx, err)
	}
	return Result{Outcome: OutcomeReplayed, administrationPage: &page}, nil
}
