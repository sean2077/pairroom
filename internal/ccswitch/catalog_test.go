package ccswitch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
	_ "modernc.org/sqlite"
)

type fixtureProfile struct {
	id, appType, name, settings, meta string
	current, failover                 int
}

func writeFixture(t *testing.T, schema int, profiles ...fixtureProfile) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
		settings_config TEXT NOT NULL, meta TEXT NOT NULL DEFAULT '{}',
		sort_index INTEGER,
		is_current BOOLEAN NOT NULL DEFAULT 0,
		in_failover_queue BOOLEAN NOT NULL DEFAULT 0,
		PRIMARY KEY (id, app_type));`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(schema)); err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if _, err := db.Exec(`INSERT INTO providers (id, app_type, name, settings_config, meta, is_current, in_failover_queue) VALUES (?, ?, ?, ?, ?, ?, ?)`, p.id, p.appType, p.name, p.settings, p.meta, p.current, p.failover); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCatalogAndResolveSupportedProfilesWithoutSecretOutput(t *testing.T) {
	const claudeSecret = "claude-fixture-secret"
	const codexSecret = "codex-fixture-secret"
	const grokSecret = "grok-fixture-secret"
	path := writeFixture(t, 18,
		fixtureProfile{"c", "claude", "Claude direct", `{"env":{"ANTHROPIC_AUTH_TOKEN":"` + claudeSecret + `","ANTHROPIC_BASE_URL":"https://claude.invalid","ANTHROPIC_MODEL":"claude-test"}}`, `{"apiFormat":"anthropic"}`, 1, 0},
		fixtureProfile{"o", "codex", "Codex direct", `{"auth":{"OPENAI_API_KEY":"` + codexSecret + `"},"config":"model_provider = \"custom\"\nmodel = \"gpt-test\"\n[model_providers.custom]\nname = \"Custom\"\nwire_api = \"responses\"\nbase_url = \"https://codex.invalid/v1\""}`, `{"apiFormat":"responses"}`, 0, 0},
		fixtureProfile{"g", "grokbuild", "Grok direct", `{"config":"[models]\ndefault = \"direct\"\n[model.\"direct\"]\nmodel = \"grok-test\"\nbase_url = \"https://grok.invalid/v1\"\nname = \"Grok Test\"\napi_backend = \"responses\"\napi_key = \"` + grokSecret + `\"\ncontext_window = 500000"}`, `{"apiFormat":"responses"}`, 0, 0},
		fixtureProfile{"oauth", "codex", "Managed OAuth", `{"auth":{"auth_mode":"chatgpt","tokens":{"access_token":"oauth-secret"}},"config":"model = \"gpt-oauth\""}`, `{}`, 0, 0},
		fixtureProfile{"proxy", "claude", "Proxy conversion", `{"env":{"ANTHROPIC_AUTH_TOKEN":"proxy-secret"}}`, `{"apiFormat":"proxy-conversion"}`, 0, 0},
		fixtureProfile{"fail", "claude", "Failover", `{"env":{"ANTHROPIC_AUTH_TOKEN":"fail-secret"}}`, `{}`, 0, 1},
	)
	reader, err := NewReader(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := reader.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Schema != 18 || len(catalog.Profiles) != 6 {
		t.Fatalf("catalog = %#v", catalog)
	}
	supported := 0
	reasons := map[string]bool{}
	for _, profile := range catalog.Profiles {
		if profile.Supported {
			supported++
		} else {
			reasons[profile.ReasonCode] = true
		}
	}
	if supported != 3 || !reasons[ReasonManagedOAuth] || !reasons[ReasonProxyConversion] || !reasons[ReasonFailover] {
		t.Fatalf("profile classification = %#v", catalog.Profiles)
	}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{claudeSecret, codexSecret, grokSecret, "oauth-secret", "proxy-secret", "fail-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("catalog leaked a fixture credential")
		}
	}
	tests := []struct {
		ref                                     model.ProviderRef
		runtime                                 model.RuntimeKind
		secret, modelName, envKey, providerName string
	}{
		{model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "claude", ProfileID: "c"}, model.RuntimeClaude, claudeSecret, "claude-test", "ANTHROPIC_AUTH_TOKEN", "Claude direct"},
		{model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "o"}, model.RuntimeCodex, codexSecret, "gpt-test", "PAIRROOM_CC_SWITCH_CODEX_API_KEY", "Codex direct"},
		{model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "grokbuild", ProfileID: "g"}, model.RuntimeGrok, grokSecret, "direct", "PAIRROOM_CC_SWITCH_GROK_API_KEY", "Grok direct"},
	}
	for _, test := range tests {
		materialized, err := reader.Resolve(context.Background(), test.ref, test.runtime)
		if err != nil {
			t.Fatalf("resolve %s: %v", test.runtime, err)
		}
		if materialized.Env[test.envKey] != test.secret || materialized.DefaultModel != test.modelName {
			t.Fatal("materialization did not contain the expected isolated credential and model")
		}
		// The display name travels with the internal reference label so a UI can
		// show a human-readable Provider without parsing the reference form.
		if materialized.ProviderName != test.providerName {
			t.Fatalf("materialization provider name = %q, want %q", materialized.ProviderName, test.providerName)
		}
		if !strings.HasPrefix(materialized.ProviderLabel, "cc-switch:") {
			t.Fatalf("materialization lost its internal reference label: %q", materialized.ProviderLabel)
		}
		encoded, _ := json.Marshal(materialized)
		if strings.Contains(string(encoded), test.secret) {
			t.Fatal("materialization JSON leaked a fixture credential")
		}
		if strings.Contains(strings.Join(materialized.Args, " "), test.secret) {
			t.Fatal("argv leaked secret")
		}
		if test.runtime == model.RuntimeGrok {
			overlay, effective, err := RenderGrokOverlay(materialized.Grok, "custom/model")
			if err != nil || effective != "custom/model" {
				t.Fatalf("render Grok overlay: effective=%q err=%v", effective, err)
			}
			if strings.Contains(overlay, test.secret) || !strings.Contains(overlay, `env_key = "PAIRROOM_CC_SWITCH_GROK_API_KEY"`) || !strings.Contains(overlay, `[model."custom/model"]`) {
				t.Fatal("Grok overlay contained a fixture credential or lacked safe environment indirection")
			}
		}
	}
}

func TestSanitizeProviderNameBoundsLengthAndStripsControls(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \t ", ""},
		{"plain", "Anthropic Direct", "Anthropic Direct"},
		{"surrounding space", "  Anthropic Direct  ", "Anthropic Direct"},
		{"C0 controls", "An\rthr\ropic\x1b[31m\x00Direct", "Anthropic[31mDirect"},
		{"DEL and C1", "Anthropic\x7fDirect\u0085\u009f", "AnthropicDirect"},
		{"slash sequence is kept", "Anthropic // prod", "Anthropic // prod"},
		{"at the bound", strings.Repeat("a", maxProviderNameBytes), strings.Repeat("a", maxProviderNameBytes)},
		{"over the bound", strings.Repeat("a", maxProviderNameBytes+40), strings.Repeat("a", maxProviderNameBytes)},
	}
	for _, test := range tests {
		if got := sanitizeProviderName(test.value); got != test.want {
			t.Fatalf("%s: sanitizeProviderName(%q) = %q, want %q", test.name, test.value, got, test.want)
		}
	}
	// Undecodable bytes are normalized rather than passed through, so the result
	// is always valid UTF-8 for JSON, the browser, and the durable event log.
	if got := sanitizeProviderName("Anthropic" + string([]byte{0xff, 0xfe}) + "Direct"); !utf8.ValidString(got) {
		t.Fatalf("invalid UTF-8 survived sanitization: %q", got)
	}
	// A multibyte name must be cut on a rune boundary, never mid-rune, so the
	// result stays valid UTF-8 for the browser and the durable event log.
	multibyte := strings.Repeat("磁🧪", 100)
	got := sanitizeProviderName(multibyte)
	if len(got) > maxProviderNameBytes {
		t.Fatalf("multibyte name kept %d bytes, want <= %d", len(got), maxProviderNameBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("multibyte truncation produced invalid UTF-8: %q", got)
	}
	if !strings.HasPrefix(multibyte, got) {
		t.Fatal("multibyte truncation was not a prefix of the original")
	}
	if len(got) == 0 {
		t.Fatal("multibyte truncation discarded the whole name")
	}
}

func TestCatalogAndResolveBoundProfileDisplayNameAndRejectCredentialNames(t *testing.T) {
	const leakySecret = "leaky-fixture-secret"
	longName := "Provider\r\n" + strings.Repeat("磁🧪", 100)
	path := writeFixture(t, 18,
		fixtureProfile{"long", "claude", longName, `{"env":{"ANTHROPIC_AUTH_TOKEN":"long-fixture-secret","ANTHROPIC_MODEL":"claude-test"}}`, `{}`, 0, 0},
		fixtureProfile{"leaky", "claude", "Profile " + leakySecret, `{"env":{"ANTHROPIC_AUTH_TOKEN":"` + leakySecret + `","ANTHROPIC_MODEL":"claude-test"}}`, `{}`, 0, 0},
	)
	reader, err := NewReader(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := reader.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ProfileSummary{}
	for _, profile := range catalog.Profiles {
		byID[profile.ProviderRef.ProfileID] = profile
	}
	long := byID["long"]
	if len(long.Name) > maxProviderNameBytes || !utf8.ValidString(long.Name) {
		t.Fatalf("catalog name is unbounded or invalid UTF-8: %d bytes %q", len(long.Name), long.Name)
	}
	if strings.ContainsAny(long.Name, "\r\n\x00\x1b\x7f") {
		t.Fatalf("catalog name kept control characters: %q", long.Name)
	}
	// The catalog summary is credential-redacted; the leaky profile stays in the
	// catalog as disabled rather than disappearing.
	if strings.Contains(byID["leaky"].Name, leakySecret) {
		t.Fatal("catalog name leaked a fixture credential")
	}

	materialized, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "claude", ProfileID: "long"}, model.RuntimeClaude)
	if err != nil {
		t.Fatalf("resolve bounded name: %v", err)
	}
	if len(materialized.ProviderName) > maxProviderNameBytes || !utf8.ValidString(materialized.ProviderName) {
		t.Fatalf("materialized name is unbounded or invalid UTF-8: %d bytes %q", len(materialized.ProviderName), materialized.ProviderName)
	}
	if strings.ContainsAny(materialized.ProviderName, "\r\n\x00\x1b\x7f") {
		t.Fatalf("materialized name kept control characters: %q", materialized.ProviderName)
	}
	// A Profile that embeds its own credential in the display name fails closed
	// instead of projecting the credential into RuntimeInfo or the event log.
	_, err = reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "claude", ProfileID: "leaky"}, model.RuntimeClaude)
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeProfileUnsupported || typed.Params["reason"] != ReasonInvalidConfig {
		t.Fatalf("credential-bearing name error = %#v", err)
	}
	if strings.Contains(err.Error(), leakySecret) {
		t.Fatal("the fail-closed error leaked the credential it rejected")
	}
}

func TestCatalogDisablesProtocolConversionAndResolvesDeclaredEnvironmentCredential(t *testing.T) {
	t.Setenv("PAIRROOM_FIXTURE_CODEX_KEY", "env-secret")
	path := writeFixture(t, 18,
		fixtureProfile{"claude-conversion", "claude", "Claude conversion", `{"env":{"ANTHROPIC_AUTH_TOKEN":"secret"}}`, `{"apiFormat":"openai_responses"}`, 0, 0},
		fixtureProfile{"codex-conversion", "codex", "Codex conversion", `{"auth":{"OPENAI_API_KEY":"secret"},"config":"model_provider = \"custom\"\n[model_providers.custom]\nwire_api = \"responses\"\nbase_url = \"https://example.invalid/v1\""}`, `{"apiFormat":"openai_chat"}`, 0, 0},
		fixtureProfile{"codex-env", "codex", "Codex environment", `{"auth":{},"config":"model_provider = \"custom\"\nmodel = \"gpt-test\"\n[model_providers.custom]\nwire_api = \"responses\"\nbase_url = \"https://example.invalid/v1\"\nenv_key = \"PAIRROOM_FIXTURE_CODEX_KEY\""}`, `{"apiFormat":"responses"}`, 0, 0},
	)
	reader, err := NewReader(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := reader.Catalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, profile := range catalog.Profiles {
		if strings.Contains(profile.ProviderRef.ProfileID, "conversion") && (profile.Supported || profile.ReasonCode != ReasonProxyConversion) {
			t.Fatalf("conversion profile was not disabled: %#v", profile)
		}
	}
	resolved, err := reader.Resolve(context.Background(), model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "codex-env"}, model.RuntimeCodex)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != "env-secret" {
		t.Fatal("environment credential was not isolated")
	}
}

func TestReaderIsReadOnlyAndReresolvesProfileChanges(t *testing.T) {
	path := writeFixture(t, 18, fixtureProfile{"c", "claude", "Direct", `{"env":{"ANTHROPIC_AUTH_TOKEN":"first","ANTHROPIC_MODEL":"model-1"}}`, `{}`, 1, 0})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, _ := NewReader(path)
	ref := model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "claude", ProfileID: "c"}
	first, err := reader.Resolve(context.Background(), ref, model.RuntimeClaude)
	if err != nil {
		t.Fatal(err)
	}
	if first.Env["ANTHROPIC_AUTH_TOKEN"] != "first" {
		t.Fatal("first key not resolved")
	}
	after, _ := os.ReadFile(path)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("read-only resolution changed the database")
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE providers SET settings_config = ? WHERE id = 'c' AND app_type = 'claude'`, `{"env":{"ANTHROPIC_AUTH_TOKEN":"second","ANTHROPIC_MODEL":"model-2"}}`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	second, err := reader.Resolve(context.Background(), ref, model.RuntimeClaude)
	if err != nil {
		t.Fatal(err)
	}
	if second.Env["ANTHROPIC_AUTH_TOKEN"] != "second" || second.DefaultModel != "model-2" {
		t.Fatalf("profile edit not re-read: %#v", second)
	}
	db, _ = sql.Open("sqlite", path)
	_, _ = db.Exec(`DELETE FROM providers WHERE id = 'c' AND app_type = 'claude'`)
	_ = db.Close()
	_, err = reader.Resolve(context.Background(), ref, model.RuntimeClaude)
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeProfileMissing {
		t.Fatalf("delete error = %#v", err)
	}
}

func TestReaderFailsClosedForMissingAndMismatchedSchema(t *testing.T) {
	missing, _ := NewReader(filepath.Join(t.TempDir(), "missing.db"))
	_, err := missing.Catalog(context.Background())
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeDatabaseMissing {
		t.Fatalf("missing error = %v", err)
	}
	for _, schema := range []int{17, 19} {
		path := writeFixture(t, schema)
		reader, _ := NewReader(path)
		_, err := reader.Catalog(context.Background())
		if !errors.As(err, &typed) || typed.Code != CodeSchemaMismatch {
			t.Fatalf("schema %d error = %v", schema, err)
		}
	}
}

func TestReaderFailsClosedWhenDatabaseIsExclusivelyLocked(t *testing.T) {
	path := writeFixture(t, 18)
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec("PRAGMA locking_mode=EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	defer writer.Exec("ROLLBACK")
	reader, _ := NewReader(path)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = reader.Catalog(ctx)
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != CodeDatabaseUnreadable {
		t.Fatalf("locked database error = %v", err)
	}
}

func TestCatalogRedactionUsesLongestMatchWithoutReprocessingMarkers(t *testing.T) {
	for _, secrets := range [][]string{{"key", "key-long", "redacted"}, {"redacted", "key-long", "key"}} {
		if got := redactSecrets("key-long key redacted", secrets); got != "[redacted] [redacted] [redacted]" {
			t.Fatalf("partial credential or rewritten marker: %q", got)
		}
	}
}
