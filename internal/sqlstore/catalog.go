package sqlstore

import (
	"context"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

type catalogMutation struct {
	kind  string
	query CatalogQuery
	cmd   CatalogCommand
}

func catalogReadOnly(kind string) bool { return kind == "read" }

func validCatalogName(name string) bool {
	return name != "" && len(name) <= model.MaxEnterpriseRoomNameBytes && strings.TrimSpace(name) == name && utf8.ValidString(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

func validateCatalogQuery(q CatalogQuery) bool {
	if q.Limit < 1 || q.Limit > 50 || q.After != "" && !validRoomCreateIdentity(q.After) || q.State != "" && q.State != "active" && q.State != "archived" && q.State != "all" {
		return false
	}
	switch q.Resource {
	case "rooms":
		return q.RoomID == "" && (q.ProjectID == "" || validRoomCreateIdentity(q.ProjectID))
	case "room", "room-members":
		return validRoomCreateIdentity(q.RoomID) && q.ProjectID == "" && q.State == ""
	case "projects", "devices", "agents", "sessions":
		return q.RoomID == "" && q.ProjectID == "" && (q.Resource == "projects" || q.State == "")
	case "project-members":
		return q.RoomID == "" && validRoomCreateIdentity(q.ProjectID) && q.State == ""
	default:
		return false
	}
}

func validateCatalogIntent(i CatalogIntent) bool {
	room, project, user, session, agent, name := false, false, false, false, false, false
	switch i.Kind {
	case "project.create", "project.rename":
		project, name = true, true
	case "project.archive", "project.restore":
		project = true
	case "project.member.add", "project.member.remove":
		project, user = true, true
	case "room.rename":
		room, name = true, true
	case "room.archive", "room.restore":
		room = true
	case "room.project":
		room = true
		project = i.ProjectID != "" // Empty explicitly removes the logical grouping.
	case "session.revoke":
		session = true
	case "agent.retire":
		agent = true
	default:
		return false
	}
	for _, value := range []struct {
		required bool
		value    string
	}{{room, i.RoomID}, {project, i.ProjectID}, {user, i.TargetUserID}, {session, i.SessionID}, {agent, i.AgentID}} {
		if value.required && !validRoomCreateIdentity(value.value) || !value.required && value.value != "" {
			return false
		}
	}
	return name && validCatalogName(i.Name) || !name && i.Name == ""
}

func (s *Store) catalogPlan(auth AuthContext, clientID string, mutation catalogMutation) (opPlan, error) {
	if auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(auth.UserID) || !validRoomCreateIdentity(auth.SessionID) || auth.DeviceGrantID != "" || auth.BindingGrantID != "" || !validRoomCreateIdentity(clientID) {
		return opPlan{}, refusedError(ClassAuthorization, nil)
	}
	if catalogReadOnly(mutation.kind) {
		if !validateCatalogQuery(mutation.query) {
			return opPlan{}, refusedError(ClassInvalidRequest, nil)
		}
	} else if mutation.kind != mutation.cmd.Intent.Kind || !validateCatalogIntent(mutation.cmd.Intent) || mutation.cmd.ExpectedRevision > math.MaxInt64 || (mutation.cmd.Intent.Kind == "project.create" || mutation.cmd.Intent.Kind == "session.revoke" || mutation.cmd.Intent.Kind == "agent.retire") && mutation.cmd.ExpectedRevision != 0 {
		return opPlan{}, refusedError(ClassInvalidRequest, nil)
	}
	key := OpKey{Namespace: "catalog", Deployment: auth.Deployment, OpKind: mutation.kind, Identifier: clientID, Principal: auth.UserID}
	hash := controlHash(key, struct {
		Intent   CatalogIntent
		Revision uint64
	}{mutation.cmd.Intent, mutation.cmd.ExpectedRevision})
	return opPlan{auth: auth, userID: auth.UserID, key: key, hash: hash, catalog: &mutation}, nil
}

func (s *Store) ReadCatalog(ctx context.Context, auth AuthContext, query CatalogQuery) (CatalogPage, error) {
	if query.Limit == 0 {
		query.Limit = 25
	}
	plan, err := s.catalogPlan(auth, "read", catalogMutation{kind: "read", query: query})
	if err != nil {
		return CatalogPage{}, err
	}
	result, err := s.resolveWithFallback(ctx, plan)
	if err != nil {
		return CatalogPage{}, err
	}
	if result.catalogPage == nil {
		return CatalogPage{}, refusedError(ClassStateInvalid, nil)
	}
	return *result.catalogPage, nil
}

func (s *Store) CommitCatalog(ctx context.Context, cmd CatalogCommand) (Result, error) {
	plan, err := s.catalogPlan(cmd.Auth, cmd.ClientID, catalogMutation{kind: cmd.Intent.Kind, cmd: cmd})
	if err != nil {
		return refused(ClassInvalidRequest, err)
	}
	return s.commitWithRestarts(ctx, plan, model.Event{})
}

func (s *Store) ResolveCatalog(ctx context.Context, cmd CatalogCommand) (Result, error) {
	plan, err := s.catalogPlan(cmd.Auth, cmd.ClientID, catalogMutation{kind: cmd.Intent.Kind, cmd: cmd})
	if err != nil {
		return refused(ClassInvalidRequest, err)
	}
	return s.resolveWithFallback(ctx, plan)
}
