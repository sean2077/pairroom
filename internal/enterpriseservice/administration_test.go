package enterpriseservice

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/sqlstore"
)

type fakeAdministrationBackend struct {
	*fakeBackend
	reads, commits, resolves int
	command                  sqlstore.AdministrationCommand
	query                    sqlstore.AdministrationQuery
	corrupt                  bool
}

func (b *fakeAdministrationBackend) ReadAdministration(_ context.Context, auth sqlstore.AuthContext, query sqlstore.AdministrationQuery) (sqlstore.AdministrationPage, error) {
	b.reads++
	b.lastAuth = auth
	b.query = query
	return sqlstore.AdministrationPage{Resource: query.Resource}, nil
}
func (b *fakeAdministrationBackend) CommitAdministration(_ context.Context, command sqlstore.AdministrationCommand) (sqlstore.Result, error) {
	b.commits++
	b.command = command
	receipt := &sqlstore.ControlReceipt{Version: 1, Kind: command.Intent.Kind, Seq: 1, UserID: command.Auth.UserID, TargetUserID: command.Intent.TargetUserID, Enabled: command.Intent.Enabled}
	if b.corrupt {
		receipt.TargetUserID = "another-user"
	}
	return sqlstore.Result{Outcome: sqlstore.OutcomeCommitted, Control: receipt}, nil
}
func (b *fakeAdministrationBackend) ResolveAdministration(_ context.Context, command sqlstore.AdministrationCommand) (sqlstore.Result, error) {
	b.resolves++
	b.command = command
	return sqlstore.Result{Outcome: sqlstore.OutcomeNotCommitted}, nil
}
func newAdministrationHTTPFixture(t *testing.T) (*Handler, *fakeAdministrationBackend, *http.Cookie, string) {
	h, _, base := newHandlerFixture(t)
	b := &fakeAdministrationBackend{fakeBackend: base}
	h.backend = b
	cookie := loginSession(t, h)
	return h, b, cookie, sessionCSRF(t, h, cookie)
}
func TestAdministrationHTTPQueriesAreBoundedAndUseCurrentBrowserAuthority(t *testing.T) {
	h, b, cookie, _ := newAdministrationHTTPFixture(t)
	for _, resource := range []string{"users", "admissions", "administrators", "connections"} {
		w := request(t, h, "GET", administrationPrefix+resource+"?limit=1", "", cookie, "")
		if w.Code != 200 || b.query.Resource != resource || b.query.Limit != 1 || b.lastAuth.SessionID == "" {
			t.Fatalf("valid directory %s: %d", resource, w.Code)
		}
	}
	reads := b.reads
	for _, query := range []string{"?limit=0", "?limit=51", "?limit=01", "?limit=1&limit=2", "?user_id=other", "?after=++", "?"} {
		w := request(t, h, "GET", administrationPrefix+"users"+query, "", cookie, "")
		if w.Code < 400 || b.reads != reads {
			t.Fatalf("invalid directory reached backend: %s", query)
		}
	}
	for hash, session := range b.sessions {
		session.Administrator = false
		b.sessions[hash] = session
	}
	w := request(t, h, "GET", administrationPrefix+"users", "", cookie, "")
	if w.Code != 403 || b.reads != reads {
		t.Fatal("nonadmin directory read reached backend")
	}
}
func TestAdministrationHTTPStrictIntentCSRFOriginalResolveAndMatchingReceipt(t *testing.T) {
	h, b, cookie, csrf := newAdministrationHTTPFixture(t)
	body := `{"client_id":"disable-user","intent":{"kind":"user.status","target_user_id":"target-user","enabled":false}}`
	w := request(t, h, "POST", administrationPrefix+"commands", body, cookie, csrf)
	if w.Code != 200 || b.commits != 1 || b.command.Auth.SessionID == "" || b.command.Intent.Enabled {
		t.Fatalf("typed control: %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "POST", administrationPrefix+"resolve", body, cookie, csrf)
	if w.Code != 409 || b.resolves != 1 || b.commits != 1 || b.command.ClientID != "disable-user" {
		t.Fatal("resolution changed identity or wrote a control")
	}
	for _, bad := range []string{strings.Replace(body, `,"enabled":false`, "", 1), strings.Replace(body, `"enabled":false`, `"enabled":null`, 1), strings.Replace(body, `"enabled":false`, `"enabled":false,"auth_time":123`, 1), strings.Replace(body, `"client_id"`, `"auth":{},"client_id"`, 1)} {
		w = request(t, h, "POST", administrationPrefix+"commands", bad, cookie, csrf)
		if w.Code != 400 || b.commits != 1 {
			t.Fatal("malformed administration input reached backend")
		}
	}
	w = request(t, h, "POST", administrationPrefix+"commands", body, cookie, "")
	if w.Code != 403 || b.commits != 1 {
		t.Fatal("CSRF-free control reached backend")
	}
	b.corrupt = true
	w = request(t, h, "POST", administrationPrefix+"commands", body, cookie, csrf)
	if w.Code != 503 || strings.Contains(w.Body.String(), "another-user") {
		t.Fatal("mismatched receipt disclosed or confirmed")
	}
}
