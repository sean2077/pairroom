package sqlstore

import (
	"encoding/json"
	"strings"
	"testing"
)

func reasonIntent(t *testing.T, reason string) GovernanceIntent {
	t.Helper()
	var intent GovernanceIntent
	data, _ := json.Marshal(map[string]any{"room_id": "room-one", "target_user_id": "target", "expected_owner_user_id": "owner", "expected_revision": 2, "reason": reason})
	if err := json.Unmarshal(data, &intent); err != nil {
		t.Fatal(err)
	}
	return intent
}

func TestGovernanceReasonRequiredBoundedAndImmutableHash(t *testing.T) {
	s := &Store{cfg: Config{DeploymentID: "dep"}}
	auth := AuthContext{Deployment: "dep", UserID: "admin", SessionID: "session"}
	for _, reason := range []string{"", " \t\r\n\u00a0\u0085\u3000", strings.Repeat("界", 342), "reason\x00", "reason\x1b"} {
		for _, mutation := range []governanceMutation{{kind: governancePrepare}, {kind: governanceTransfer, actionID: "action-one"}} {
			mutation.intent = reasonIntent(t, reason)
			if _, err := s.governancePlan(auth, "op-one", mutation); err == nil {
				t.Fatalf("accepted invalid reason %q for %s", reason, mutation.kind)
			}
		}
	}
	first, err := s.governancePlan(auth, "op-one", governanceMutation{kind: governancePrepare, intent: reasonIntent(t, " original\n说明\t ")})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.governancePlan(auth, "op-one", governanceMutation{kind: governancePrepare, intent: reasonIntent(t, "original\n说明\t")})
	if err != nil || first.hash == second.hash {
		t.Fatal("changed original reason reused its immutable operation hash")
	}
	if _, err := s.governancePlan(auth, "op-normal", governanceMutation{kind: governanceTransfer, intent: reasonIntent(t, "")}); err != nil {
		t.Fatal("ordinary Owner transfer acquired a new reason requirement")
	}
}

func TestGovernanceReasonUTF8ByteBoundary(t *testing.T) {
	for _, reason := range []string{strings.Repeat("a", 1024), strings.Repeat("界", 341) + "a", " original\n说明\t "} {
		if !ValidGovernanceReason(reason) {
			t.Fatal("valid exact boundary rejected")
		}
	}
	for _, reason := range []string{strings.Repeat("a", 1025), string([]byte{0xff}), "x\u0085"} {
		if ValidGovernanceReason(reason) {
			t.Fatal("invalid byte/control reason accepted")
		}
	}
}
