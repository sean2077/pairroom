package sqlstore

import (
	"context"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func roomCreateCommand(t *testing.T) RoomCreateCommand {
	t.Helper()
	intent, err := model.PrepareEnterpriseRoomIntent("Enterprise discussion", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	return RoomCreateCommand{Auth: AuthContext{Deployment: "dep-test", UserID: "u-owner"}, RoomID: "room-created", ClientID: "create-1", Intent: intent}
}

// A submitted creation intent is already frozen. Invalid identifiers and
// unfrozen policy are rejected before a connection or transaction is touched.
func TestRoomCreateRejectsInvalidFrozenRequests(t *testing.T) {
	store := &Store{cfg: Config{DeploymentID: "dep-test"}}
	tests := []struct {
		name   string
		change func(*RoomCreateCommand)
	}{
		{"different deployment", func(c *RoomCreateCommand) { c.Auth.Deployment = "other" }},
		{"empty principal", func(c *RoomCreateCommand) { c.Auth.UserID = "" }},
		{"invalid principal", func(c *RoomCreateCommand) { c.Auth.UserID = "user/1" }},
		{"empty room", func(c *RoomCreateCommand) { c.RoomID = "" }},
		{"invalid room", func(c *RoomCreateCommand) { c.RoomID = "room/1" }},
		{"empty client", func(c *RoomCreateCommand) { c.ClientID = "" }},
		{"invalid client", func(c *RoomCreateCommand) { c.ClientID = "client/1" }},
		{"unfrozen policy", func(c *RoomCreateCommand) { c.Intent.Collaboration = model.Collaboration{} }},
		{"unsupported policy", func(c *RoomCreateCommand) { c.Intent.Collaboration.Version = 999 }},
		{"changed default text", func(c *RoomCreateCommand) { c.Intent.Collaboration.Instructions = "invented default" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := roomCreateCommand(t)
			test.change(&cmd)
			res, err := store.CommitRoomCreate(context.Background(), cmd)
			if got := kernelErr(t, res, err); got.Outcome != OutcomeRefused || got.Class != ClassInvalidRequest {
				t.Fatalf("commit admission: %+v", res)
			}
			res, err = store.ResolveRoomCreate(context.Background(), cmd.Auth, cmd.RoomID, cmd.ClientID, cmd.Intent)
			if got := kernelErr(t, res, err); got.Outcome != OutcomeRefused || got.Class != ClassInvalidRequest {
				t.Fatalf("resolve admission: %+v", res)
			}
		})
	}
}

func TestRoomCreateHashBindsFullFrozenIntentAndIdentity(t *testing.T) {
	store := &Store{cfg: Config{DeploymentID: "dep-test"}}
	cmd := roomCreateCommand(t)
	base, class := store.roomCreatePlan(cmd)
	if class != "" {
		t.Fatal(class)
	}
	changes := []func(*RoomCreateCommand){
		func(c *RoomCreateCommand) { c.RoomID = "room-other" },
		func(c *RoomCreateCommand) { c.ClientID = "create-other" },
		func(c *RoomCreateCommand) { c.Auth.UserID = "u-other" },
		func(c *RoomCreateCommand) { c.Intent.Name = "different name" },
		func(c *RoomCreateCommand) {
			c.Intent.Collaboration = model.Collaboration{Version: 2, Mode: "custom", Instructions: "custom policy"}
		},
		func(c *RoomCreateCommand) {
			c.Intent.Collaboration, _ = (model.Collaboration{Version: 1}).ForCreation()
		},
	}
	for i, change := range changes {
		changed := cmd
		change(&changed)
		plan, class := store.roomCreatePlan(changed)
		if class != "" || plan.hash == base.hash {
			t.Fatalf("changed identity or intent %d: class=%q hash unchanged=%v", i, class, plan.hash == base.hash)
		}
	}
}
