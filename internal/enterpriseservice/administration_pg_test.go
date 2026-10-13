package enterpriseservice

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/sqlstore"
)

func TestAdministrationHTTPPGActualOIDCControlReceiptsSessionRevocationAndNoRoomAccess(t *testing.T) {
	f := newHTTPPGFixture(t)
	operator, member := f.browser("operator"), f.browser("member-one")
	f.login(operator)
	f.login(member)
	admin, user := f.session(operator), f.session(member)
	f.createRoom(member, user.CSRF, "private-admin-test-room")
	for _, resource := range []string{"users", "administrators", "admissions", "connections"} {
		status, body := f.get(operator, administrationPrefix+resource+"?limit=1")
		var page sqlstore.AdministrationPage
		if status != 200 || json.Unmarshal(body, &page) != nil || page.Resource != resource || strings.Contains(string(body), "private-admin-test-room") {
			t.Fatalf("actual directory %s: %d %s", resource, status, body)
		}
		status, _ = f.get(member, administrationPrefix+resource)
		if status != 403 {
			t.Fatalf("ordinary User read administration: %d", status)
		}
		if resource == "users" {
			if len(page.Users) != 1 || page.Next == "" {
				t.Fatal("bounded User cursor missing")
			}
			status, body = f.get(operator, administrationPrefix+resource+"?limit=1&after="+page.Next)
			var next sqlstore.AdministrationPage
			if status != 200 || json.Unmarshal(body, &next) != nil || len(next.Users) != 1 || next.Users[0].UserID == page.Users[0].UserID {
				t.Fatal("actual User keyset page did not advance")
			}
		}
	}
	if status, _ := f.get(operator, "/enterprise/api/v1/rooms/private-admin-test-room"); status != 403 {
		t.Fatal("administrator directory conferred Room content authority")
	}
	command := func(key string, intent sqlstore.AdministrationIntent) map[string]any {
		return map[string]any{"client_id": key, "intent": intent}
	}
	last := command("last-admin-refusal", sqlstore.AdministrationIntent{Kind: "administrator.set", TargetUserID: admin.UserID, Enabled: false})
	status, body := f.post(operator, administrationPrefix+"commands", last, admin.CSRF)
	if status != 403 || !strings.Contains(string(body), `"last_administrator"`) {
		t.Fatalf("last admin refusal: %d %s", status, body)
	}
	promote := command("promote-member", sqlstore.AdministrationIntent{Kind: "administrator.set", TargetUserID: user.UserID, Enabled: true})
	status, original := f.post(operator, administrationPrefix+"commands", promote, admin.CSRF)
	if status != 200 {
		t.Fatalf("promote User: %d %s", status, original)
	}
	if status, _ = f.get(member, "/enterprise/api/v1/session"); status != 401 {
		t.Fatal("role grant revived old session")
	}
	status, body = f.post(operator, administrationPrefix+"resolve", promote, admin.CSRF)
	var accepted, replayed struct {
		Outcome string                  `json:"outcome"`
		Receipt sqlstore.ControlReceipt `json:"receipt"`
	}
	if status != 200 || json.Unmarshal(original, &accepted) != nil || json.Unmarshal(body, &replayed) != nil || replayed.Outcome != "replayed" || replayed.Receipt != accepted.Receipt {
		t.Fatal("actual resolve changed accepted receipt")
	}
	f.login(member)
	current := f.session(member)
	if !current.Administrator {
		t.Fatal("fresh OIDC login omitted promoted role")
	}
	disable := command("disable-new-admin", sqlstore.AdministrationIntent{Kind: "user.status", TargetUserID: user.UserID, Enabled: false})
	status, body = f.post(operator, administrationPrefix+"commands", disable, admin.CSRF)
	if status != 200 {
		t.Fatalf("disable User: %d %s", status, body)
	}
	if status, _ = f.get(member, administrationPrefix+"users"); status != 401 {
		t.Fatal("disabled User retained browser directory authority")
	}
	removeRole := command("remove-disabled-role", sqlstore.AdministrationIntent{Kind: "administrator.set", TargetUserID: user.UserID, Enabled: false})
	status, body = f.post(operator, administrationPrefix+"commands", removeRole, admin.CSRF)
	if status != 200 {
		t.Fatalf("safe disabled role revoke: %d %s", status, body)
	}
	enable := command("enable-without-role", sqlstore.AdministrationIntent{Kind: "user.status", TargetUserID: user.UserID, Enabled: true})
	status, body = f.post(operator, administrationPrefix+"commands", enable, admin.CSRF)
	if status != 200 {
		t.Fatalf("enable User: %d %s", status, body)
	}
	if status, _ = f.get(member, "/enterprise/api/v1/session"); status != 401 {
		t.Fatal("enable restored revoked credential")
	}
	f.login(member)
	if f.session(member).Administrator {
		t.Fatal("enable recreated removed administrator role")
	}
	identity := sqlstore.AdministrationIdentity{ConnectionID: "fixture-connection", Issuer: f.idp.server.URL, Subject: " Exact case-sensitive subject "}
	admission := command("exact-admin-admission", sqlstore.AdministrationIntent{Kind: "identity.admission", Identity: identity, Enabled: true})
	status, body = f.post(operator, administrationPrefix+"commands", admission, admin.CSRF)
	if status != 200 {
		t.Fatalf("exact admission: %d %s", status, body)
	}
	status, body = f.get(operator, administrationPrefix+"admissions?limit=50")
	var entries sqlstore.AdministrationPage
	if status != http.StatusOK || json.Unmarshal(body, &entries) != nil {
		t.Fatal("exact admission directory unavailable")
	}
	found := false
	for _, entry := range entries.Admissions {
		if entry.AdministrationIdentity == identity && entry.Enabled && entry.UserID == "" {
			found = true
		}
	}
	if !found {
		t.Fatal("exact unmapped admission normalized or omitted")
	}
}
