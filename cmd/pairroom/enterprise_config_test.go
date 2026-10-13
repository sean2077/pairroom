package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func testEnterpriseConfig() enterpriseFileConfig {
	return enterpriseFileConfig{Version: 1, DeploymentID: "deployment-test", OrganizationID: "org-test", DatabaseSchema: "enterprise_test", DatabaseDSNEnv: "PAIRROOM_TEST_DSN", Listen: "127.0.0.1:8443", PublicOrigin: "https://pairroom.example.test", TLSCertificateFile: "server.crt", TLSKeyFile: "server.key", OIDC: enterpriseOIDCConfig{ConnectionID: "test-idp", Issuer: "https://idp.example.test/realm", ClientID: "pairroom", ClientSecretEnv: "PAIRROOM_TEST_CLIENT_SECRET", ClientAuthMethod: "basic", AllowedOrigins: []string{"https://idp.example.test"}, AdmittedSubjects: []string{"Alice", "alice"}}}
}

func TestEnterpriseConfigDefaultsAndSecretReferences(t *testing.T) {
	cfg := testEnterpriseConfig()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "enterprise.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadEnterpriseConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SessionTTL != "12h" || loaded.PendingLoginLimit != 1024 || loaded.TLSKeyFile != filepath.Join(filepath.Dir(path), "server.key") || loaded.OIDC.AdmittedSubjects[0] != "Alice" || loaded.OIDC.AdmittedSubjects[1] != "alice" {
		t.Fatalf("operator values or defaults changed: %+v", loaded)
	}
	provider := loaded.providerConfig("synthetic-secret")
	if provider.ClientSecret != "synthetic-secret" || provider.RedirectURL != "https://pairroom.example.test/enterprise/auth/callback" || provider.AuthStyle != oauth2.AuthStyleInHeader {
		t.Fatal("configured callback or explicit token style was not preserved")
	}
	encoded, err := json.Marshal(loaded)
	if err != nil || strings.Contains(string(encoded), "synthetic-secret") {
		t.Fatal("resolved provider secret entered operator configuration serialization")
	}
}

func TestEnterpriseConfigRejectsBeforeOpeningResources(t *testing.T) {
	cases := map[string]func(*enterpriseFileConfig){
		"future version":      func(c *enterpriseFileConfig) { c.Version++ },
		"invalid schema":      func(c *enterpriseFileConfig) { c.DatabaseSchema = "public; DROP TABLE users" },
		"hostname listener":   func(c *enterpriseFileConfig) { c.Listen = "localhost:8443" },
		"ephemeral port":      func(c *enterpriseFileConfig) { c.Listen = "127.0.0.1:0" },
		"plaintext origin":    func(c *enterpriseFileConfig) { c.PublicOrigin = "http://pairroom.example.test" },
		"origin path":         func(c *enterpriseFileConfig) { c.PublicOrigin += "/enterprise" },
		"origin credentials":  func(c *enterpriseFileConfig) { c.PublicOrigin = "https://user:secret@pairroom.example.test" },
		"origin case alias":   func(c *enterpriseFileConfig) { c.PublicOrigin = "https://PairRoom.example.test" },
		"no TLS policy":       func(c *enterpriseFileConfig) { c.TLSCertificateFile, c.TLSKeyFile = "", "" },
		"partial TLS pair":    func(c *enterpriseFileConfig) { c.TLSKeyFile = "" },
		"trust every proxy":   func(c *enterpriseFileConfig) { c.TrustedProxyPrefixes = []string{"0.0.0.0/0"} },
		"unmasked proxy":      func(c *enterpriseFileConfig) { c.TrustedProxyPrefixes = []string{"10.0.0.4/24"} },
		"long session":        func(c *enterpriseFileConfig) { c.SessionTTL = "48h" },
		"unbounded logins":    func(c *enterpriseFileConfig) { c.PendingLoginLimit = 100001 },
		"unapproved issuer":   func(c *enterpriseFileConfig) { c.OIDC.Issuer = "https://other.example.test" },
		"issuer query":        func(c *enterpriseFileConfig) { c.OIDC.Issuer += "?endpoint=other" },
		"implicit auth probe": func(c *enterpriseFileConfig) { c.OIDC.ClientAuthMethod = "" },
		"inline secret":       func(c *enterpriseFileConfig) { c.DatabaseDSNEnv = "postgres://user:secret@server/db" },
		"duplicate subject":   func(c *enterpriseFileConfig) { c.OIDC.AdmittedSubjects = []string{"same", "same"} },
		"subject control":     func(c *enterpriseFileConfig) { c.OIDC.AdmittedSubjects = []string{"subject\x00"} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := testEnterpriseConfig()
			change(&cfg)
			if err := cfg.validate(); err == nil {
				t.Fatal("unsafe or unsupported operator configuration accepted")
			}
		})
	}
	cfg := testEnterpriseConfig()
	cfg.TLSCertificateFile, cfg.TLSKeyFile = "", ""
	cfg.TrustedProxyPrefixes = []string{"127.0.0.1/32", "::1/128"}
	if err := cfg.validate(); err != nil {
		t.Fatalf("explicit TLS proxy configuration rejected: %v", err)
	}
}

func TestEnterpriseConfigRejectsUnsupportedJSON(t *testing.T) {
	valid, err := json.Marshal(testEnterpriseConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{string(valid) + "{}", `{"version":1,"database_dsn":"secret"}`, strings.Repeat(" ", 64<<10) + string(valid)} {
		path := filepath.Join(t.TempDir(), "enterprise.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadEnterpriseConfig(path); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatal("invalid configuration accepted or inline secret echoed")
		}
	}
}

func TestEnterpriseEnvironmentSecretDoesNotLeak(t *testing.T) {
	t.Setenv("PAIRROOM_TEST_SECRET", "private-test-value")
	value, err := enterpriseSecretFromEnv("PAIRROOM_TEST_SECRET", true)
	if err != nil || value != "private-test-value" {
		t.Fatal("explicit secret reference was not resolved")
	}
	t.Setenv("PAIRROOM_TEST_SECRET", "   ")
	if _, err := enterpriseSecretFromEnv("PAIRROOM_TEST_SECRET", true); err == nil || err.Error() != "enterprise secret environment reference PAIRROOM_TEST_SECRET is unavailable" {
		t.Fatal("invalid secret accepted or exposed")
	}
	if value, err := enterpriseSecretFromEnv("", false); err != nil || value != "" {
		t.Fatal("public OIDC client unexpectedly requires a secret")
	}
}

func TestEnterpriseAdmissionLimitPrecedesDatabase(t *testing.T) {
	cfg := testEnterpriseConfig()
	cfg.OIDC.AdmittedSubjects = make([]string, 1000)
	for i := range cfg.OIDC.AdmittedSubjects {
		cfg.OIDC.AdmittedSubjects[i] = fmt.Sprintf("subject-%d", i)
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("supported admission boundary refused: %v", err)
	}
	cfg.OIDC.AdmittedSubjects = append(cfg.OIDC.AdmittedSubjects, "one-too-many")
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "enterprise.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// A missing DSN provides a second observable boundary: static admission
	// validation must reject before environment lookup or database effects.
	t.Setenv(cfg.DatabaseDSNEnv, "")
	err = runEnterprise([]string{"init", "--config", path, "--bootstrap-subject", "administrator"})
	if err == nil || err.Error() != "enterprise OIDC origin or admission configuration exceeds its budget" {
		t.Fatalf("admission overflow reached resource setup: %v", err)
	}
}

func TestEnterpriseConnectionBudgetLoadsBeforeOpeningResources(t *testing.T) {
	for _, limit := range []int{-1, 1, 128, 256, 257} {
		cfg := testEnterpriseConfig()
		cfg.Resources = &enterpriseResourceConfig{Connections: limit}
		data, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "enterprise.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadEnterpriseConfig(path)
		if limit < 1 || limit > 256 {
			if err == nil {
				t.Fatal("invalid connection budget reached resource setup")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		limits, err := loaded.httpResourceLimits().Normalized()
		if err != nil || limits.Connections != limit || limits.ConcurrentRequests != 32 {
			t.Fatal("strict JSON connection budget was lost before listener admission")
		}
	}
}
