// Package ccswitch provides a deliberately read-only adapter for the pinned
// CC Switch database contract. PairRoom never updates the database, changes a
// current profile, or exposes raw settings_config/meta values.
package ccswitch

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
	_ "modernc.org/sqlite"
)

// CC Switch bumps user_version for any table change, including tables PairRoom
// never reads. Verified schemas were checked against upstream migrations for the
// providers columns and settings_config/meta semantics PairRoom depends on. A
// newer schema is accepted only when the providers table still has every
// required column, and the catalog marks it unverified; older schemas predate
// the pinned contract and fail closed.
const (
	SupportedCCSwitchVersion = "v4.0.4"
	SupportedSchemaVersion   = 20
	MinimumSchemaVersion     = 18
)

// VerifiedSchemaVersions maps each verified schema to the newest CC Switch
// release checked for it.
var VerifiedSchemaVersions = map[int]string{18: "v3.20.1", 19: "v3.20.4", 20: "v4.0.4"}

// SchemaVerified reports whether schema was checked against upstream
// migrations rather than accepted by structural validation alone.
func SchemaVerified(schema int) bool {
	_, ok := VerifiedSchemaVersions[schema]
	return ok
}

const (
	CodeDatabaseMissing      = "cc_switch_database_missing"
	CodeDatabaseUnreadable   = "cc_switch_database_unreadable"
	CodeSchemaMismatch       = "cc_switch_schema_mismatch"
	CodeSchemaUnverified     = "cc_switch_schema_unverified"
	CodeSchemaIncompatible   = "cc_switch_schema_incompatible"
	CodeProfileMissing       = "cc_switch_profile_missing"
	CodeProfileUnsupported   = "cc_switch_profile_unsupported"
	CodeProfileInvalid       = "cc_switch_profile_invalid"
	CodeRuntimeMismatch      = "cc_switch_runtime_mismatch"
	ReasonUnsupportedApp     = "unsupported_app_type"
	ReasonManagedOAuth       = "managed_oauth"
	ReasonProxyConversion    = "proxy_conversion"
	ReasonMissingCredential  = "missing_api_key"
	ReasonUnsupportedWireAPI = "unsupported_wire_api"
	ReasonInvalidConfig      = "invalid_profile_config"
)

// Error carries a stable localization code and safe interpolation parameters.
// Detail never contains settings_config, meta, credentials, or a database DSN.
type Error struct {
	Code   string
	Params map[string]string
	Detail string
	Cause  error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Detail != "" {
		return e.Detail
	}
	return e.Code
}

func (e *Error) Unwrap() error { return e.Cause }

type ProfileSummary struct {
	ProviderRef    model.ProviderRef `json:"provider"`
	Name           string            `json:"name"`
	Runtime        model.RuntimeKind `json:"runtime"`
	Supported      bool              `json:"supported"`
	DisabledReason string            `json:"disabled_reason,omitempty"`
	ReasonCode     string            `json:"reason_code,omitempty"`
	Models         []string          `json:"models,omitempty"`
	Current        bool              `json:"current,omitempty"`
}

type Catalog struct {
	CCSwitchVersion string           `json:"cc_switch_version"`
	Schema          int              `json:"schema"`
	SchemaVerified  bool             `json:"schema_verified"`
	Profiles        []ProfileSummary `json:"profiles"`
}

// Materialization is process-local and intentionally has no JSON tags. Callers
// must copy Env only into the selected child process and must never log it.
type Materialization struct {
	ProviderLabel string            `json:"provider_label"`
	ProviderName  string            `json:"provider_name,omitempty"`
	Env           map[string]string `json:"-"`
	Args          []string          `json:"-"`
	Models        []string          `json:"models,omitempty"`
	DefaultModel  string            `json:"default_model,omitempty"`
	DefaultEffort string            `json:"default_effort,omitempty"`
	Grok          *GrokProfile      `json:"-"`
}

// GrokProfile contains only the non-secret fields needed to build a
// process-local Grok Build config overlay. CredentialValue is kept separate
// in Materialization.Env and is never rendered into the overlay.
type GrokProfile struct {
	ProfileModel  string
	UpstreamModel string
	BaseURL       string
	Name          string
	APIBackend    string
	ContextWindow int64
}

type Reader struct {
	database string
}

func NewReader(database string) (*Reader, error) {
	path, err := ResolveDatabasePath(database)
	if err != nil {
		return nil, err
	}
	return &Reader{database: path}, nil
}

func ResolveDatabasePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", &Error{Code: CodeDatabaseUnreadable, Detail: "locate the CC Switch database: " + err.Error(), Cause: err}
		}
		value = filepath.Join(home, ".cc-switch", "cc-switch.db")
	} else if !filepath.IsAbs(value) {
		return "", &Error{Code: CodeDatabaseUnreadable, Detail: "cc_switch.database must be an absolute path"}
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", &Error{Code: CodeDatabaseUnreadable, Detail: "resolve the CC Switch database path: " + err.Error(), Cause: err}
	}
	return filepath.Clean(abs), nil
}

func (r *Reader) Catalog(ctx context.Context) (Catalog, error) {
	db, schema, err := r.open(ctx)
	if err != nil {
		return Catalog{}, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, app_type, name, settings_config, meta, is_current, in_failover_queue FROM providers ORDER BY app_type, sort_index, name, id`)
	if err != nil {
		return Catalog{}, dbError(CodeDatabaseUnreadable, "read CC Switch profiles", err)
	}
	defer rows.Close()
	result := Catalog{CCSwitchVersion: VerifiedSchemaVersions[schema], Schema: schema, SchemaVerified: SchemaVerified(schema)}
	for rows.Next() {
		profile, err := scanProfile(rows)
		if err != nil {
			return Catalog{}, dbError(CodeDatabaseUnreadable, "decode a CC Switch profile row", err)
		}
		summary, _ := inspectProfile(profile)
		result.Profiles = append(result.Profiles, summary)
	}
	if err := rows.Err(); err != nil {
		return Catalog{}, dbError(CodeDatabaseUnreadable, "read CC Switch profile rows", err)
	}
	return result, nil
}

func (r *Reader) Resolve(ctx context.Context, ref model.ProviderRef, runtime model.RuntimeKind) (Materialization, error) {
	runtime = runtime.Canonical()
	if err := ref.ValidateForRuntime(runtime); err != nil {
		return Materialization{}, &Error{Code: CodeRuntimeMismatch, Params: safeRefParams(ref), Detail: err.Error()}
	}
	db, _, err := r.open(ctx)
	if err != nil {
		return Materialization{}, err
	}
	defer db.Close()
	row := db.QueryRowContext(ctx, `SELECT id, app_type, name, settings_config, meta, is_current, in_failover_queue FROM providers WHERE id = ? AND app_type = ?`, ref.ProfileID, canonicalAppType(ref.AppType))
	profile, err := scanProfile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Materialization{}, &Error{Code: CodeProfileMissing, Params: safeRefParams(ref), Detail: fmt.Sprintf("CC Switch profile %s/%s no longer exists", canonicalAppType(ref.AppType), ref.ProfileID)}
	}
	if err != nil {
		return Materialization{}, dbError(CodeDatabaseUnreadable, "read the selected CC Switch profile", err)
	}
	summary, materialized := inspectProfile(profile)
	if summary.Runtime != runtime {
		return Materialization{}, &Error{Code: CodeRuntimeMismatch, Params: safeRefParams(ref), Detail: fmt.Sprintf("CC Switch profile %s/%s is for %s, not %s", profile.AppType, profile.ID, summary.Runtime, runtime)}
	}
	if !summary.Supported {
		return Materialization{}, &Error{Code: CodeProfileUnsupported, Params: map[string]string{"app_type": summary.ProviderRef.AppType, "profile_id": summary.ProviderRef.ProfileID, "reason": summary.ReasonCode}, Detail: summary.DisabledReason}
	}
	return materialized, nil
}

type profileRow struct {
	ID, AppType, Name, Settings, Meta string
	Current, Failover                 bool
}

type scanner interface{ Scan(...any) error }

func scanProfile(row scanner) (profileRow, error) {
	var p profileRow
	var current, failover int
	err := row.Scan(&p.ID, &p.AppType, &p.Name, &p.Settings, &p.Meta, &current, &failover)
	p.AppType = canonicalAppType(p.AppType)
	p.Current, p.Failover = current != 0, failover != 0
	return p, err
}

func (r *Reader) open(ctx context.Context) (*sql.DB, int, error) {
	info, err := os.Stat(r.database)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, &Error{Code: CodeDatabaseMissing, Detail: "CC Switch database was not found; install or open CC Switch, or configure an absolute cc_switch.database path"}
	}
	if err != nil || info.IsDir() {
		if err == nil {
			err = errors.New("path is a directory")
		}
		return nil, 0, dbError(CodeDatabaseUnreadable, "open the CC Switch database", err)
	}
	dsnPath := filepath.ToSlash(r.database)
	if filepath.VolumeName(r.database) != "" && !strings.HasPrefix(dsnPath, "/") {
		dsnPath = "/" + dsnPath
	}
	dsnURL := &url.URL{Scheme: "file", Path: dsnPath}
	query := dsnURL.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "query_only(1)")
	query.Add("_pragma", "busy_timeout(750)")
	dsnURL.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", dsnURL.String())
	if err != nil {
		return nil, 0, dbError(CodeDatabaseUnreadable, "open the CC Switch database", err)
	}
	db.SetMaxOpenConns(1)
	closeFailure := func(cause error) (*sql.DB, int, error) {
		_ = db.Close()
		return nil, 0, cause
	}
	if err := db.PingContext(ctx); err != nil {
		return closeFailure(dbError(CodeDatabaseUnreadable, "open the CC Switch database read-only", err))
	}
	var queryOnly, schema int
	if err := db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		if err == nil {
			err = errors.New("SQLite query_only was not enabled")
		}
		return closeFailure(dbError(CodeDatabaseUnreadable, "enforce read-only CC Switch access", err))
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema); err != nil {
		return closeFailure(dbError(CodeDatabaseUnreadable, "read the CC Switch schema version", err))
	}
	if schema < MinimumSchemaVersion {
		return closeFailure(&Error{Code: CodeSchemaMismatch, Params: map[string]string{"actual": strconv.Itoa(schema), "supported": strconv.Itoa(MinimumSchemaVersion)}, Detail: fmt.Sprintf("CC Switch schema %d is unsupported; PairRoom requires schema %d or newer (verified through CC Switch %s schema %d); upgrade CC Switch", schema, MinimumSchemaVersion, SupportedCCSwitchVersion, SupportedSchemaVersion)})
	}
	if err := validateProviderTable(ctx, db, schema); err != nil {
		return closeFailure(err)
	}
	return db, schema, nil
}

func validateProviderTable(ctx context.Context, db *sql.DB, schema int) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(providers)")
	if err != nil {
		return dbError(CodeDatabaseUnreadable, "inspect the CC Switch providers table", err)
	}
	defer rows.Close()
	want := map[string]bool{"id": false, "app_type": false, "name": false, "settings_config": false, "meta": false, "sort_index": false, "is_current": false, "in_failover_queue": false}
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &pk); err != nil {
			return dbError(CodeDatabaseUnreadable, "inspect the CC Switch providers table", err)
		}
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		return dbError(CodeDatabaseUnreadable, "inspect the CC Switch providers table", err)
	}
	for name, present := range want {
		if !present {
			return &Error{Code: CodeSchemaIncompatible, Params: map[string]string{"actual": strconv.Itoa(schema), "column": name}, Detail: fmt.Sprintf("CC Switch schema %d is missing required providers.%s", schema, name)}
		}
	}
	return nil
}

// inspectProfile owns one fresh row snapshot. Catalog and Resolve share its
// validation and materialization, but never cache it across database reads.
func inspectProfile(p profileRow) (ProfileSummary, Materialization) {
	runtime := runtimeForAppType(p.AppType)
	s := ProfileSummary{
		ProviderRef: model.ProviderRef{Source: model.ProviderCCSwitch, AppType: p.AppType, ProfileID: p.ID},
		Name:        p.Name, Runtime: runtime, Current: p.Current,
	}
	settings, meta, err := decodeProfileJSON(p)
	if err != nil {
		return uninspectableSummary(s), Materialization{}
	}
	if runtime == model.RuntimeClaude && claudeOpaqueSettings(settings) {
		return uninspectableSummary(s), Materialization{}
	}
	configText, _ := settings["config"].(string)
	parsed := parseTOML(configText)
	if uninspectableTOMLCredentials(parsed, runtime) {
		// An opaque or malformed credential declaration cannot be redacted
		// reliably. Withhold row-authored metadata as well as materialization.
		return uninspectableSummary(s), Materialization{}
	}
	// Derive credential candidates only for in-process redaction checks. A
	// malformed or malicious profile must not smuggle a token into a model
	// suggestion, display name, or ProviderRef even when it is later disabled.
	secrets, unsafeCredentials := profileSecretValues(settings, meta, parsed)
	if unsafeCredentials {
		return uninspectableSummary(s), Materialization{}
	}
	// Sanitize before redacting: stripping control characters first would
	// otherwise rejoin a credential that was split across them, producing a value
	// the redaction pass can no longer recognize.
	s.Name = redactSecrets(sanitizeProviderName(p.Name), secrets)
	s.ProviderRef.AppType = redactSecrets(s.ProviderRef.AppType, secrets)
	s.ProviderRef.ProfileID = redactSecrets(s.ProviderRef.ProfileID, secrets)
	models := profileModels(runtime, settings, meta, parsed)
	for _, candidate := range models {
		if containsAnySecret(candidate, secrets) {
			s.ReasonCode, s.DisabledReason = ReasonInvalidConfig, "Profile configuration is invalid and cannot be materialized safely."
			return s, Materialization{}
		}
		s.Models = append(s.Models, candidate)
	}
	if !runtime.Valid() {
		s.ReasonCode, s.DisabledReason = ReasonUnsupportedApp, "This CC Switch application type is not supported by PairRoom."
		return s, Materialization{}
	}
	// Queue membership is not a credential or routing mode. A direct API-key
	// provider can also participate in CC Switch failover or aggregation; this
	// reference still selects only that provider, without either routing policy.
	if profileHasManagedOAuth(settings) || profileMetaManagedOAuth(meta) {
		s.ReasonCode, s.DisabledReason = ReasonManagedOAuth, "Managed OAuth profiles remain owned by CC Switch and cannot be materialized independently."
		return s, Materialization{}
	}
	if profileRequiresConversion(runtime, meta) {
		s.ReasonCode, s.DisabledReason = ReasonProxyConversion, "Proxy or protocol-conversion profiles require CC Switch global state and cannot be selected."
		return s, Materialization{}
	}
	materialized, err := materializeSnapshot(p, runtime, profileSnapshot{settings: settings, meta: meta, config: parsed, secrets: secrets, models: models})
	if err != nil {
		if mapped, ok := err.(*Error); ok {
			s.ReasonCode = mapped.Params["reason"]
			s.DisabledReason = mapped.Detail
		} else {
			s.ReasonCode, s.DisabledReason = ReasonInvalidConfig, "Profile configuration is invalid and cannot be materialized safely."
		}
		return s, Materialization{}
	}
	// Keep the catalog's model list sourced from the exact safe materialization
	// rather than from arbitrary nested JSON values.
	s.Models = append([]string(nil), materialized.Models...)
	s.Supported = true
	return s, materialized
}

func uninspectableSummary(s ProfileSummary) ProfileSummary {
	s.Name = "Invalid CC Switch profile"
	s.ProviderRef.ProfileID = ""
	if s.Runtime.Valid() {
		s.ProviderRef.AppType = canonicalAppType(string(s.Runtime))
	} else {
		s.ProviderRef.AppType = ""
	}
	s.ReasonCode, s.DisabledReason = ReasonInvalidConfig, "Profile configuration cannot be inspected safely; repair it in CC Switch."
	return s
}

func decodeProfileJSON(p profileRow) (map[string]any, map[string]any, error) {
	settings := make(map[string]any)
	meta := make(map[string]any)
	if err := json.Unmarshal([]byte(p.Settings), &settings); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(p.Meta) != "" {
		if err := json.Unmarshal([]byte(p.Meta), &meta); err != nil {
			return nil, nil, err
		}
	}
	return settings, meta, nil
}

// profileSnapshot is private to one inspectProfile call. Its parsed TOML,
// model suggestions, and credential set are reused by every projection check.
type profileSnapshot struct {
	settings, meta  map[string]any
	config          tomlDocument
	secrets, models []string
}

func materializeSnapshot(p profileRow, runtime model.RuntimeKind, snapshot profileSnapshot) (Materialization, error) {
	settings, meta, parsed := snapshot.settings, snapshot.meta, snapshot.config
	label := "cc-switch:" + p.AppType + "/" + p.ID
	// The Profile display name travels with the internal reference label so a UI
	// can show a human-readable Provider without parsing the reference form. It
	// is deliberately not called `name`: the Grok branch already uses that for
	// the TOML [model.<x>] name, which is a different concept.
	// Sanitize before redacting (a credential split across stripped control
	// characters must not be rejoined past the redaction pass), and redact
	// against the full profile secret set rather than only the materialized
	// environment, so a credential in a field PairRoom never copies into Env
	// still cannot survive in the display name.
	secrets := snapshot.secrets
	providerName := redactSecrets(sanitizeProviderName(p.Name), secrets)
	switch runtime.Canonical() {
	case model.RuntimeClaude:
		env := stringMap(settings["env"])
		if err := validateClaudeProfile(p, settings, env); err != nil {
			return Materialization{}, err
		}
		secret := firstNonEmpty(env["ANTHROPIC_AUTH_TOKEN"], env["ANTHROPIC_API_KEY"])
		if secret == "" {
			return Materialization{}, profileError(p, ReasonMissingCredential, "Claude profile has no API key and cannot be activated independently.")
		}
		if isProxyCredential(secret) {
			return Materialization{}, profileError(p, ReasonProxyConversion, "CC Switch routing credentials cannot be activated as a direct profile.")
		}
		allowed := make(map[string]string)
		for key, value := range env {
			if claudeDirectEnvKey(key) {
				allowed[key] = value
			}
		}
		return finishMaterialization(p, Materialization{ProviderLabel: label, ProviderName: providerName, Env: allowed, Args: claudeCompatibilityArgs(allowed), Models: snapshot.models, DefaultModel: firstNonEmpty(env["ANTHROPIC_MODEL"], stringValue(settings["model"]))}, secrets)
	case model.RuntimeCodex:
		auth := stringMap(settings["auth"])
		section, err := codexProviderSection(p, parsed)
		if err != nil {
			return Materialization{}, err
		}
		wire := strings.ToLower(firstNonEmpty(section["wire_api"], stringValue(meta["apiFormat"])))
		if wire == "response" {
			wire = "responses"
		}
		if wire != "responses" {
			return Materialization{}, profileError(p, ReasonUnsupportedWireAPI, "Codex profile must use the Responses wire API.")
		}
		baseURL := strings.TrimSpace(section["base_url"])
		if baseURL == "" {
			return Materialization{}, profileError(p, ReasonInvalidConfig, "Codex profile does not provide a custom provider base URL.")
		}
		if err := validateEndpoint(baseURL); err != nil {
			return Materialization{}, profileError(p, ReasonInvalidConfig, "Codex profile base URL cannot be materialized safely: "+err.Error())
		}
		key := firstNonEmpty(auth["OPENAI_API_KEY"], section["experimental_bearer_token"], parsed.Root["experimental_bearer_token"])
		if envName, declared := section["env_key"]; declared {
			// CC Switch 4 keeps an explicit credential source authoritative. An
			// unavailable env_key must not fall back to a stale auth.json snapshot.
			key = credentialFromEnvironment(envName)
		}
		if key == "" {
			return Materialization{}, profileError(p, ReasonMissingCredential, "Codex profile has no available API key and cannot be activated independently.")
		}
		if isProxyCredential(key) {
			return Materialization{}, profileError(p, ReasonProxyConversion, "CC Switch routing credentials cannot be activated as a direct profile.")
		}
		const envKey = "PAIRROOM_CC_SWITCH_CODEX_API_KEY"
		id := "pairroom_ccswitch_" + shortID(p.AppType+"\x00"+p.ID)
		args := []string{
			"-c", "model_provider=" + tomlQuote(id),
			"-c", "model_providers." + id + ".name=" + tomlQuote(firstNonEmpty(section["name"], p.Name)),
			"-c", "model_providers." + id + ".wire_api=\"responses\"",
			"-c", "model_providers." + id + ".env_key=" + tomlQuote(envKey),
			"-c", "model_providers." + id + ".base_url=" + tomlQuote(baseURL),
			"-c", "model_providers." + id + ".requires_openai_auth=false",
		}
		options, err := codexProfileOptions(p, parsed, id)
		if err != nil {
			return Materialization{}, err
		}
		args = append(args, options...)
		return finishMaterialization(p, Materialization{ProviderLabel: label, ProviderName: providerName, Env: map[string]string{envKey: key}, Args: args, Models: snapshot.models, DefaultModel: parsed.Root["model"], DefaultEffort: parsed.Root["model_reasoning_effort"]}, secrets)
	case model.RuntimeGrok:
		selected := strings.TrimSpace(parsed.Sections["models"]["default"])
		sectionName := "model." + tomlKey(selected)
		for _, path := range []string{
			"models.default", sectionName + ".model", sectionName + ".name",
			sectionName + ".base_url", sectionName + ".api_backend",
		} {
			if !supportedTOMLScalar(parsed, path, tomlString) {
				return Materialization{}, profileError(p, ReasonInvalidConfig, "Grok Build profile contains an invalid model or credential declaration.")
			}
		}
		section := grokModelSection(parsed, selected)
		if unsupportedTOMLPrefix(parsed, sectionName) {
			return Materialization{}, profileError(p, ReasonInvalidConfig, "Grok Build profile contains unsupported provider configuration syntax.")
		}
		if selected == "" || len(section) == 0 {
			return Materialization{}, profileError(p, ReasonInvalidConfig, "Grok Build profile does not contain the selected [model.<name>] table.")
		}
		key := strings.TrimSpace(section["api_key"])
		if key == "" {
			key = credentialFromEnvironment(section["env_key"])
		}
		if key == "" {
			return Materialization{}, profileError(p, ReasonMissingCredential, "Grok Build profile has no available direct API key.")
		}
		if isProxyCredential(key) {
			return Materialization{}, profileError(p, ReasonProxyConversion, "CC Switch routing credentials cannot be activated as a direct profile.")
		}
		backend := strings.ToLower(strings.TrimSpace(section["api_backend"]))
		if backend == "" {
			backend = "responses"
		}
		if backend != "responses" && backend != "chat_completions" && backend != "messages" {
			return Materialization{}, profileError(p, ReasonUnsupportedWireAPI, "Grok Build profile uses an unsupported API backend.")
		}
		baseURL := strings.TrimSpace(section["base_url"])
		if err := validateEndpoint(baseURL); err != nil {
			return Materialization{}, profileError(p, ReasonInvalidConfig, "Grok Build profile base URL cannot be materialized safely: "+err.Error())
		}
		upstreamModel := strings.TrimSpace(section["model"])
		name := strings.TrimSpace(section["name"])
		if upstreamModel == "" || name == "" {
			return Materialization{}, profileError(p, ReasonInvalidConfig, "Grok Build profile is missing model or name.")
		}
		contextWindow, parseErr := strconv.ParseInt(strings.TrimSpace(section["context_window"]), 10, 64)
		if parseErr != nil || contextWindow <= 0 {
			return Materialization{}, profileError(p, ReasonInvalidConfig, "Grok Build profile context_window must be a positive integer.")
		}
		const envKey = "PAIRROOM_CC_SWITCH_GROK_API_KEY"
		models := uniqueStrings([]string{selected, upstreamModel})
		return finishMaterialization(p, Materialization{
			ProviderLabel: label,
			ProviderName:  providerName,
			Env:           map[string]string{envKey: key},
			Models:        models,
			DefaultModel:  selected,
			Grok: &GrokProfile{
				ProfileModel: selected, UpstreamModel: upstreamModel, BaseURL: baseURL,
				Name: name, APIBackend: backend, ContextWindow: contextWindow,
			},
		}, secrets)
	default:
		return Materialization{}, profileError(p, ReasonUnsupportedApp, "This CC Switch application type is not supported by PairRoom.")
	}
}

// finishMaterialization is the last secret boundary before a profile leaves
// the mapper. CC Switch credentials are allowed only in the returned Env map;
// every value that can reach argv, a temporary overlay, RuntimeInfo, or a
// catalog is checked against the credential values. The error deliberately
// contains no profile payload or credential.
func finishMaterialization(profile profileRow, value Materialization, profileSecrets []string) (Materialization, error) {
	if len(value.DefaultModel) > 512 || len(value.DefaultEffort) > 128 ||
		strings.ContainsFunc(value.DefaultModel+value.DefaultEffort, unicode.IsControl) {
		return Materialization{}, profileError(profile, ReasonInvalidConfig, "CC Switch profile contains an invalid model or effort value.")
	}
	secrets := append(materializationSecrets(value.Env), profileSecrets...)
	values := append([]string{profile.ID, profile.AppType, profile.Name, value.ProviderLabel, value.ProviderName, value.DefaultModel, value.DefaultEffort}, value.Args...)
	// Inspect the raw provider options as well as their serialized arguments.
	// JSON escaping can otherwise conceal a credential copied into --settings.
	for key, item := range value.Env {
		if !credentialEnvironmentName(key) {
			values = append(values, item)
		}
	}
	values = append(values, value.Models...)
	if value.Grok != nil {
		values = append(values, value.Grok.ProfileModel, value.Grok.UpstreamModel, value.Grok.BaseURL, value.Grok.Name, value.Grok.APIBackend)
	}
	if containsAnySecretInValues(values, secrets) {
		return Materialization{}, profileError(profile, ReasonInvalidConfig, "CC Switch profile contains a credential in a non-secret field and cannot be materialized safely.")
	}
	return value, nil
}

func materializationSecrets(env map[string]string) []string {
	var values []string
	for key, value := range env {
		if strings.TrimSpace(value) == "" || !credentialEnvironmentName(key) {
			continue
		}
		values = append(values, strings.TrimSpace(value))
	}
	return uniqueSecrets(values)
}

func credentialEnvironmentName(value string) bool {
	name := strings.ToUpper(strings.TrimSpace(value))
	if name == "ANTHROPIC_AUTH_TOKEN" || name == "ANTHROPIC_API_KEY" ||
		name == "OPENAI_API_KEY" || strings.HasSuffix(name, "_API_KEY") ||
		strings.HasSuffix(name, "_AUTH_TOKEN") || strings.HasSuffix(name, "_ACCESS_TOKEN") ||
		strings.HasSuffix(name, "_SECRET") || strings.Contains(name, "_PASSWORD") {
		return true
	}
	return strings.HasPrefix(name, "PAIRROOM_CC_SWITCH_") && strings.Contains(name, "KEY")
}

// profileSecretValues walks only credential-shaped fields. It is used for
// catalog redaction before support classification, including disabled OAuth
// and failover rows. env_key values are variable names, so the corresponding
// process environment value is resolved for the safety check but the name
// itself is never treated as a secret.
func profileSecretValues(settings, meta map[string]any, parsed tomlDocument) ([]string, bool) {
	values := make([]string, 0, 4)
	unsafeCredentials := false
	configText, _ := settings["config"].(string)
	var collect func(any, []string)
	collect = func(value any, path []string) {
		switch typed := value.(type) {
		case string:
			if credential := jsonCredentialField(path); credential != "" {
				appendCredentialValue(typed, credential, &values)
			}
		case []any:
			for _, item := range typed {
				collect(item, path)
			}
		case map[string]any:
			for key, item := range typed {
				field := strings.ToLower(strings.TrimSpace(key))
				if field == "config" {
					if text, ok := item.(string); ok {
						// Reuse the row's parsed configuration. An additional nested
						// snapshot, if present, still receives credential inspection.
						doc := parsed
						if text != configText {
							doc = parseTOML(text)
						}
						// A credential-shaped opaque value cannot be redacted.
						// Generic malformed text in unused metadata is not an
						// active provider declaration and does not disable it.
						unsafeCredentials = unsafeCredentials || doc.UnsafeCredentials
						collectTOMLCredentialValues(doc, &values)
					}
				}
				collect(item, append(path, field))
			}
		}
	}
	collect(settings, nil)
	collect(meta, nil)
	return uniqueSecrets(values), unsafeCredentials
}

func jsonCredentialField(path []string) string {
	var credential string
	var parent string
	for i, field := range path {
		if credentialJSONFieldName(field, parent, i+1 < len(path)) {
			credential = field
		}
		if parent != "" {
			parent += "."
		}
		parent += field
	}
	return credential
}

func collectTOMLCredentialValues(doc tomlDocument, values *[]string) {
	for path := range doc.ScalarKinds {
		if field := tomlCredentialField(path, false); field != "" {
			appendCredentialValue(tomlScalarValue(doc, path), field, values)
		}
	}
}

func appendCredentialValue(value, field string, values *[]string) {
	if field == "env_key" || field == "env-key" {
		value = credentialFromEnvironment(value)
	}
	if value = strings.TrimSpace(value); value != "" {
		*values = append(*values, value)
	}
}

// Some JSON snapshots have flattened credential keys. Keep their historical
// redaction, but do not reinterpret a quoted TOML component as a dotted path.
func credentialJSONFieldName(value, parent string, container bool) bool {
	if offset := strings.LastIndexByte(value, '.'); offset >= 0 {
		if parent != "" {
			parent += "."
		}
		parent += value[:offset]
		value = strings.Trim(value[offset+1:], `"'`)
		// Preserve the legacy flattened-field redaction rule. A plain env_key
		// is a reference, but a literal dotted JSON key was never one.
		if value == "env_key" || value == "env-key" {
			return false
		}
	}
	// A data name never reopens a collection. A server named model must keep
	// the env map below it in its environment role.
	dataName := false
	for _, part := range strings.Split(parent, ".") {
		parent = strings.Trim(part, `"'`)
		if dataName {
			parent = ""
			dataName = false
			continue
		}
		collection := parent
		if collection == "mcpservers" {
			collection = "mcp_servers" // JSON uses a camel-case spelling.
		}
		dataName = credentialDataCollection(collection)
	}
	return credentialFieldName(value, parent, container)
}

func credentialDataCollection(value string) bool {
	return value == "model_providers" || value == "model" || value == "mcp_servers"
}

// Exact credential names establish container semantics. Suffix matching is a
// leaf heuristic, except in env/auth maps where the key names a credential
// variable. A namespace such as my_token.command is not itself a secret source.
func credentialFieldName(value, parent string, container bool) bool {
	if value == "env_key" || value == "env-key" {
		return true
	}
	if value == "api_key" || value == "apikey" || value == "auth_token" || value == "token" || value == "authorization" ||
		value == "access_token" || value == "refresh_token" || value == "credential" ||
		value == "password" || value == "secret" || value == "experimental_bearer_token" {
		return true
	}
	return (!container || parent == "env" || parent == "auth") && (strings.HasSuffix(value, "_api_key") ||
		strings.HasSuffix(value, "_token") || strings.HasSuffix(value, "_secret"))
}

func uniqueSecrets(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 16<<10 {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func containsAnySecret(value string, secrets []string) bool {
	for _, secret := range secrets {
		if secret != "" && (strings.Contains(value, secret) || strings.Contains(value, strconv.Quote(secret))) {
			return true
		}
	}
	return false
}

func containsAnySecretInValues(values, secrets []string) bool {
	for _, value := range values {
		if containsAnySecret(value, secrets) {
			return true
		}
	}
	return false
}

func redactSecrets(value string, secrets []string) string {
	// Longest matches win, and replacements are never scanned again. A short
	// credential must not expose the suffix of another or rewrite the marker.
	ordered := uniqueSecrets(secrets)
	sort.Slice(ordered, func(i, j int) bool {
		if len(ordered[i]) != len(ordered[j]) {
			return len(ordered[i]) > len(ordered[j])
		}
		return ordered[i] < ordered[j]
	})
	pairs := make([]string, 0, len(ordered)*2)
	for _, secret := range ordered {
		pairs = append(pairs, secret, "[redacted]")
	}
	return strings.NewReplacer(pairs...).Replace(value)
}

// maxProviderNameBytes bounds a CC Switch Profile display name before it can
// reach a catalog, RuntimeInfo, or a durable event. The limit matches the
// Room-name bound so one external string cannot dominate a projection.
const maxProviderNameBytes = 160

// sanitizeProviderName strips control characters and bounds a Profile display
// name. The name is not a credential: finishMaterialization fails closed when
// a secret appears in it. It is still external input that reaches the browser
// and the append-only event log, so it must be control-free and length-bounded.
// Truncation walks back to a rune boundary so a multibyte name is never split.
func sanitizeProviderName(value string) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, value))
	if len(value) <= maxProviderNameBytes {
		return value
	}
	cut := maxProviderNameBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut])
}

func profileError(p profileRow, reason, detail string) error {
	return &Error{Code: CodeProfileUnsupported, Params: map[string]string{"app_type": p.AppType, "profile_id": p.ID, "reason": reason}, Detail: detail}
}

func dbError(code, action string, err error) error {
	detail := action + " failed"
	if err != nil {
		detail += ": " + sanitizeSQLiteError(err.Error())
	}
	return &Error{Code: code, Detail: detail, Cause: err}
}

func sanitizeSQLiteError(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	if len(value) > 320 {
		value = value[:320] + "…"
	}
	return value
}

func safeRefParams(ref model.ProviderRef) map[string]string {
	return map[string]string{"app_type": canonicalAppType(ref.AppType), "profile_id": strings.TrimSpace(ref.ProfileID)}
}

func canonicalAppType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claude-code", "claudecode", "claude_code":
		return "claude"
	case "codex":
		return "codex"
	case "grok", "grok-build", "grokbuild", "grok_build":
		return "grokbuild"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func runtimeForAppType(value string) model.RuntimeKind {
	switch canonicalAppType(value) {
	case "claude":
		return model.RuntimeClaude
	case "codex":
		return model.RuntimeCodex
	case "grokbuild":
		return model.RuntimeGrok
	default:
		return ""
	}
}

func profileHasManagedOAuth(settings map[string]any) bool {
	auth, _ := settings["auth"].(map[string]any)
	if len(auth) == 0 {
		return false
	}
	if _, ok := auth["tokens"]; ok {
		return true
	}
	mode := strings.ToLower(stringValue(auth["auth_mode"]))
	return strings.Contains(mode, "oauth") || strings.Contains(mode, "chatgpt")
}

func profileMetaManagedOAuth(meta map[string]any) bool {
	providerType := strings.ToLower(firstNonEmpty(stringValue(meta["providerType"]), stringValue(meta["provider_type"])))
	binding, _ := meta["authBinding"].(map[string]any)
	return strings.Contains(providerType, "oauth") || providerType == "github_copilot" ||
		stringValue(binding["source"]) == "managed_account" ||
		boolValue(meta["requiresOAuth"]) || boolValue(meta["requires_oauth"])
}

func profileRequiresConversion(runtime model.RuntimeKind, meta map[string]any) bool {
	mode := strings.ToLower(stringValue(meta["mode"]))
	if mode == "proxy" || mode == "routing" || mode == "aggregation" || boolValue(meta["isFullUrl"]) || boolValue(meta["requiresProxy"]) || boolValue(meta["requires_proxy"]) {
		return true
	}
	format := strings.ToLower(firstNonEmpty(stringValue(meta["apiFormat"]), stringValue(meta["api_format"])))
	switch runtime.Canonical() {
	case model.RuntimeClaude:
		return format != "" && format != "anthropic"
	case model.RuntimeCodex:
		return format != "" && format != "responses" && format != "openai_responses"
	default:
		return false
	}
}

func boolValue(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, _ := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed
	default:
		return false
	}
}

func credentialFromEnvironment(name string) string {
	name = strings.TrimSpace(name)
	if !validEnvironmentName(name) {
		return ""
	}
	return strings.TrimSpace(os.Getenv(name))
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || index > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func validateEndpoint(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("base URL is empty")
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return errors.New("base URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return errors.New("base URL must not contain user credentials")
	}
	if parsed.Fragment != "" {
		return errors.New("base URL must not contain a fragment")
	}
	return nil
}

func grokModelSection(parsed tomlDocument, selected string) map[string]string {
	return parsed.Sections["model."+tomlKey(selected)]
}

// RenderGrokOverlay creates a non-secret per-process overlay for one CC Switch
// Grok Build profile. The selected model is used as a quoted TOML table key,
// so custom model IDs remain data rather than syntax.
func RenderGrokOverlay(profile *GrokProfile, requestedModel string) (string, string, error) {
	if profile == nil {
		return "", "", errors.New("Grok Build profile materialization is missing")
	}
	effective := strings.TrimSpace(requestedModel)
	upstream := profile.UpstreamModel
	if effective == "" {
		effective = profile.ProfileModel
	} else if effective != profile.ProfileModel {
		upstream = effective
	}
	if effective == "" || upstream == "" {
		return "", "", errors.New("Grok Build model is empty")
	}
	const envKey = "PAIRROOM_CC_SWITCH_GROK_API_KEY"
	text := "[models]\n" +
		"default = " + tomlQuote(effective) + "\n\n" +
		"[model." + tomlQuote(effective) + "]\n" +
		"model = " + tomlQuote(upstream) + "\n" +
		"base_url = " + tomlQuote(profile.BaseURL) + "\n" +
		"name = " + tomlQuote(profile.Name) + "\n" +
		"env_key = " + tomlQuote(envKey) + "\n" +
		"api_backend = " + tomlQuote(profile.APIBackend) + "\n" +
		"context_window = " + strconv.FormatInt(profile.ContextWindow, 10) + "\n"
	return text, effective, nil
}

func stringMap(value any) map[string]string {
	input, _ := value.(map[string]any)
	result := make(map[string]string, len(input))
	for key, raw := range input {
		if text, ok := raw.(string); ok {
			result[key] = strings.TrimSpace(text)
		}
	}
	return result
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 || strings.ContainsFunc(value, unicode.IsControl) || strings.Contains(value, "://") {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
		if len(result) == 50 {
			break
		}
	}
	sort.Strings(result)
	return result
}

func shortID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func tomlQuote(value string) string { return strconv.Quote(value) }
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
