package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnterpriseClientCLIRequiresPinnedScopeAndClosedActions(t *testing.T) {
	cases := [][]string{{}, {"unknown"}, {"status"}, {"refresh", "--service-origin", "https://example.test", "--deployment", "dep-test", "--recover", "--retry"}, {"login", "--service-origin", "http://example.test", "--deployment", "dep-test"}, {"status", "--service-origin", "https://example.test", "--deployment", "dep-test", "extra"}, {"status", "--token", "secret"}}
	for _, args := range cases {
		if e := runEnterpriseClient(args); e == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
}
func TestEnterpriseClientCLILocalStatusAndLogoutAreMetadataOnly(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	for _, action := range []string{"status", "logout"} {
		capture := startStdoutCapture(t)
		e := runEnterpriseClient([]string{action, "--service-origin", "https://example.test", "--deployment", "dep-test", "--state-dir", directory})
		output := capture.Stop()
		if e != nil {
			t.Fatal(e)
		}
		var decoded map[string]any
		if e = json.Unmarshal([]byte(output), &decoded); e != nil {
			t.Fatalf("status is not JSON: %v", e)
		}
		if decoded["scope"] != "local" {
			t.Fatalf("local action claimed server authority: %s", output)
		}
		for _, field := range []string{"\"access\"", "\"refresh\"", "\"proof\"", "client_state", "callback_uri"} {
			if strings.Contains(output, field) {
				t.Fatalf("private field in CLI stdout: %s", field)
			}
		}
		if action == "logout" && !strings.Contains(output, "Server registration and Bindings remain") {
			t.Fatal("logout implied server revocation")
		}
	}
}
