package sqlstore

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestAdministrationCursorKeepsExactIdentityAndResourceScope(t *testing.T) {
	keys := []string{"connection-1", "https://idp.example.test/exact/", "Opaque Subject "}
	encoded := encodeAdministrationCursor("admissions", keys)
	decoded, err := decodeAdministrationCursor(AdministrationQuery{Resource: "admissions", Limit: 50, After: encoded})
	if err != nil || len(decoded) != 3 || decoded[2] != keys[2] || decoded[1] != keys[1] {
		t.Fatalf("opaque identity normalized: %#v %v", decoded, err)
	}
	for _, query := range []AdministrationQuery{{Resource: "users", Limit: 50, After: encoded}, {Resource: "admissions", Limit: 51}, {Resource: "rooms", Limit: 1}, {Resource: "users", Limit: 1, After: strings.Repeat("A", 8193)}, {Resource: "users", Limit: 1, After: base64.RawURLEncoding.EncodeToString([]byte(`{"version":1,"resource":"users","keys":["user-1"],"admin":true}`))}} {
		if _, err := decodeAdministrationCursor(query); err == nil {
			t.Fatalf("unbounded or ambiguous cursor accepted: %+v", query)
		}
	}
}

func TestAdministrationIntentCannotExpressRoomAuthorityOrFreshnessClaims(t *testing.T) {
	identity := AdministrationIdentity{ConnectionID: "connection-1", Issuer: "https://idp.example.test", Subject: "subject "}
	for _, intent := range []AdministrationIntent{{Kind: controlUserStatus, TargetUserID: "user-1", Enabled: false}, {Kind: controlAdminSet, TargetUserID: "user-1", Enabled: true}, {Kind: controlAdmission, Identity: identity, Enabled: true}} {
		if !validateAdministrationIntent(intent) {
			t.Fatalf("valid closed intent rejected: %+v", intent)
		}
	}
	for _, intent := range []AdministrationIntent{{Kind: "room.owner", TargetUserID: "user-1", Enabled: true}, {Kind: controlUserStatus, TargetUserID: "user-1", Identity: identity}, {Kind: controlAdmission, Identity: identity, TargetUserID: "user-1"}, {Kind: controlAdmission, Identity: AdministrationIdentity{ConnectionID: "connection-1", Issuer: "https://idp.example.test", Subject: "bad\x00subject"}}} {
		if validateAdministrationIntent(intent) {
			t.Fatalf("unrelated authority fields accepted: %+v", intent)
		}
	}
	s := &Store{cfg: Config{DeploymentID: "deployment-1"}}
	for _, auth := range []AuthContext{{Deployment: "deployment-1", UserID: "user-1"}, {Deployment: "deployment-1", UserID: "user-1", DeviceGrantID: "grant"}, {Deployment: "deployment-1", UserID: "user-1", SessionID: "session-1", BindingGrantID: "binding"}} {
		if s.administrationCommandAllowed(AdministrationCommand{Auth: auth, ClientID: "control-1", Intent: AdministrationIntent{Kind: controlAdminSet, TargetUserID: "user-1", Enabled: true}}) {
			t.Fatal("administrator mutation accepted nonbrowser selector")
		}
	}
}
