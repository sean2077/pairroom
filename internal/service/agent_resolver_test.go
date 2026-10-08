package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/ccswitch"
	"github.com/sean2077/pairroom/internal/config"
	"github.com/sean2077/pairroom/internal/model"
	_ "modernc.org/sqlite"
)

func TestAgentResolverIsolatesConcurrentProfilesAndRefreshesOnlyOnResolve(t *testing.T) {
	database := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
		settings_config TEXT NOT NULL, meta TEXT NOT NULL DEFAULT '{}', sort_index INTEGER,
		is_current BOOLEAN NOT NULL DEFAULT 0, in_failover_queue BOOLEAN NOT NULL DEFAULT 0,
		PRIMARY KEY (id, app_type)); PRAGMA user_version = 18;`); err != nil {
		t.Fatal(err)
	}
	profile := func(alias, upstream, secret string) string {
		configText := "[models]\ndefault = \"" + alias + "\"\n[model.\"" + alias + "\"]\nmodel = \"" + upstream + "\"\nbase_url = \"https://example.invalid/v1\"\nname = \"Fixture\"\napi_key = \"" + secret + "\"\napi_backend = \"responses\"\ncontext_window = 500000\n"
		encoded, marshalErr := json.Marshal(map[string]string{"config": configText})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return string(encoded)
	}
	for _, row := range []struct{ id, settings string }{
		{"profile-a", profile("alpha", "model-a", "secret-a")},
		{"profile-b", profile("beta", "model-b", "secret-b")},
	} {
		if _, err := db.Exec(`INSERT INTO providers (id, app_type, name, settings_config, meta) VALUES (?, 'grokbuild', ?, ?, '{"apiFormat":"responses"}')`, row.id, row.id, row.settings); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := ccswitch.NewReader(database)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewAgentResolver(AgentResolverConfig{
		Defaults: defaultAgentSelections(),
		Runtimes: config.RuntimeTemplates{
			Claude: config.RuntimeTemplate{Command: "claude"},
			Codex:  config.RuntimeTemplate{Command: "codex"},
			Grok:   config.RuntimeTemplate{Command: "grok"},
		},
		CCSwitch: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	selection := func(id, modelName string) model.AgentSelection {
		return model.AgentSelection{
			Runtime:  model.RuntimeGrok,
			Provider: model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "grokbuild", ProfileID: id},
			Model:    modelName,
		}
	}
	type result struct {
		cfg agent.Config
		err error
	}
	results := make([]result, 2)
	var group sync.WaitGroup
	for index, test := range []struct {
		actor         model.ActorID
		id, modelName string
	}{
		{model.ActorSlot1, "profile-a", "custom-a"},
		{model.ActorSlot2, "profile-b", "custom-b"},
	} {
		group.Add(1)
		go func(index int, test struct {
			actor         model.ActorID
			id, modelName string
		}) {
			defer group.Done()
			results[index].cfg, results[index].err = resolver.Resolve(context.Background(), test.actor, selection(test.id, test.modelName), model.RuntimeGrok, t.TempDir(), t.TempDir())
		}(index, test)
	}
	group.Wait()
	for _, result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
	}
	if results[0].cfg.Env["PAIRROOM_CC_SWITCH_GROK_API_KEY"] != "secret-a" || results[1].cfg.Env["PAIRROOM_CC_SWITCH_GROK_API_KEY"] != "secret-b" {
		t.Fatal("profile environments crossed")
	}
	if results[0].cfg.Env["GROK_CONFIG_PATH"] == results[1].cfg.Env["GROK_CONFIG_PATH"] {
		t.Fatal("concurrent Rooms shared a Grok Build overlay")
	}
	// The fixture inserts each profile's id as its display name, so the resolved
	// Config must carry the human-readable name alongside the internal label.
	for index, want := range []string{"profile-a", "profile-b"} {
		if results[index].cfg.ProviderName != want {
			t.Fatalf("resolved provider name = %q, want %q", results[index].cfg.ProviderName, want)
		}
		if !strings.HasPrefix(results[index].cfg.Provider, "cc-switch:grokbuild/") {
			t.Fatalf("resolved provider label lost its internal reference form: %q", results[index].cfg.Provider)
		}
	}
	for _, result := range results {
		content, err := os.ReadFile(result.cfg.Env["GROK_CONFIG_PATH"])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), "secret-") || !strings.Contains(string(content), `env_key = "PAIRROOM_CC_SWITCH_GROK_API_KEY"`) {
			t.Fatal("overlay contains a fixture credential or lacks environment indirection")
		}
		encoded, err := json.Marshal(result.cfg)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "secret-") {
			t.Fatal("agent.Config JSON leaked an environment credential")
		}
	}

	updated := profile("alpha", "model-a", "secret-a-new")
	if _, err := db.Exec(`UPDATE providers SET settings_config = ? WHERE id = 'profile-a' AND app_type = 'grokbuild'`, updated); err != nil {
		t.Fatal(err)
	}
	if results[0].cfg.Env["PAIRROOM_CC_SWITCH_GROK_API_KEY"] != "secret-a" {
		t.Fatal("an active materialization changed without re-resolution")
	}
	fresh, err := resolver.Resolve(context.Background(), model.ActorSlot1, selection("profile-a", "custom-a"), model.RuntimeGrok, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Env["PAIRROOM_CC_SWITCH_GROK_API_KEY"] != "secret-a-new" {
		t.Fatal("profile edit was not applied on the next resolution")
	}
}

func TestAgentCatalogWarnsOnlyForUnverifiedCCSwitchSchema(t *testing.T) {
	for _, test := range []struct {
		schema int
		warned bool
	}{{18, false}, {ccswitch.SupportedSchemaVersion, false}, {ccswitch.SupportedSchemaVersion + 1, true}} {
		database := filepath.Join(t.TempDir(), "cc-switch.db")
		db, err := sql.Open("sqlite", database)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE providers (
			id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
			settings_config TEXT NOT NULL, meta TEXT NOT NULL DEFAULT '{}', sort_index INTEGER,
			is_current BOOLEAN NOT NULL DEFAULT 0, in_failover_queue BOOLEAN NOT NULL DEFAULT 0,
			PRIMARY KEY (id, app_type)); PRAGMA user_version = ` + strconv.Itoa(test.schema) + `;`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		reader, err := ccswitch.NewReader(database)
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Defaults()
		resolver, err := NewAgentResolver(AgentResolverConfig{Defaults: cfg.DefaultSelections(), Runtimes: cfg.Runtimes, CCSwitch: reader, Mock: true})
		if err != nil {
			t.Fatal(err)
		}
		catalog := resolver.Catalog(context.Background())
		if catalog.ProviderError != nil {
			t.Fatalf("schema %d provider error = %#v", test.schema, catalog.ProviderError)
		}
		if got := catalog.ProviderWarning != nil; got != test.warned {
			t.Fatalf("schema %d provider warning = %#v", test.schema, catalog.ProviderWarning)
		}
		if test.warned && (catalog.ProviderWarning.Code != ccswitch.CodeSchemaUnverified || catalog.ProviderWarning.Params["actual"] != strconv.Itoa(test.schema)) {
			t.Fatalf("schema %d provider warning = %#v", test.schema, catalog.ProviderWarning)
		}
	}
}

func TestAgentResolverCCSwitchV4DefaultsAndSlotOverrides(t *testing.T) {
	database := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE providers (
		id TEXT NOT NULL, app_type TEXT NOT NULL, name TEXT NOT NULL,
		settings_config TEXT NOT NULL, meta TEXT NOT NULL DEFAULT '{}', sort_index INTEGER,
		is_current BOOLEAN NOT NULL DEFAULT 0, in_failover_queue BOOLEAN NOT NULL DEFAULT 0,
		PRIMARY KEY (id, app_type)); PRAGMA user_version = 20;`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alpha", "beta"} {
		settings, err := json.Marshal(map[string]any{
			"auth": map[string]string{},
			"config": "model_provider = 'custom'\nmodel = '" + id + "-model'\nmodel_reasoning_effort = 'high'\n" +
				"review_model = 'review-v4'\nmodel_context_window = 262144\n" +
				"[model_providers.custom]\nname = 'Direct'\nwire_api = 'responses'\n" +
				"base_url = 'https://" + id + ".invalid/v1'\nexperimental_bearer_token = 'fixture-key-" + id + "'\n",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO providers (id, app_type, name, settings_config) VALUES (?, 'codex', ?, ?)`, id, id, string(settings)); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := ccswitch.NewReader(database)
	if err != nil {
		t.Fatal(err)
	}
	defaults := config.Defaults()
	resolver, err := NewAgentResolver(AgentResolverConfig{Defaults: defaults.DefaultSelections(), Runtimes: defaults.Runtimes, CCSwitch: reader})
	if err != nil {
		t.Fatal(err)
	}
	selections := map[model.ActorID]model.AgentSelection{
		model.ActorSlot1: {Runtime: model.RuntimeCodex, Provider: model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "alpha"}},
		model.ActorSlot2: {Runtime: model.RuntimeCodex, Provider: model.ProviderRef{Source: model.ProviderCCSwitch, AppType: "codex", ProfileID: "beta"}, Model: "slot-model", Effort: "low"},
	}
	validated, err := resolver.ValidateSelections(context.Background(), selections)
	if err != nil {
		t.Fatal(err)
	}
	if validated[model.ActorSlot1].Model != "" || validated[model.ActorSlot1].Effort != "" {
		t.Fatal("resolved profile defaults were written into the immutable Room selection")
	}
	for _, test := range []struct {
		actor         model.ActorID
		id, wantModel string
		wantEffort    string
	}{
		{model.ActorSlot1, "alpha", "alpha-model", "high"},
		{model.ActorSlot2, "beta", "slot-model", "low"},
	} {
		cfg, err := resolver.Resolve(context.Background(), test.actor, validated[test.actor], model.RuntimeCodex, t.TempDir(), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Model != test.wantModel || cfg.Effort != test.wantEffort || cfg.Env["PAIRROOM_CC_SWITCH_CODEX_API_KEY"] != "fixture-key-"+test.id {
			t.Fatal("slot configuration did not retain its selected credential and model/effort precedence")
		}
		args := strings.Join(cfg.CommandArgs, " ")
		if !strings.Contains(args, "https://"+test.id+".invalid/v1") || !strings.Contains(args, `review_model="review-v4"`) || !strings.Contains(args, "model_context_window=262144") {
			t.Fatal("profile-owned endpoint and model options were not projected")
		}
		if strings.Contains(args, "fixture-key-") {
			t.Fatal("inline bearer credential escaped into argv")
		}
	}
	native, err := resolver.Resolve(context.Background(), model.ActorSlot1, model.AgentSelection{Runtime: model.RuntimeCodex, Provider: model.NativeProviderRef()}, model.RuntimeCodex, t.TempDir(), t.TempDir())
	if err != nil || native.Model != "" || native.Effort != "" || len(native.Env) != 0 {
		t.Fatal("native inheritance was changed by CC Switch defaults")
	}
}
