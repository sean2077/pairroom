package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
)

func unitLogin() IdentityLoginCommand {
	return IdentityLoginCommand{ClientID: "login-unit", Completion: enterpriseauth.Completion{Purpose: enterpriseauth.PurposeLogin, Identity: enterpriseauth.VerifiedIdentity{ConnectionID: "corporate", Issuer: "https://identity.example.test/issuer", Subject: "exact-subject"}}, Session: BrowserSessionCandidate{SessionID: "session-unit", TokenHash: sha256.Sum256([]byte("synthetic-token")), CSRFHash: sha256.Sum256([]byte("synthetic-csrf")), ExpiresAt: time.Date(2026, 10, 14, 1, 0, 0, 123456789, time.UTC)}}
}

func TestIdentityLoginRejectsUntrustedShapeBeforeDatabase(t *testing.T) {
	store := &Store{cfg: Config{DeploymentID: "dep-test"}}
	for _, change := range []func(*IdentityLoginCommand){
		func(c *IdentityLoginCommand) { c.ClientID = "" },
		func(c *IdentityLoginCommand) { c.Completion.Identity.Subject = "" },
		func(c *IdentityLoginCommand) { c.Completion.Identity.Issuer = "http://untrusted.test" },
		func(c *IdentityLoginCommand) { c.Completion.Purpose = enterpriseauth.PurposeReauthenticate },
		func(c *IdentityLoginCommand) { c.Completion.FreshReauthentication = true },
		func(c *IdentityLoginCommand) { c.Completion.CorrelationID = "privileged-action" },
		func(c *IdentityLoginCommand) { c.Session.TokenHash = [32]byte{} },
		func(c *IdentityLoginCommand) { c.Session.CSRFHash = [32]byte{} },
		func(c *IdentityLoginCommand) { c.Session.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		cmd := unitLogin()
		change(&cmd)
		result, err := store.AdmitIdentity(context.Background(), cmd)
		if got := kernelErr(t, result, err); got.Class != ClassInvalidRequest {
			t.Fatalf("invalid login shape: %+v", result)
		}
	}
}

func TestIdentityLoginHashIncludesBothVerifiersAndExactCandidate(t *testing.T) {
	store := &Store{cfg: Config{DeploymentID: "dep-test"}}
	base, class := store.loginPlan(unitLogin())
	if class != "" {
		t.Fatal(class)
	}
	for _, change := range []func(*IdentityLoginCommand){
		func(c *IdentityLoginCommand) { c.Session.TokenHash = sha256.Sum256([]byte("other-token")) },
		func(c *IdentityLoginCommand) { c.Session.CSRFHash = sha256.Sum256([]byte("other-csrf")) },
		func(c *IdentityLoginCommand) { c.Session.SessionID = "session-other" },
		func(c *IdentityLoginCommand) { c.Session.ExpiresAt = c.Session.ExpiresAt.Add(time.Second) },
		func(c *IdentityLoginCommand) { c.Completion.Identity.Subject = "different-subject" },
	} {
		cmd := unitLogin()
		change(&cmd)
		plan, class := store.loginPlan(cmd)
		if class != "" || base.hash == plan.hash {
			t.Fatal("changed candidate or identity retained its hash")
		}
	}
	again, class := store.loginPlan(unitLogin())
	if class != "" || again.hash != base.hash || again.control.newUserID == base.control.newUserID {
		t.Fatal("generated User identity changed stable intent or was reused")
	}
	encoded, err := json.Marshal(unitLogin().Session)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "TokenHash") || strings.Contains(string(encoded), "CSRFHash") {
		t.Fatal("candidate serialization exposed verifier material")
	}
}

func TestIdentityConfigurationRequiresExactOperatorSelection(t *testing.T) {
	identity := unitLogin().Completion.Identity
	config := IdentityConfig{Connections: []TrustConnectionConfig{{ConnectionID: identity.ConnectionID, Issuer: identity.Issuer}}, Bootstrap: identity}
	if _, err := freezeIdentityConfig(config); err != nil {
		t.Fatal(err)
	}
	config.Bootstrap.Issuer = "https://different.example.test"
	if _, err := freezeIdentityConfig(config); err == nil {
		t.Fatal("bootstrap silently crossed trust connections")
	}
	if (&Store{}).RequiresSessionAuth() {
		t.Fatal("trusted-only fixture store advertised public credentials")
	}
	if !(&Store{cfg: Config{RequireSessionAuth: true}}).RequiresSessionAuth() {
		t.Fatal("public adapter cannot verify mandatory credential mode")
	}
}
