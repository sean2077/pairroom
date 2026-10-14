package enterpriseservice

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type fakeCatalogBackend struct {
	*fakeDeviceBackend
	reads, commits, resolves int
	query                    sqlstore.CatalogQuery
	command                  sqlstore.CatalogCommand
}

func (b *fakeCatalogBackend) ReadCatalog(_ context.Context, auth sqlstore.AuthContext, q sqlstore.CatalogQuery) (sqlstore.CatalogPage, error) {
	b.reads++
	b.query = q
	b.lastAuth = auth
	return sqlstore.CatalogPage{Resource: q.Resource}, nil
}
func (b *fakeCatalogBackend) CommitCatalog(_ context.Context, c sqlstore.CatalogCommand) (sqlstore.Result, error) {
	b.commits++
	b.command = c
	return sqlstore.Result{Outcome: b.deviceOutcome, Catalog: &sqlstore.CatalogReceipt{Version: 1, Intent: c.Intent, Revision: c.ExpectedRevision + 1}}, b.deviceError
}
func (b *fakeCatalogBackend) ResolveCatalog(_ context.Context, c sqlstore.CatalogCommand) (sqlstore.Result, error) {
	b.resolves++
	b.command = c
	return sqlstore.Result{Outcome: sqlstore.OutcomeNotCommitted}, nil
}
func newCatalogHTTPFixture(t *testing.T) (*Handler, *fakeCatalogBackend, *http.Cookie, string) {
	t.Helper()
	h, base := newDeviceFixture(t)
	b := &fakeCatalogBackend{fakeDeviceBackend: base}
	h.backend = b
	cookie := loginSession(t, h)
	return h, b, cookie, sessionCSRF(t, h, cookie)
}

func TestCatalogHTTPQueryAllowanceIsResourceSpecificAndBounded(t *testing.T) {
	h, b, cookie, _ := newCatalogHTTPFixture(t)
	w := request(t, h, "GET", "/enterprise/api/v1/catalog/rooms?limit=2&after=room-1&state=archived", "", cookie, "")
	if w.Code != 200 || b.reads != 1 || b.query.Limit != 2 || b.query.After != "room-1" || b.lastAuth.SessionID == "" {
		t.Fatalf("valid catalog request: %d %s", w.Code, w.Body.String())
	}
	for _, path := range []string{"/enterprise/api/v1/catalog/rooms?limit=0", "/enterprise/api/v1/catalog/rooms?limit=51", "/enterprise/api/v1/catalog/rooms?limit=01", "/enterprise/api/v1/catalog/rooms?limit=2&limit=3", "/enterprise/api/v1/catalog/rooms?user_id=other", "/enterprise/api/v1/catalog/devices?project_id=private", "/enterprise/api/v1/catalog/room-members?room_id=room-1&project_id=project-1", "/enterprise/api/v1/catalog/room-members", "/enterprise/api/v1/rooms/room-1?state=all", "/enterprise/api/v1/catalog/rooms?"} {
		w = request(t, h, "GET", path, "", cookie, "")
		if w.Code < 400 || b.reads != 1 {
			t.Fatalf("bad query reached SQL: %s %d", path, w.Code)
		}
	}
}

func TestCatalogHTTPMutationsAndResolutionUseFrozenBrowserAuthority(t *testing.T) {
	h, b, cookie, csrf := newCatalogHTTPFixture(t)
	body := `{"client_id":"rename-1","expected_revision":3,"intent":{"kind":"room.rename","room_id":"room-1","name":"New title"}}`
	w := request(t, h, "POST", "/enterprise/api/v1/catalog/commands", body, cookie, csrf)
	if w.Code != 200 || b.commits != 1 || b.command.Auth.SessionID == "" || b.command.Auth.DeviceGrantID != "" || b.command.Intent.Name != "New title" {
		t.Fatalf("catalog mutation: %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "POST", "/enterprise/api/v1/catalog/resolve", body, cookie, csrf)
	if w.Code != 409 || b.commits != 1 || b.resolves != 1 {
		t.Fatal("Resolve retried a mutation")
	}
	for _, body := range []string{`{"client_id":"forged","auth":{"UserID":"other"},"intent":{"kind":"project.create"}}`, strings.Replace(body, `"name":"New title"`, `"name":"New title","path":"C:/private"`, 1)} {
		w = request(t, h, "POST", "/enterprise/api/v1/catalog/commands", body, cookie, csrf)
		if w.Code != 400 || b.commits != 1 {
			t.Fatal("untyped fields reached catalog")
		}
	}
	w = request(t, h, "POST", "/enterprise/api/v1/catalog/commands", body, cookie, "")
	if w.Code != 403 || b.commits != 1 {
		t.Fatal("missing CSRF reached catalog mutation")
	}
}

func TestRoomTemplateFreezesServerCollaborationWithoutCredentialOrSQL(t *testing.T) {
	h, b, cookie, csrf := newCatalogHTTPFixture(t)
	w := request(t, h, "POST", "/enterprise/api/v1/room-template", `{"name":" New Room ","collaboration":{"mode":"default"}}`, cookie, csrf)
	var out struct {
		Intent model.EnterpriseRoomIntent `json:"intent"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Intent.Name != "New Room" || out.Intent.Collaboration.Instructions != model.DefaultCollaborationInstructions || model.ValidateEnterpriseRoomIntent(out.Intent) != nil || b.commits != 0 || b.reads != 0 {
		t.Fatalf("template is not server-frozen: %d %s", w.Code, w.Body.String())
	}
	w = request(t, h, "POST", "/enterprise/api/v1/room-template", `{"name":"Room","runtime":"codex"}`, cookie, csrf)
	if w.Code != 400 {
		t.Fatal("Room creation template accepted Runtime selection")
	}
}

func TestDeviceRevocationResolutionNeverPerformsRevocation(t *testing.T) {
	h, b, cookie, csrf := newCatalogHTTPFixture(t)
	w := request(t, h, "POST", "/enterprise/api/v1/devices/device-1/resolve-revoke", `{"client_id":"revoke-1"}`, cookie, csrf)
	if w.Code != 200 || b.resolveRevokeCalls != 1 || b.revokeDeviceCalls != 0 || b.lastAuth.SessionID == "" {
		t.Fatalf("device resolve: %d %s", w.Code, w.Body.String())
	}
}
