package sqlstore

import (
	"context"
	"testing"
)

func TestUserDisableIgnoresPermanentBindingAndGrantHistory(t *testing.T) {
	f := identityPGFixture(t, nil)
	ctx := context.Background()
	_, admin := identityPGLogin(t, f, "operator-selected", "history-admin")
	_, owner := identityPGLogin(t, f, "member-one", "history-owner")
	c, device := devicePGEnroll(t, f, owner, "history-device")
	_, token := devicePGSeedBinding(t, f, owner, c, device, "history-room", true)
	// Historical generations and renewals have no remaining authority. Keep
	// one real live Binding and current family, including an archived Room.
	for _, sql := range []string{
		`INSERT INTO slot_generations(org_id,room_id,slot,generation) SELECT 'org-test','history-room','slot2',i FROM generate_series(2,10002) i`,
		`INSERT INTO bindings(binding_id,org_id,room_id,slot,generation,agent_id,created_at,retired_at,retired_reason) SELECT 'history-binding-'||i,'org-test','history-room','slot2',i,(SELECT agent_id FROM bindings WHERE binding_id='binding-history-room'),now()-interval '1 day',now(),'replaced' FROM generate_series(2,10002) i`,
		`INSERT INTO device_grants(grant_id,family_id,access_hash,refresh_hash,created_at,expires_at,revoked_at) SELECT 'history-grant-'||i,'request-history-device',decode(md5('access'||i)||md5('access'||i),'hex'),decode(md5('refresh'||i)||md5('refresh'||i),'hex'),now()-interval '1 day',now()-interval '1 hour',now() FROM generate_series(1,10001) i`,
		`INSERT INTO browser_sessions(session_id,user_id,token_hash,csrf_hash,restore_epoch,created_at,expires_at,revoked_at) SELECT 'history-session-'||i,(SELECT user_id FROM devices WHERE device_id='device-history-device'),decode(md5('session-token'||i)||md5('session-token'||i),'hex'),decode(md5('session-csrf'||i)||md5('session-csrf'||i),'hex'),(SELECT restore_epoch FROM identity_policy),now()-interval '1 day',now()+interval '1 hour',now() FROM generate_series(1,10001) i`,
		`UPDATE rooms SET archived_at=now() WHERE room_id='history-room'`,
	} {
		if _, e := f.admin.Exec(ctx, sql); e != nil {
			t.Fatal(e)
		}
	}
	r, e := f.store.SetUserEnabled(ctx, admin.Auth, "disable-history-owner", owner.Auth.UserID, false)
	if e != nil || r.Outcome != OutcomeCommitted {
		t.Fatalf("history blocked disable: %+v %v", r, e)
	}
	if _, e = f.store.AuthenticateBinding(ctx, token, c.Origin); e == nil {
		t.Fatal("archived live Binding remained authorized")
	}
	var history, active, grants int
	if e = f.admin.QueryRow(ctx, `SELECT (SELECT count(*) FROM bindings WHERE retired_at IS NOT NULL),(SELECT count(*) FROM bindings WHERE retired_at IS NULL),(SELECT count(*) FROM device_grants WHERE revoked_at IS NULL)`).Scan(&history, &active, &grants); e != nil || history != 10002 || active != 0 || grants != 0 {
		t.Fatalf("history lost or authority remained: %d %d %d %v", history, active, grants, e)
	}
}
