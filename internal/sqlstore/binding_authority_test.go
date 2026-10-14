package sqlstore

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func bindingUnitCommand() BindingRequestCommand {
	return BindingRequestCommand{Auth: AuthContext{Deployment: "dep", UserID: "user", DeviceGrantID: "device-grant"}, ClientID: "operation", RequestID: "request", RoomID: "room", AgentID: "agent", BindingID: "binding", Slot: model.ActorSlot1, ObservedGeneration: 1, ParkEnabled: true, ProofHash: sha256.Sum256([]byte("private-proof")), Grant: BindingGrantCandidate{GrantID: "grant", TokenHash: sha256.Sum256([]byte("private-token"))}}
}
func TestBindingAuthorityClosedAdmissionAndStableFamilyHash(t *testing.T) {
	s := &Store{cfg: Config{DeploymentID: "dep"}}
	cmd := bindingUnitCommand()
	plan, class := s.bindingRequestPlan(cmd)
	if class != "" {
		t.Fatal(class)
	}
	d := &bindingDependencies{credential: sessionDependency{deviceID: "device", familyID: "family"}}
	original := bindingIntentHash(plan, d)
	for _, change := range []func(*BindingRequestCommand){
		func(c *BindingRequestCommand) { c.Slot = model.ActorSlot2 }, func(c *BindingRequestCommand) { c.ObservedGeneration++ }, func(c *BindingRequestCommand) { c.Replace = true },
		func(c *BindingRequestCommand) { c.ParkEnabled = false }, func(c *BindingRequestCommand) { c.AgentID = "other-agent" }, func(c *BindingRequestCommand) { c.BindingID = "other-binding" },
		func(c *BindingRequestCommand) { c.Grant.GrantID = "other-grant" }, func(c *BindingRequestCommand) { c.Grant.TokenHash[0]++ }, func(c *BindingRequestCommand) { c.ProofHash[0]++ },
	} {
		copy := cmd
		change(&copy)
		p, class := s.bindingRequestPlan(copy)
		if class != "" {
			t.Fatal(class)
		}
		if bindingIntentHash(p, d) == original {
			t.Fatalf("stable intent omitted field: %+v", copy)
		}
	}
	rotated := cmd
	rotated.Auth.DeviceGrantID = "rotated-grant"
	p, class := s.bindingRequestPlan(rotated)
	if class != "" || bindingIntentHash(p, d) != original {
		t.Fatal("access rotation changed original Device family operation")
	}
	changedFamily := *d
	changedFamily.credential.familyID = "other-family"
	if bindingIntentHash(plan, &changedFamily) == original {
		t.Fatal("family scope omitted from hash")
	}
	for _, auth := range []AuthContext{{Deployment: "dep", UserID: "user", SessionID: "browser"}, {Deployment: "dep", UserID: "user", BindingGrantID: "binding-grant"}, {Deployment: "dep", UserID: "user", DeviceGrantID: "device", SessionID: "browser"}} {
		copy := cmd
		copy.Auth = auth
		if _, class := s.bindingRequestPlan(copy); class != ClassAuthorization {
			t.Fatalf("non-Device request admitted: %+v %s", auth, class)
		}
	}
	decision := BindingDecisionCommand{Auth: cmd.Auth, ClientID: "decide", RoomID: cmd.RoomID, RequestID: cmd.RequestID, Approve: true}
	if _, class := s.bindingDecisionPlan(decision); class != ClassAuthorization {
		t.Fatal("Device became Owner browser")
	}
}

func TestBindingAuthorityReceiptShapeHasNoCredentialChannel(t *testing.T) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	i := BindingRequestInfo{RequestID: "request", RoomID: "room", Slot: model.ActorSlot1, ObservedGeneration: 1, AgentID: "agent", Runtime: model.RuntimeClaude, BindingID: "binding", UserID: "user", DeviceID: "device", Status: "pending", RequestedAt: at, ExpiresAt: at.Add(bindingRequestTTL)}
	r := BindingAuthorityReceipt{Version: 1, Kind: bindingRequest, Request: &i}
	if err := validateBindingAuthorityReceipt(r); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*BindingAuthorityReceipt){func(r *BindingAuthorityReceipt) { r.GrantID = "grant" }, func(r *BindingAuthorityReceipt) { r.Seq = 1 }, func(r *BindingAuthorityReceipt) { r.Revision = 1 }, func(r *BindingAuthorityReceipt) { r.ExpiresAt = at.Add(time.Hour) }, func(r *BindingAuthorityReceipt) { r.Kind = "raw" }} {
		copy := r
		change(&copy)
		if validateBindingAuthorityReceipt(copy) == nil {
			t.Fatalf("unrelated result fields accepted: %+v", copy)
		}
	}
	data, _ := json.Marshal(r)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"proof_hash", "token_hash", "session_id", "credential", "family_id"} {
		if _, ok := fields[name]; ok {
			t.Fatalf("receipt contains private field %s", name)
		}
	}
}
