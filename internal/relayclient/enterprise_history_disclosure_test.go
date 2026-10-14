package relayclient

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseclient"
)

func assertEnterpriseHistoryNotice(t *testing.T, data []byte) {
	t.Helper()
	var result map[string]json.RawMessage
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	var notice string
	if json.Unmarshal(result["notice"], &notice) != nil {
		t.Fatal("notice missing")
	}
	for _, clause := range []string{"full retained history", "before", "referenced attachments", "same-Room quoted evidence"} {
		if !strings.Contains(notice, clause) {
			t.Fatalf("foreground notice omitted %q", clause)
		}
	}
	if len(notice) > 512 {
		t.Fatal("foreground notice grew beyond bounded display budget")
	}
	if result["history_scope"] != nil || bytes.Contains(data, []byte("private-local-relay-capability")) {
		t.Fatal("disclosure altered wire authority or exposed local credential")
	}
}

func TestEnterpriseForegroundPendingHistoryDisclosure(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "disclosure-second-session")
	var out bytes.Buffer
	args := []string{"bind", "--repo", f.root, "--enterprise-service", f.server.URL, "--deployment", "dep-test", "--enterprise-ca", f.ca, "--enterprise-state-dir", f.deviceDir, "--room", "server-room", "--slot", "2"}
	if err := f.run(args, nil, &out); err != nil {
		t.Fatal(err)
	}
	assertEnterpriseHistoryNotice(t, out.Bytes())
}

func TestEnterpriseForegroundReadyHistoryDisclosureKeepsBootstrap(t *testing.T) {
	f := newEnterpriseCLIFixture(t)
	var out bytes.Buffer
	view := enterpriseclient.RelayContext{Bootstrap: "unchanged bootstrap"}
	if err := writeEnterpriseReady(&out, f.state, view); err != nil {
		t.Fatal(err)
	}
	assertEnterpriseHistoryNotice(t, out.Bytes())
	var result struct {
		Bootstrap string `json:"bootstrap"`
	}
	if json.Unmarshal(out.Bytes(), &result) != nil || result.Bootstrap != view.Bootstrap {
		t.Fatal("disclosure rewrote native bootstrap")
	}
}
