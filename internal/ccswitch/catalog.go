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

	"github.com/sean2077/pairroom/internal/model"
	_ "modernc.org/sqlite"
)

const (
	SupportedCCSwitchVersion = "v3.20.1"
	SupportedSchemaVersion   = 18
)

const (
	CodeDatabaseMissing      = "cc_switch_database_missing"
	CodeDatabaseUnreadable   = "cc_switch_database_unreadable"
	CodeSchemaMismatch       = "cc_switch_schema_mismatch"
	CodeProfileMissing       = "cc_switch_profile_missing"
	CodeProfileUnsupported   = "cc_switch_profile_unsupported"
	CodeProfileInvalid       = "cc_switch_profile_invalid"
	CodeRuntimeMismatch      = "cc_switch_runtime_mismatch"
	ReasonUnsupportedApp     = "unsupported_app_type"
	ReasonManagedOAuth       = "managed_oauth"
	ReasonProxyConversion    = "proxy_conversion"
	ReasonFailover           = "failover_profile"
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
	Profiles        []ProfileSummary `json:"profiles"`
}

// Materialization is process-local and intentionally has no JSON tags. Callers
// must copy Env only into the selected child process and must never log it.
type Materialization struct {
	ProviderLabel string            `json:"provider_label"`
	Env           map[string]string `json:"-"`
	Args          []string          `json:"-"`
	Models        []string          `json:"models,omitempty"`
	DefaultModel  string            `json:"default_model,omitempty"`
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
	result := Catalog{CCSwitchVersion: SupportedCCSwitchVersion, Schema: schema}
	for rows.Next() {
		profile, err := scanProfile(rows)
		if err != nil {
			return Catalog{}, dbError(CodeDatabaseUnreadable, "decode a CC Switch profile row", err)
		}
		result.Profiles = append(result.Profiles, summarize(profile))
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
	summary := summarize(profile)
	if summary.Runtime != runtime {
		return Materialization{}, &Error{Code: CodeRuntimeMismatch, Params: safeRefParams(ref), Detail: fmt.Sprintf("CC Switch profile %s/%s is for %s, not %s", profile.AppType, profile.ID, summary.Runtime, runtime)}
	}
	if !summary.Supported {
		return Materialization{}, &Error{Code: CodeProfileUnsupported, Params: map[string]string{"app_type": profile.AppType, "profile_id": profile.ID, "reason": summary.ReasonCode}, Detail: summary.DisabledReason}
	}
	materialized, mapErr := materialize(profile, runtime)
	if mapErr != nil {
		return Materialization{}, mapErr
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
	if schema != SupportedSchemaVersion {
		return closeFailure(&Error{Code: CodeSchemaMismatch, Params: map[string]string{"actual": strconv.Itoa(schema), "supported": strconv.Itoa(SupportedSchemaVersion)}, Detail: fmt.Sprintf("CC Switch schema %d is unsupported; PairRoom supports CC Switch %s schema %d", schema, SupportedCCSwitchVersion, SupportedSchemaVersion)})
	}
	if err := validateProviderTable(ctx, db); err != nil {
		return closeFailure(err)
	}
	return db, schema, nil
}

func validateProviderTable(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info(providers)")
	if err != nil {
		return dbError(CodeDatabaseUnreadable, "inspect the CC Switch providers table", err)
	}
	defer rows.Close()
	want := map[string]bool{"id": false, "app_type": false, "name": false, "settings_config": false, "meta": false, "is_current": false, "in_failover_queue": false}
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
	for name, present := range want {
		if !present {
			return &Error{Code: CodeSchemaMismatch, Params: map[string]string{"actual": strconv.Itoa(SupportedSchemaVersion), "supported": strconv.Itoa(SupportedSchemaVersion)}, Detail: "CC Switch schema 18 is missing required providers." + name}
		}
	}
	return nil
}

func summarize(p profileRow) ProfileSummary {
	runtime := runtimeForAppType(p.AppType)
	s := ProfileSummary{
		ProviderRef: model.ProviderRef{Source: model.ProviderCCSwitch, AppType: p.AppType, ProfileID: p.ID},
		Name:        p.Name, Runtime: runtime, Current: p.Current,
	}
	settings, meta, err := decodeProfileJSON(p)
	if err != nil {
		s.ReasonCode, s.DisabledReason = ReasonInvalidConfig, "Profile configuration is invalid and cannot be materialized safely."
		return s
	}
	// Derive credential candidates only for in-process redaction checks. A
	// malformed or malicious profile must not smuggle a token into a model
	// suggestion, display name, or ProviderRef even when it is later disabled.
	secrets := profileSecretValues(settings, meta)
	s.Name = redactSecrets(s.Name, secrets)
	s.ProviderRef.AppType = redactSecrets(s.ProviderRef.AppType, secrets)
	s.ProviderRef.ProfileID = redactSecrets(s.ProviderRef.ProfileID, secrets)
	for _, candidate := range modelSuggestions(settings) {
		if containsAnySecret(candidate, secrets) {
			s.ReasonCode, s.DisabledReason = ReasonInvalidConfig, "Profile configuration is invalid and cannot be materialized safely."
			return s
		}
		s.Models = append(s.Models, candidate)
	}
	if !runtime.Valid() {
		s.ReasonCode, s.DisabledReason = ReasonUnsupportedApp, "This CC Switch application type is not supported by PairRoom."
		return s
	}
	if p.Failover {
		s.ReasonCode, s.DisabledReason = ReasonFailover, "Failover queue profiles cannot be selected directly."
		return s
	}
	if profileHasManagedOAuth(settings) || profileMetaManagedOAuth(meta) {
		s.ReasonCode, s.DisabledReason = ReasonManagedOAuth, "Managed OAuth profiles remain owned by CC Switch and cannot be materialized independently."
		return s
	}
	if profileRequiresConversion(runtime, meta) {
		s.ReasonCode, s.DisabledReason = ReasonProxyConversion, "Proxy or protocol-conversion profiles require CC Switch global state and cannot be selected."
		return s
	}
	materialized, err := materializeDecoded(p, runtime, settings, meta)
	if err != nil {
		if mapped, ok := err.(*Error); ok {
			s.ReasonCode = mapped.Params["reason"]
			s.DisabledReason = mapped.Detail
		} else {
			s.ReasonCode, s.DisabledReason = ReasonInvalidConfig, "Profile configuration is invalid and cannot be materialized safely."
		}
		return s
	}
	// Keep the catalog's model list sourced from the exact safe materialization
	// rather than from arbitrary nested JSON values.
	s.Models = append([]string(nil), materialized.Models...)
	s.Supported = true
	return s
}

func materialize(p profileRow, runtime model.RuntimeKind) (Materialization, error) {
	settings, meta, err := decodeProfileJSON(p)
	if err != nil {
		return Materialization{}, profileError(p, ReasonInvalidConfig, "CC Switch profile configuration is invalid")
	}
	return materializeDecoded(p, runtime, settings, meta)
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

func materializeDecoded(p profileRow, runtime model.RuntimeKind, settings, meta map[string]any) (Materialization, error) {
	label := "cc-switch:" + p.AppType + "/" + p.ID
	switch runtime.Canonical() {
	case model.RuntimeClaude:
		env := stringMap(settings["env"])
		secret := firstNonEmpty(env["ANTHROPIC_AUTH_TOKEN"], env["ANTHROPIC_API_KEY"])
		if secret == "" {
			return Materialization{}, profileError(p, ReasonMissingCredential, "Claude profile has no API key and cannot be activated independently.")
		}
		allowed := make(map[string]string)
		for key, value := range env {
			if key == "ANTHROPIC_AUTH_TOKEN" || key == "ANTHROPIC_API_KEY" || key == "ANTHROPIC_BASE_URL" || key == "ANTHROPIC_MODEL" || (strings.HasPrefix(key, "ANTHROPIC_DEFAULT_") && (strings.HasSuffix(key, "_MODEL") || strings.HasSuffix(key, "_MODEL_NAME"))) {
				allowed[key] = value
			}
		}
		models := modelSuggestions(settings)
		return finishMaterialization(p, Materialization{ProviderLabel: label, Env: allowed, Models: models, DefaultModel: firstNonEmpty(env["ANTHROPIC_MODEL"], first(models))})
	case model.RuntimeCodex:
		auth := stringMap(settings["auth"])
		if profileHasManagedOAuth(settings) {
			return Materialization{}, profileError(p, ReasonManagedOAuth, "Managed OAuth profiles remain owned by CC Switch and cannot be materialized independently.")
		}
		configText, _ := settings["config"].(string)
		parsed := parseTOML(configText)
		providerID := strings.TrimSpace(parsed.Root["model_provider"])
		if providerID == "" {
			return Materialization{}, profileError(p, ReasonInvalidConfig, "Codex profile does not select a custom model provider.")
		}
		section := parsed.Sections["model_providers."+providerID]
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
		key := strings.TrimSpace(auth["OPENAI_API_KEY"])
		if key == "" {
			key = credentialFromEnvironment(section["env_key"])
		}
		if key == "" {
			return Materialization{}, profileError(p, ReasonMissingCredential, "Codex profile has no available API key and cannot be activated independently.")
		}
		const envKey = "PAIRROOM_CC_SWITCH_CODEX_API_KEY"
		id := "pairroom_ccswitch_" + shortID(p.AppType+"\x00"+p.ID)
		args := []string{
			"-c", "model_provider=" + tomlQuote(id),
			"-c", "model_providers." + id + ".name=" + tomlQuote(firstNonEmpty(section["name"], p.Name)),
			"-c", "model_providers." + id + ".wire_api=\"responses\"",
			"-c", "model_providers." + id + ".env_key=" + tomlQuote(envKey),
			"-c", "model_providers." + id + ".base_url=" + tomlQuote(baseURL),
		}
		models := modelSuggestions(settings)
		return finishMaterialization(p, Materialization{ProviderLabel: label, Env: map[string]string{envKey: key}, Args: args, Models: models, DefaultModel: firstNonEmpty(parsed.Root["model"], first(models))})
	case model.RuntimeGrok:
		configText, _ := settings["config"].(string)
		parsed := parseTOML(configText)
		selected := strings.TrimSpace(parsed.Sections["models"]["default"])
		section := grokModelSection(parsed, selected)
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
			Env:           map[string]string{envKey: key},
			Models:        models,
			DefaultModel:  selected,
			Grok: &GrokProfile{
				ProfileModel: selected, UpstreamModel: upstreamModel, BaseURL: baseURL,
				Name: name, APIBackend: backend, ContextWindow: contextWindow,
			},
		})
	default:
		return Materialization{}, profileError(p, ReasonUnsupportedApp, "This CC Switch application type is not supported by PairRoom.")
	}
}

// finishMaterialization is the last secret boundary before a profile leaves
// the mapper. CC Switch credentials are allowed only in the returned Env map;
// every value that can reach argv, a temporary overlay, RuntimeInfo, or a
// catalog is checked against the credential values. The error deliberately
// contains no profile payload or credential.
func finishMaterialization(profile profileRow, value Materialization) (Materialization, error) {
	secrets := materializationSecrets(value.Env)
	values := append([]string{profile.ID, profile.AppType, profile.Name, value.ProviderLabel, value.DefaultModel}, value.Args...)
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
func profileSecretValues(settings, meta map[string]any) []string {
	values := make([]string, 0, 4)
	collectCredentialValues(settings, &values)
	collectCredentialValues(meta, &values)
	return uniqueSecrets(values)
}

func collectCredentialValues(value any, values *[]string) {
	switch typed := value.(type) {
	case string:
		return
	case []any:
		for _, item := range typed {
			collectCredentialValues(item, values)
		}
	case map[string]any:
		for key, item := range typed {
			lower := strings.ToLower(strings.TrimSpace(key))
			if lower == "env_key" || lower == "env-key" {
				if name, ok := item.(string); ok {
					if secret := credentialFromEnvironment(name); secret != "" {
						*values = append(*values, secret)
					}
				}
			} else if credentialFieldName(lower) {
				if secret, ok := item.(string); ok && strings.TrimSpace(secret) != "" {
					*values = append(*values, strings.TrimSpace(secret))
				}
			}
			if lower == "config" {
				if configText, ok := item.(string); ok {
					doc := parseTOML(configText)
					collectCredentialValues(tomlDocumentMap(doc), values)
				}
			}
			collectCredentialValues(item, values)
		}
	}
}

func tomlDocumentMap(doc tomlDocument) map[string]any {
	result := make(map[string]any, len(doc.Root)+len(doc.Sections))
	for key, value := range doc.Root {
		result[key] = value
	}
	for name, section := range doc.Sections {
		items := make(map[string]any, len(section))
		for key, value := range section {
			items[key] = value
		}
		result[name] = items
	}
	return result
}

func credentialFieldName(value string) bool {
	if value == "env_key" || value == "env-key" {
		return false
	}
	return value == "api_key" || value == "apikey" || value == "auth_token" ||
		value == "access_token" || value == "refresh_token" || value == "credential" ||
		value == "password" || value == "secret" || strings.HasSuffix(value, "_api_key") ||
		strings.HasSuffix(value, "_token") || strings.HasSuffix(value, "_secret")
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
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	return value
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
	return strings.Contains(providerType, "oauth") || boolValue(meta["requiresOAuth"]) || boolValue(meta["requires_oauth"])
}

func profileRequiresConversion(runtime model.RuntimeKind, meta map[string]any) bool {
	mode := strings.ToLower(stringValue(meta["mode"]))
	if mode == "proxy" || boolValue(meta["requiresProxy"]) || boolValue(meta["requires_proxy"]) {
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
	for _, name := range []string{"model." + selected, "model." + strconv.Quote(selected)} {
		if section := parsed.Sections[name]; len(section) > 0 {
			return section
		}
	}
	return nil
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

func flattenStrings(value any) string {
	var values []string
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case string:
			values = append(values, typed)
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				values = append(values, key)
				walk(typed[key])
			}
		}
	}
	walk(value)
	return strings.Join(values, " ")
}

func modelSuggestions(settings map[string]any) []string {
	var candidates []string
	var walk func(string, any)
	walk = func(key string, value any) {
		switch typed := value.(type) {
		case string:
			lower := strings.ToLower(key)
			if lower == "config" {
				parsed := parseTOML(typed)
				candidates = append(candidates, parsed.Root["model"])
				for _, section := range parsed.Sections {
					candidates = append(candidates, section["model"])
				}
			} else if lower == "model" || strings.HasSuffix(lower, "_model") {
				candidates = append(candidates, typed)
			}
		case []any:
			if strings.Contains(strings.ToLower(key), "model") {
				for _, item := range typed {
					if text, ok := item.(string); ok {
						candidates = append(candidates, text)
					}
				}
			}
		case map[string]any:
			for child, item := range typed {
				walk(child, item)
			}
		}
	}
	for key, value := range settings {
		walk(key, value)
	}
	return uniqueStrings(candidates)
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") || strings.Contains(value, "://") {
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

type tomlDocument struct {
	Root     map[string]string
	Sections map[string]map[string]string
}

// parseTOML intentionally recognizes only the scalar subset emitted by CC
// Switch for the three supported provider shapes. Unknown syntax is ignored;
// materialization then fails closed if a required scalar is absent.
func parseTOML(input string) tomlDocument {
	doc := tomlDocument{Root: make(map[string]string), Sections: make(map[string]map[string]string)}
	current := doc.Root
	for _, raw := range strings.Split(input, "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") && !strings.HasPrefix(line, "[[") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			if name == "" {
				current = doc.Root
				continue
			}
			if doc.Sections[name] == nil {
				doc.Sections[name] = make(map[string]string)
			}
			current = doc.Sections[name]
			continue
		}
		key, rawValue, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.Trim(strings.TrimSpace(key), "\"")
		if key == "" {
			continue
		}
		if value, ok := tomlScalar(strings.TrimSpace(rawValue)); ok {
			current[key] = value
		}
	}
	return doc
}

func stripComment(line string) string {
	quoted, escaped := false, false
	for i, r := range line {
		if escaped {
			escaped = false
			continue
		}
		if r == '\\' && quoted {
			escaped = true
			continue
		}
		if r == '"' {
			quoted = !quoted
			continue
		}
		if r == '#' && !quoted {
			return line[:i]
		}
	}
	return line
}

func tomlScalar(value string) (string, bool) {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		unquoted, err := strconv.Unquote(value)
		return unquoted, err == nil
	}
	if value == "true" || value == "false" {
		return value, true
	}
	for _, r := range value {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("-._:/", r)) {
			return "", false
		}
	}
	return value, value != ""
}

func shortID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func tomlQuote(value string) string { return strconv.Quote(value) }
func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
