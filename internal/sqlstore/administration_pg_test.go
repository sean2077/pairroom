package sqlstore

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

func TestAdministrationPGDirectoryRequiresLiveAdminAndPagesWithoutRoomAccess(t *testing.T) {
	g := newGovernanceFixture(t, false)
	ctx := context.Background()
	s := g.f.store
	for _, resource := range []string{"users", "administrators", "admissions", "connections"} {
		for _, auth := range []AuthContext{g.owner.Auth, {Deployment: g.admin.Auth.Deployment, UserID: g.admin.Auth.UserID, SessionID: g.owner.Auth.SessionID}, {Deployment: g.admin.Auth.Deployment, UserID: g.admin.Auth.UserID, DeviceGrantID: "forged-native-selector"}} {
			page, err := s.ReadAdministration(ctx, auth, AdministrationQuery{Resource: resource, Limit: 1})
			if err == nil || page.Resource != "" {
				t.Fatalf("directory exposed %s to a nonadmin/live-credential mismatch", resource)
			}
		}
		first, err := s.ReadAdministration(ctx, g.admin.Auth, AdministrationQuery{Resource: resource, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(first)
		for _, private := range []string{"room_id", "token_hash", "csrf_hash", "client_secret", "restore_epoch"} {
			if strings.Contains(string(encoded), private) {
				t.Fatalf("directory leaked private field %s", private)
			}
		}
		if resource == "users" {
			if len(first.Users) != 1 || first.Next == "" {
				t.Fatal("User page was unbounded or lacked cursor")
			}
			second, err := s.ReadAdministration(ctx, g.admin.Auth, AdministrationQuery{Resource: resource, Limit: 1, After: first.Next})
			if err != nil || len(second.Users) != 1 || second.Users[0].UserID == first.Users[0].UserID {
				t.Fatal("User keyset page did not advance")
			}
		}
	}
	if snapshot, err := s.ReadonlySnapshot(ctx, g.admin.Auth, g.intent.RoomID); err == nil || len(snapshot.Events) != 0 {
		t.Fatal("administration directory conferred Room content membership")
	}
}

func TestAdministrationPGCommandsReuseOriginalControlReceiptsAndLastAdminProtection(t *testing.T) {
	g := newGovernanceFixture(t, false)
	ctx := context.Background()
	s := g.f.store
	removeSelf := AdministrationCommand{Auth: g.admin.Auth, ClientID: "remove-last-administrator", Intent: AdministrationIntent{Kind: controlAdminSet, TargetUserID: g.admin.Auth.UserID, Enabled: false}}
	result, err := s.CommitAdministration(ctx, removeSelf)
	if err == nil || result.Class != ClassLastAdministrator {
		t.Fatalf("last administrator protection: %+v %v", result, err)
	}
	if resolved, e := s.ResolveAdministration(ctx, removeSelf); e != nil || resolved.Outcome != OutcomeNotCommitted {
		t.Fatal("refused last-admin change had an accepted receipt")
	}
	grant := AdministrationCommand{Auth: g.admin.Auth, ClientID: "grant-owner-administrator", Intent: AdministrationIntent{Kind: controlAdminSet, TargetUserID: g.owner.Auth.UserID, Enabled: true}}
	// Acceptance through the original API is recovered through the new HTTP
	// boundary's wrapper, proving there is no second namespace/hash scheme.
	original, err := s.SetAdministrator(ctx, grant.Auth, grant.ClientID, grant.Intent.TargetUserID, true)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveAdministration(ctx, grant)
	if err != nil || resolved.Outcome != OutcomeReplayed || resolved.Control == nil || resolved.Control.Seq != original.Control.Seq {
		t.Fatal("administration wrapper did not recover original control receipt")
	}
	if _, err = s.ReadAdministration(ctx, g.owner.Auth, AdministrationQuery{Resource: "users"}); err == nil {
		t.Fatal("role grant revived the target's old revoked session")
	}
	_, newAdmin := identityPGLogin(t, g.f, "member-one", "admin-after-role-grant")
	if _, err = s.ReadAdministration(ctx, newAdmin.Auth, AdministrationQuery{Resource: "users"}); err != nil {
		t.Fatalf("freshly authenticated promoted admin denied: %v", err)
	}
	if accepted, e := s.CommitAdministration(ctx, removeSelf); e != nil || accepted.Outcome != OutcomeCommitted {
		t.Fatalf("explicit demotion with another eligible admin refused: %+v %v", accepted, e)
	}
	if page, e := s.ReadAdministration(ctx, g.admin.Auth, AdministrationQuery{Resource: "users"}); e == nil || page.Resource != "" {
		t.Fatal("demoted/revoked administrator read private directory")
	}
	if old, e := s.ResolveAdministration(ctx, grant); e == nil || old.Outcome != OutcomeRefused || old.Control != nil {
		t.Fatal("former admin recovered a private receipt with revoked authority")
	}
}

func TestAdministrationPGExactAdmissionAndUserEnableDoNotReviveCredentials(t *testing.T) {
	g := newGovernanceFixture(t, false)
	ctx := context.Background()
	s := g.f.store
	identity := AdministrationIdentity{ConnectionID: "corporate", Issuer: "https://idp.example.test/issuer", Subject: "Case-sensitive exact subject "}
	command := AdministrationCommand{Auth: g.admin.Auth, ClientID: "admit-exact-subject", Intent: AdministrationIntent{Kind: controlAdmission, Identity: identity, Enabled: true}}
	verified := enterpriseauth.VerifiedIdentity{ConnectionID: identity.ConnectionID, Issuer: identity.Issuer, Subject: identity.Subject}
	original, err := s.SetIdentityAdmission(ctx, command.Auth, command.ClientID, verified, true)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := s.ResolveAdministration(ctx, command)
	if err != nil || resolved.Outcome != OutcomeReplayed || resolved.Control.IdentityHash != original.Control.IdentityHash {
		t.Fatal("admission wrapper changed exact key or original hash")
	}
	changed := command
	changed.Intent.Identity.Subject = strings.TrimSpace(identity.Subject)
	if result, e := s.ResolveAdministration(ctx, changed); e == nil || result.Class != ClassPayloadConflict {
		t.Fatal("subject normalization reused an accepted receipt")
	}
	page, err := s.ReadAdministration(ctx, g.admin.Auth, AdministrationQuery{Resource: "admissions", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range page.Admissions {
		if entry.AdministrationIdentity == identity {
			found = entry.Enabled && entry.UserID == ""
		}
	}
	if !found {
		t.Fatal("admission directory omitted the exact unmapped identity")
	}
	disable := AdministrationCommand{Auth: g.admin.Auth, ClientID: "disable-owner-admin-ui", Intent: AdministrationIntent{Kind: controlUserStatus, TargetUserID: g.owner.Auth.UserID, Enabled: false}}
	if _, err = s.CommitAdministration(ctx, disable); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateSession(ctx, g.ownerLogin.Session.TokenHash); err == nil {
		t.Fatal("disabled user retained browser credential")
	}
	disable.ClientID = "reenable-owner-admin-ui"
	disable.Intent.Enabled = true
	if _, err = s.CommitAdministration(ctx, disable); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthenticateSession(ctx, g.ownerLogin.Session.TokenHash); err == nil {
		t.Fatal("reenabling restored a revoked browser credential")
	}
	_, reenabled := identityPGLogin(t, g.f, "member-one", "admin-ui-reenabled-owner")
	if _, err = s.ReadonlySnapshot(ctx, reenabled.Auth, g.intent.RoomID); err != nil {
		t.Fatal("reenabling failed to restore unchanged Owner authority")
	}
}
