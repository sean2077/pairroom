package enterpriseservice

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestGovernanceHTTPReasonValidationPrecedesBackendOrProvider(t *testing.T) {
	h, _, backend := newGovernanceFixture(t)
	cookie := loginSession(t, h)
	csrf := sessionCSRF(t, h, cookie)
	for _, reason := range []string{"", " \t\u00a0\u3000", strings.Repeat("界", 342), "private\x00", "private\x1b"} {
		intent := governanceIntent()
		intent.Reason = reason
		for _, route := range []string{"/enterprise/api/v1/governance/actions", "/enterprise/api/v1/rooms/room-1/ownership", "/enterprise/api/v1/rooms/room-1/resolve-ownership"} {
			body := map[string]any{"client_id": "op-one", "intent": intent}
			if !strings.HasSuffix(route, "/actions") {
				body["action_id"] = "action-one"
			}
			data, _ := json.Marshal(body)
			if w := request(t, h, http.MethodPost, route, string(data), cookie, csrf); w.Code != 400 {
				t.Fatalf("invalid reason reached action %s: %d", route, w.Code)
			}
		}
	}
	if backend.prepareCalls != 0 || backend.transferCalls != 0 || backend.resolveCalls != 0 {
		t.Fatal("invalid reason entered backend")
	}
}
