package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func writeEnterpriseConfig(t *testing.T, cfg enterpriseFileConfig) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "enterprise.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The remaining operator bounds and references must fail closed before any
// resource, environment secret or database is touched.
func TestEnterpriseConfigRejectsRemainingOperatorBounds(t *testing.T) {
	cases := map[string]func(*enterpriseFileConfig){
		"attachment directory with a control character": func(c *enterpriseFileConfig) {
			c.Attachments = &enterpriseAttachmentConfig{Directory: "objects\x01"}
		},
		"attachment limits inconsistent": func(c *enterpriseFileConfig) {
			c.Attachments = &enterpriseAttachmentConfig{Directory: "objects", DeploymentBytes: 1024, RoomBytes: 2048, DeploymentPending: 4, RoomPending: 2}
		},
		"oidc connection identity": func(c *enterpriseFileConfig) { c.OIDC.ConnectionID = "bad connection" },
		"oidc client identity":     func(c *enterpriseFileConfig) { c.OIDC.ClientID = "" },
		"oidc secret reference":    func(c *enterpriseFileConfig) { c.OIDC.ClientSecretEnv = "bad reference" },
		"allowed origin with a path": func(c *enterpriseFileConfig) {
			c.OIDC.AllowedOrigins = []string{"https://idp.example.test/realm"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testEnterpriseConfig()
			mutate(&cfg)
			if _, err := loadEnterpriseConfig(writeEnterpriseConfig(t, cfg)); err == nil {
				t.Fatal("invalid operator configuration was accepted")
			}
		})
	}
}

func TestEnterpriseConfigFileBoundsAndRelativeReferences(t *testing.T) {
	cfg := testEnterpriseConfig()
	cfg.Attachments = &enterpriseAttachmentConfig{Directory: "objects"}
	loaded, err := loadEnterpriseConfig(writeEnterpriseConfig(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Attachments == nil || loaded.Attachments.Directory == "" || !filepath.IsAbs(loaded.Attachments.Directory) {
		t.Fatalf("relative attachment directory was not resolved against the configuration: %+v", loaded.Attachments)
	}
	if loaded.Attachments.DeploymentBytes != 0 {
		t.Fatalf("all-zero attachment limits must stay zero: %+v", loaded.Attachments)
	}
	if _, err := loadEnterpriseConfig(""); err == nil {
		t.Fatal("empty --config was accepted")
	}
	if _, err := loadEnterpriseConfig(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("absent configuration was accepted")
	}
	oversized := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(oversized, []byte(`{"version":1,"padding":"`+strings.Repeat("x", 64<<10)+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnterpriseConfig(oversized); err == nil {
		t.Fatal("oversized configuration was accepted")
	}
	trailing := filepath.Join(t.TempDir(), "trailing.json")
	if err := os.WriteFile(trailing, []byte(`{"version":1} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadEnterpriseConfig(trailing); err == nil {
		t.Fatal("multiple configuration objects were accepted")
	}
}

func TestEnterpriseSecretReferenceProxyPeersAndProviderRoots(t *testing.T) {
	if _, err := enterpriseSecretFromEnv("bad reference", true); err == nil {
		t.Fatal("invalid secret environment reference was accepted")
	}
	if _, err := enterpriseSecretFromEnv("PAIRROOM_ENTERPRISE_ABSENT_SECRET", true); err == nil {
		t.Fatal("unavailable required secret was accepted")
	}
	if value, err := enterpriseSecretFromEnv("", false); err != nil || value != "" {
		t.Fatal("an optional empty reference must resolve to no secret")
	}
	cfg := testEnterpriseConfig()
	cfg.TrustedProxyPrefixes = make([]string, 17)
	if _, err := cfg.proxyPrefixes(); err == nil {
		t.Fatal("too many trusted proxy peers were accepted")
	}
	cfg = testEnterpriseConfig()
	cfg.OIDC.ClientAuthMethod = "post"
	if cfg.providerConfig("synthetic").AuthStyle != oauth2.AuthStyleInParams {
		t.Fatal("explicit post client authentication was not preserved")
	}
	cfg.OIDC.CACertificateFile = filepath.Join(t.TempDir(), "absent.pem")
	if _, err := cfg.providerRoots(); err == nil {
		t.Fatal("absent OIDC CA certificate file was accepted")
	}
}

// Operator maintenance and the enterprise dispatch reject invalid usage and
// unavailable secrets before opening a database or listening.
func TestEnterpriseMaintenanceAndDispatchRejectBeforeResources(t *testing.T) {
	root := t.TempDir()
	objects := filepath.Join(root, "objects")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(root, "existing")
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	notADirectory := filepath.Join(root, "file")
	if err := os.WriteFile(notADirectory, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	withAttachments := testEnterpriseConfig()
	withAttachments.Attachments = &enterpriseAttachmentConfig{Directory: objects}
	config := writeEnterpriseConfig(t, withAttachments)
	withoutAttachments := writeEnterpriseConfig(t, testEnterpriseConfig())
	absentConfig := filepath.Join(root, "absent.json")
	// A negative case must not depend on the ambient environment: clearing the referenced variables
	// keeps every database or client secret refusal deterministic even when a developer or CI
	// happens to export the test reference names.
	t.Setenv(withAttachments.DatabaseDSNEnv, "")
	t.Setenv(withAttachments.OIDC.ClientSecretEnv, "")
	// Each case names the refusal it must produce, so a deleted validation cannot pass as the
	// unavailable database secret that unconfigured environments report instead.
	cases := map[string]struct {
		args []string
		want string
	}{
		"unsupported action":            {[]string{"unknown-action", "--config", config, "--id", "operation-1"}, "unsupported enterprise command"},
		"missing configuration":         {[]string{"backup", "--id", "operation-1"}, "enterprise --config is required"},
		"absent configuration":          {[]string{"backup", "--config", absentConfig, "--id", "operation-1"}, "cannot open enterprise configuration file"},
		"missing identifier":            {[]string{"backup", "--config", config}, "stable valid --id"},
		"invalid identifier":            {[]string{"backup", "--config", config, "--id", "bad id"}, "stable valid --id"},
		"positional arguments":          {[]string{"backup", "--config", config, "--id", "operation-1", "extra"}, "stable valid --id"},
		"attachments unconfigured":      {[]string{"backup", "--config", withoutAttachments, "--id", "operation-1", "--output", filepath.Join(root, "bundle")}, "private attachment directory"},
		"output inside the object root": {[]string{"backup", "--config", config, "--id", "operation-1", "--output", filepath.Join(objects, "bundle")}, "outside the managed object root"},
		"existing output directory":     {[]string{"backup", "--config", config, "--id", "operation-1", "--output", existing}, "requires a new output directory"},
		"output parent is a file":       {[]string{"backup", "--config", config, "--id", "operation-1", "--output", filepath.Join(notADirectory, "bundle")}, "existing direct directory"},
		"source inside the object root": {[]string{"restore", "--config", config, "--id", "operation-1", "--source", filepath.Join(objects, "bundle")}, "outside the new managed object root"},
		"non-canonical manifest digest": {[]string{"resolve-restore", "--config", config, "--id", "operation-1", "--manifest-hash", "not-a-digest"}, "canonical SHA256"},
		"unavailable database secret":   {[]string{"complete-restore", "--config", config, "--id", "operation-1", "--manifest-hash", strings.Repeat("0", 64)}, withAttachments.DatabaseDSNEnv + " is unavailable"},
		"backup-end without a database": {[]string{"backup-end", "--config", config, "--id", "operation-1"}, withAttachments.DatabaseDSNEnv + " is unavailable"},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			err := runEnterprise(testCase.args)
			if err == nil {
				t.Fatal("invalid operator invocation was accepted")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("refusal did not name its own validation: %q", err.Error())
			}
		})
	}
	capture := startStdoutCapture(t)
	if err := runEnterpriseMaintenance([]string{"backup", "--help"}); err != nil {
		t.Fatal(err)
	}
	capture.Stop()
	for _, args := range [][]string{{}, {"help"}, {"--help"}} {
		capture = startStdoutCapture(t)
		if err := runEnterprise(args); err != nil {
			t.Fatal(err)
		}
		if output := capture.Stop(); !strings.Contains(output, "Usage: pairroom enterprise init") {
			t.Fatal("enterprise help did not print usage")
		}
	}
	for _, dispatch := range []struct {
		args []string
		want string
	}{
		{[]string{"unknown"}, "unsupported enterprise command"},
		{[]string{"init", "extra"}, "unexpected enterprise command arguments"},
		{[]string{"init", "--bootstrap-subject", "bad\x01subject"}, "exact valid --bootstrap-subject"},
		{[]string{"init", "--bootstrap-subject", "alice"}, "enterprise --config is required"},
		{[]string{"serve", "--config", ""}, "enterprise --config is required"},
	} {
		err := runEnterprise(dispatch.args)
		if err == nil {
			t.Fatalf("invalid enterprise dispatch was accepted: %v", dispatch.args)
		}
		if !strings.Contains(err.Error(), dispatch.want) {
			t.Fatalf("dispatch refusal did not name its own validation: %q", err.Error())
		}
	}
	capture = startStdoutCapture(t)
	if err := runEnterprise([]string{"serve", "--help"}); err != nil {
		t.Fatal(err)
	}
	capture.Stop()
	if !maintenanceID("operation-1") || maintenanceID("bad id") {
		t.Fatal("operator identifier pattern changed")
	}
	canonical := strings.Repeat("ab", 32)
	if digest, err := maintenanceDigest(canonical); err != nil {
		t.Fatal("canonical manifest digest was rejected")
	} else {
		for _, value := range digest {
			if value != 0xab {
				t.Fatalf("canonical manifest digest decoded to unexpected bytes: %x", digest)
			}
		}
	}
	if _, err := maintenanceDigest(strings.ToUpper(canonical)); err == nil {
		t.Fatal("non-canonical manifest digest was accepted")
	}
	if _, err := maintenanceDigest(strings.Repeat("ab", 31)); err == nil {
		t.Fatal("short manifest digest was accepted")
	}
}

// Endpoint commands reach the client call and fail on an unreachable origin
// without loading operator secrets, opening a database or starting a browser.
func TestEnterpriseClientCLIHeadlessRefreshAndHelpPaths(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	capture := startStdoutCapture(t)
	if err := runEnterpriseClient([]string{"status", "--help"}); err != nil {
		t.Fatal(err)
	}
	capture.Stop()
	capture = startStdoutCapture(t)
	e := runEnterpriseClient([]string{"login", "--service-origin", "https://127.0.0.1:1", "--deployment", "dep-test", "--state-dir", directory, "--headless"})
	output := capture.Stop()
	if e == nil {
		t.Fatal("unreachable enterprise origin reported a successful headless login")
	}
	if output != "" && !strings.Contains(output, "Open this exact HTTPS URL") {
		t.Fatalf("headless login printed unexpected output: %q", output)
	}
	for _, args := range [][]string{
		{"refresh", "--service-origin", "https://127.0.0.1:1", "--deployment", "dep-test", "--state-dir", directory},
		{"refresh", "--service-origin", "https://127.0.0.1:1", "--deployment", "dep-test", "--state-dir", directory, "--retry"},
	} {
		if err := runEnterpriseClient(args); err == nil {
			t.Fatalf("unreachable enterprise origin reported success: %v", args)
		}
	}
}
