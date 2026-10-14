package sqlstore

import (
	"strings"
	"testing"
)

func TestCatalogIntentIsClosedAndLogicalOnly(t *testing.T) {
	for _, intent := range []CatalogIntent{{Kind: "project.create", ProjectID: "project-1", Name: "Research"}, {Kind: "room.project", RoomID: "room-1"}, {Kind: "room.project", RoomID: "room-1", ProjectID: "project-1"}, {Kind: "agent.retire", AgentID: "agent-1"}, {Kind: "session.revoke", SessionID: "session-1"}} {
		if !validateCatalogIntent(intent) {
			t.Fatalf("valid intent refused: %+v", intent)
		}
	}
	for _, intent := range []CatalogIntent{{Kind: "project.create", ProjectID: "project-1", Name: " Research"}, {Kind: "project.create", ProjectID: "project-1", Name: "x\x00y"}, {Kind: "project.create", ProjectID: "project-1", Name: strings.Repeat("x", 161)}, {Kind: "project.member.add", ProjectID: "project-1", TargetUserID: "user-1", RoomID: "room-1"}, {Kind: "room.archive", RoomID: "room-1", Name: "must-not-retitle"}, {Kind: "room.project", RoomID: "room-1", ProjectID: "C:/repo"}, {Kind: "session.revoke", SessionID: "session-1", AgentID: "agent-1"}, {Kind: "arbitrary", RoomID: "room-1"}} {
		if validateCatalogIntent(intent) {
			t.Fatalf("unsupported scope accepted: %+v", intent)
		}
	}
}

func TestCatalogPlansRejectCredentialSubstitutionAndHashEveryFrozenInput(t *testing.T) {
	s := &Store{cfg: Config{DeploymentID: "deployment-1", RequireSessionAuth: true}}
	auth := AuthContext{Deployment: "deployment-1", UserID: "user-1", SessionID: "session-1"}
	command := CatalogCommand{Auth: auth, ClientID: "operation-1", ExpectedRevision: 2, Intent: CatalogIntent{Kind: "room.rename", RoomID: "room-1", Name: "Name"}}
	plan, err := s.catalogPlan(auth, command.ClientID, catalogMutation{kind: command.Intent.Kind, cmd: command})
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*CatalogCommand){func(c *CatalogCommand) { c.Intent.Name = "Other" }, func(c *CatalogCommand) { c.ExpectedRevision++ }, func(c *CatalogCommand) { c.Intent.RoomID = "room-2" }} {
		changed := command
		edit(&changed)
		next, e := s.catalogPlan(auth, changed.ClientID, catalogMutation{kind: changed.Intent.Kind, cmd: changed})
		if e != nil || next.hash == plan.hash {
			t.Fatal("distinct frozen input reused accepted hash")
		}
	}
	for _, bad := range []AuthContext{{Deployment: "deployment-1", UserID: "user-1"}, {Deployment: "deployment-1", UserID: "user-1", DeviceGrantID: "device-grant"}, {Deployment: "deployment-1", UserID: "user-1", SessionID: "session-1", BindingGrantID: "binding-grant"}, {Deployment: "other", UserID: "user-1", SessionID: "session-1"}} {
		if _, err = s.catalogPlan(bad, command.ClientID, catalogMutation{kind: command.Intent.Kind, cmd: command}); err == nil {
			t.Fatal("catalog accepted unauthenticated or native authority")
		}
	}
}

func TestCatalogRoomMembershipDoesNotGrantProjectDiscoveryAndProjectDoesNotGrantRoom(t *testing.T) {
	plan := opPlan{userID: "actor", catalog: &catalogMutation{kind: "read", query: CatalogQuery{Resource: "rooms", ProjectID: "private-project"}}}
	d := catalogDependencies{credential: sessionDependency{kind: CredentialBrowser}, projectMemberMissing: true}
	if catalogReadAllowed(plan, &d) {
		t.Fatal("Room membership granted Project discovery")
	}
	plan.catalog.query = CatalogQuery{Resource: "room", RoomID: "private-room"}
	d.projectMemberMissing = false
	d.roomMemberMissing = true
	if catalogReadAllowed(plan, &d) {
		t.Fatal("Project membership granted Room discovery")
	}
	d.roomMemberMissing = false
	d.roomRole = "viewer"
	if !catalogReadAllowed(plan, &d) {
		t.Fatal("live Room viewer could not read metadata")
	}
	plan.catalog.kind = "room.rename"
	plan.catalog.cmd.Intent = CatalogIntent{Kind: "room.rename", RoomID: "private-room", Name: "Renamed"}
	d.roomOwner = "someone-else"
	if catalogWriteAllowed(plan, &d) {
		t.Fatal("non-Owner metadata mutation admitted")
	}
}

func TestCatalogQueryIsBoundedAndResourceSelectorsDoNotBleed(t *testing.T) {
	for _, q := range []CatalogQuery{{Resource: "rooms", Limit: 50, State: "all"}, {Resource: "room", RoomID: "room-1", Limit: 1}, {Resource: "project-members", ProjectID: "project-1", Limit: 25}, {Resource: "sessions", Limit: 25}} {
		if !validateCatalogQuery(q) {
			t.Fatalf("valid query refused: %+v", q)
		}
	}
	for _, q := range []CatalogQuery{{Resource: "rooms", Limit: 51}, {Resource: "room", Limit: 1}, {Resource: "projects", RoomID: "room-1", Limit: 1}, {Resource: "sessions", State: "all", Limit: 1}, {Resource: "users", Limit: 1}, {Resource: "rooms", After: "../other", Limit: 1}} {
		if validateCatalogQuery(q) {
			t.Fatalf("invalid query accepted: %+v", q)
		}
	}
}

func TestCatalogArchiveApprovalSetIsPartOfDiscoveryEquality(t *testing.T) {
	before := dependencyState{catalog: &catalogDependencies{}}
	after := dependencyState{catalog: &catalogDependencies{archiveApprovals: [32]byte{1}}}
	if before.equal(after) || after.equal(before) {
		t.Fatal("expanded or consumed approval set did not force a discovery restart")
	}
}
