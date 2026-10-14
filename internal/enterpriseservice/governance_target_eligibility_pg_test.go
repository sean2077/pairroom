package enterpriseservice

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseauth"
	"github.com/sean2077/pairroom/internal/sqlstore"
)

type httpTargetFixture struct {
	f                       *httpPGFixture
	owner, target, operator *http.Client
	o, m, a                 httpPGSession
	intent                  sqlstore.GovernanceIntent
}

func newHTTPTargetFixture(t *testing.T) httpTargetFixture {
	t.Helper()
	f := newHTTPPGFixture(t)
	f.providerConfig.Reauthentication = enterpriseauth.ReauthenticatePromptAndMaxAge
	f.installHandler(f.store)
	g := httpTargetFixture{f: f, owner: f.browser("member-one"), target: f.browser("member-two"), operator: f.browser("operator")}
	f.login(g.owner)
	f.login(g.target)
	f.login(g.operator)
	g.o, g.m, g.a = f.session(g.owner), f.session(g.target), f.session(g.operator)
	f.createRoom(g.owner, g.o.CSRF, "target-qualified-room")
	status, body := f.post(g.owner, "/enterprise/api/v1/rooms/target-qualified-room/memberships", map[string]any{"client_id": "admit-existing-target", "expected_revision": 1, "intent": map[string]string{"action": "admit", "target_user_id": g.m.UserID, "role": "member"}}, g.o.CSRF)
	if status != 200 {
		t.Fatalf("typed target fixture admission: %d %s", status, body)
	}
	g.intent = sqlstore.GovernanceIntent{RoomID: "target-qualified-room", TargetUserID: g.m.UserID, ExpectedOwnerUserID: g.o.UserID, ExpectedRevision: 2, Reason: "Recover disabled Owner to exact qualified Member"}
	return g
}

func httpTargetState(t *testing.T, g httpTargetFixture) [32]byte {
	t.Helper()
	var state string
	if err := g.f.admin.QueryRow(t.Context(), `SELECT jsonb_build_object(
 'room',(SELECT to_jsonb(r) FROM rooms r WHERE room_id=$1),
 'members',(SELECT jsonb_agg(to_jsonb(m) ORDER BY user_id) FROM room_members m WHERE room_id=$1),
 'actions',(SELECT jsonb_agg(to_jsonb(a) ORDER BY action_id) FROM governance_actions a),
 'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY op_kind,identifier) FROM governance_operations o),
 'facts',(SELECT jsonb_agg(to_jsonb(e) ORDER BY seq) FROM room_events e WHERE room_id=$1),
 'audit',(SELECT jsonb_agg(to_jsonb(a) ORDER BY audit_seq) FROM audit a)
)::text`, g.intent.RoomID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256([]byte(state))
}

func httpTargetUserStatus(t *testing.T, g httpTargetFixture, key, user string, enabled bool) {
	t.Helper()
	status, body := g.f.post(g.operator, "/enterprise/api/v1/administration/commands", map[string]any{"client_id": key, "intent": map[string]any{"kind": "user.status", "target_user_id": user, "enabled": enabled}}, g.a.CSRF)
	if status != 200 {
		t.Fatalf("typed HTTP user status: %d %s", status, body)
	}
}

func httpTargetMember(t *testing.T, g *httpTargetFixture, key, action, role string) {
	t.Helper()
	var revision uint64
	if err := g.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id=$1`, g.intent.RoomID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	intent := map[string]string{"target_user_id": g.m.UserID, "action": action}
	if role != "" {
		intent["role"] = role
	}
	status, body := g.f.post(g.owner, "/enterprise/api/v1/rooms/"+g.intent.RoomID+"/memberships", map[string]any{"client_id": key, "expected_revision": revision, "intent": intent}, g.o.CSRF)
	if status != 200 {
		t.Fatalf("typed HTTP membership: %d %s", status, body)
	}
	if err := g.f.admin.QueryRow(t.Context(), `SELECT revision FROM rooms WHERE room_id=$1`, g.intent.RoomID).Scan(&g.intent.ExpectedRevision); err != nil {
		t.Fatal(err)
	}
}

func httpTargetReady(t *testing.T, g httpTargetFixture, key string) sqlstore.GovernanceAction {
	t.Helper()
	original := g.f.cookie(g.operator).Value
	status, body := g.f.post(g.operator, "/enterprise/api/v1/governance/actions", map[string]any{"client_id": key, "intent": g.intent}, g.a.CSRF)
	var prepared struct {
		Action sqlstore.GovernanceAction `json:"action"`
		URL    string                    `json:"authorization_url"`
	}
	if status != 200 || json.Unmarshal(body, &prepared) != nil || prepared.Action.ID == "" || prepared.URL == "" {
		t.Fatalf("prepare real step-up: %d %s", status, body)
	}
	response, err := g.operator.Get(prepared.URL)
	if err != nil {
		t.Fatal("configured OIDC step-up failed")
	}
	_ = response.Body.Close()
	if response.StatusCode != 200 || g.f.cookie(g.operator).Value != original {
		t.Fatal("signed fresh step-up replaced original application session or failed")
	}
	status, body = g.f.get(g.operator, "/enterprise/api/v1/governance/actions/"+prepared.Action.ID)
	var ready struct {
		Action sqlstore.GovernanceAction `json:"action"`
	}
	if status != 200 || json.Unmarshal(body, &ready) != nil || !ready.Action.Ready || ready.Action.Intent != g.intent {
		t.Fatal("actual signed fresh proof did not freeze original qualified target")
	}
	return ready.Action
}

func httpTargetNoAdminContent(t *testing.T, g httpTargetFixture) {
	t.Helper()
	status, body := g.f.get(g.operator, "/enterprise/api/v1/rooms/"+g.intent.RoomID)
	if status != 403 || strings.Contains(string(body), "Private Room") {
		t.Fatal("governance disclosed administrator Room contents")
	}
	var memberships int
	if err := g.f.admin.QueryRow(t.Context(), `SELECT count(*) FROM room_members WHERE room_id=$1 AND user_id=$2`, g.intent.RoomID, g.a.UserID).Scan(&memberships); err != nil || memberships != 0 {
		t.Fatal("governance silently admitted administrator", err)
	}
}

func TestGovernanceTargetHTTPPGIneligiblePrepareNeverStartsFreshOIDCOrEffect(t *testing.T) {
	for _, kind := range []string{"viewer", "disabled-user", "removed-membership", "no-membership", "missing-user", "no-qualified-member"} {
		t.Run(kind, func(t *testing.T) {
			g := newHTTPTargetFixture(t)
			switch kind {
			case "viewer":
				httpTargetMember(t, &g, "http-make-viewer", "change_role", "viewer")
			case "disabled-user":
				httpTargetUserStatus(t, g, "http-disable-target", g.m.UserID, false)
			case "removed-membership":
				httpTargetMember(t, &g, "http-remove-target", "remove", "")
			case "no-membership":
				g.intent.TargetUserID = g.a.UserID
			case "missing-user":
				g.intent.TargetUserID = "user-does-not-exist"
			case "no-qualified-member":
				httpTargetMember(t, &g, "http-remove-only-member", "remove", "")
				g.intent.TargetUserID = g.a.UserID
			}
			httpTargetUserStatus(t, g, "http-disable-owner", g.o.UserID, false)
			before := httpTargetState(t, g)
			exchanges := g.f.idp.tokenCalls.Load()
			status, body := g.f.post(g.operator, "/enterprise/api/v1/governance/actions", map[string]any{"client_id": "reject-http-target", "intent": g.intent}, g.a.CSRF)
			if status != 403 || strings.Contains(string(body), "authorization_url") || g.f.idp.tokenCalls.Load() != exchanges || httpTargetState(t, g) != before {
				t.Fatalf("ineligible target started proof/effect: %d", status)
			}
			httpTargetNoAdminContent(t, g)
		})
	}
}

func TestGovernanceTargetHTTPPGFreshSignedProofRejectsDisabledTargetAtConsume(t *testing.T) {
	g := newHTTPTargetFixture(t)
	httpTargetUserStatus(t, g, "disable-owner-before-reauth", g.o.UserID, false)
	action := httpTargetReady(t, g, "fresh-proof-target")
	httpTargetUserStatus(t, g, "disable-target-after-fresh-proof", g.m.UserID, false)
	before := httpTargetState(t, g)
	command := map[string]any{"client_id": "consume-now-disabled-target", "intent": g.intent, "action_id": action.ID}
	status, body := g.f.post(g.operator, "/enterprise/api/v1/rooms/"+g.intent.RoomID+"/ownership", command, g.a.CSRF)
	if status != 403 || !strings.Contains(string(body), `"authorization"`) || httpTargetState(t, g) != before {
		t.Fatalf("fresh proof bypassed live target gate: %d %s", status, body)
	}
	status, body = g.f.post(g.operator, "/enterprise/api/v1/rooms/"+g.intent.RoomID+"/resolve-ownership", command, g.a.CSRF)
	if status != http.StatusConflict || !strings.Contains(string(body), `"not_committed"`) || httpTargetState(t, g) != before {
		t.Fatal("refused consume acquired accepted result")
	}
	httpTargetNoAdminContent(t, g)
}

func TestGovernanceTargetHTTPPGFreshSignedProofRejectsDemotedOrRemovedTarget(t *testing.T) {
	for _, kind := range []string{"demoted", "removed"} {
		t.Run(kind, func(t *testing.T) {
			g := newHTTPTargetFixture(t)
			httpTargetUserStatus(t, g, "disable-owner-before-live-proof", g.o.UserID, false)
			action := httpTargetReady(t, g, "fresh-proof-before-membership-change")
			command := map[string]any{"client_id": "consume-original-membership-proof", "intent": g.intent, "action_id": action.ID}
			// An enabled Owner and a fresh application login are necessary for
			// the typed membership change. This legal roundtrip also advances
			// revision; live eligibility is rejected before that secondary CAS.
			httpTargetUserStatus(t, g, "enable-owner-for-membership-change", g.o.UserID, true)
			g.f.login(g.owner)
			g.o = g.f.session(g.owner)
			if kind == "demoted" {
				httpTargetMember(t, &g, "demote-after-signed-proof", "change_role", "viewer")
			} else {
				httpTargetMember(t, &g, "remove-after-signed-proof", "remove", "")
			}
			httpTargetUserStatus(t, g, "disable-owner-after-membership-change", g.o.UserID, false)
			before := httpTargetState(t, g)
			status, body := g.f.post(g.operator, "/enterprise/api/v1/rooms/"+g.intent.RoomID+"/ownership", command, g.a.CSRF)
			if status != 403 || !strings.Contains(string(body), `"authorization"`) || httpTargetState(t, g) != before {
				t.Fatalf("original signed proof ignored changed target: %d %s", status, body)
			}
			httpTargetNoAdminContent(t, g)
		})
	}
}

func TestGovernanceTargetHTTPPGQualifiedControlAndOriginalResolveAfterTargetDisable(t *testing.T) {
	g := newHTTPTargetFixture(t)
	httpTargetUserStatus(t, g, "disable-owner-for-control", g.o.UserID, false)
	action := httpTargetReady(t, g, "qualified-http-proof")
	command := map[string]any{"client_id": "qualified-http-transfer", "intent": g.intent, "action_id": action.ID}
	status, original := g.f.post(g.operator, "/enterprise/api/v1/rooms/"+g.intent.RoomID+"/ownership", command, g.a.CSRF)
	if status != 200 || !strings.Contains(string(original), `"committed"`) {
		t.Fatalf("qualified live target control failed: %d %s", status, original)
	}
	httpTargetNoAdminContent(t, g)
	httpTargetUserStatus(t, g, "disable-accepted-target", g.m.UserID, false)
	before := httpTargetState(t, g)
	status, replayed := g.f.post(g.operator, "/enterprise/api/v1/rooms/"+g.intent.RoomID+"/resolve-ownership", command, g.a.CSRF)
	var accepted, recovered struct {
		Outcome string                     `json:"outcome"`
		Receipt sqlstore.GovernanceReceipt `json:"receipt"`
	}
	if status != 200 || json.Unmarshal(original, &accepted) != nil || json.Unmarshal(replayed, &recovered) != nil || recovered.Outcome != "replayed" || recovered.Receipt != accepted.Receipt || httpTargetState(t, g) != before {
		t.Fatal("current target state blocked original accepted HTTP recovery")
	}
	httpTargetNoAdminContent(t, g)
}
