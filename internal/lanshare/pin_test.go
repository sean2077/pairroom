package lanshare

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The pinned comparison is byte-exact against the lowercase fingerprint a host
// emits, and the guest's routing identity derives from the pin's spelling. An
// invitation carrying uppercase hex must therefore be canonicalized at parse
// time, not validate and then fail every handshake.
func TestParseInviteCanonicalizesThePinnedHostKey(t *testing.T) {
	canonical := strings.Repeat("ab", 32)
	invite := Invite{Version: Version, Endpoint: "https://192.168.1.2:8877", HostPin: strings.Repeat("AB", 32), RoomID: "room-one", InviteID: "invitation-one", ExpiresAt: time.Now().UTC().Add(time.Hour)}
	encoded, err := json.Marshal(invite)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseInvite("pairroom://join/" + base64.RawURLEncoding.EncodeToString(encoded))
	if err != nil {
		t.Fatalf("uppercase pin was rejected as malformed: %v", err)
	}
	if parsed.HostPin != canonical {
		t.Fatalf("pin was not canonicalized: %q", parsed.HostPin)
	}
	if !ValidFingerprint(parsed.HostPin) {
		t.Fatal("canonicalized pin did not validate")
	}
}
