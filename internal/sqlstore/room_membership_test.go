package sqlstore

import (
	"context"
	"math"
	"testing"
)

func membershipCommand() RoomMembershipCommand {
	return RoomMembershipCommand{
		Auth: AuthContext{Deployment: "dep-test", UserID: "u-owner"}, RoomID: "room-membership", ClientID: "membership-1", ExpectedRevision: 1,
		Intent: RoomMembershipIntent{TargetUserID: "u-target", Action: MembershipAdmit, Role: RoomMember},
	}
}

func TestRoomMembershipAdmissionAndHash(t *testing.T) {
	store := &Store{cfg: Config{DeploymentID: "dep-test"}}
	for _, change := range []func(*RoomMembershipCommand){
		func(c *RoomMembershipCommand) { c.Auth.Deployment = "other" },
		func(c *RoomMembershipCommand) { c.Auth.UserID = "" },
		func(c *RoomMembershipCommand) { c.RoomID = "room/other" },
		func(c *RoomMembershipCommand) { c.ClientID = "" },
		func(c *RoomMembershipCommand) { c.ExpectedRevision = math.MaxUint64 },
		func(c *RoomMembershipCommand) { c.Intent.TargetUserID = "u/target" },
		func(c *RoomMembershipCommand) { c.Intent.Action = "transfer" },
		func(c *RoomMembershipCommand) { c.Intent.Role = "owner" },
		func(c *RoomMembershipCommand) { c.Intent.Action = MembershipRemove },
	} {
		cmd := membershipCommand()
		change(&cmd)
		result, err := store.CommitRoomMembership(context.Background(), cmd)
		if got := kernelErr(t, result, err); got.Class != ClassInvalidRequest {
			t.Fatalf("invalid admission: %+v", result)
		}
	}
	base, class := store.roomMembershipPlan(membershipCommand())
	if class != "" || base.ownershipLock() != lockExclusive {
		t.Fatalf("valid plan: class=%s lock=%s", class, base.ownershipLock())
	}
	for _, change := range []func(*RoomMembershipCommand){
		func(c *RoomMembershipCommand) { c.Auth.UserID = "u-other" },
		func(c *RoomMembershipCommand) { c.RoomID = "room-other" },
		func(c *RoomMembershipCommand) { c.ClientID = "other" },
		func(c *RoomMembershipCommand) { c.Intent.TargetUserID = "u-other" },
		func(c *RoomMembershipCommand) { c.Intent.Action = MembershipChangeRole },
		func(c *RoomMembershipCommand) { c.Intent.Role = RoomViewer },
	} {
		cmd := membershipCommand()
		change(&cmd)
		plan, class := store.roomMembershipPlan(cmd)
		if class != "" || plan.hash == base.hash {
			t.Fatal("changed frozen intent/key failed to alter its hash")
		}
	}
	cmd := membershipCommand()
	cmd.ExpectedRevision = 123
	plan, class := store.roomMembershipPlan(cmd)
	if class != "" || plan.hash != base.hash {
		t.Fatal("expected revision became part of stable intent")
	}
}
