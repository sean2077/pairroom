package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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
			Model:    modelName, OrdinaryReviewerPolicy: model.ReviewerEnforced,
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
		{model.ActorClaude, "profile-a", "custom-a"},
		{model.ActorCodex, "profile-b", "custom-b"},
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
	fresh, err := resolver.Resolve(context.Background(), model.ActorClaude, selection("profile-a", "custom-a"), model.RuntimeGrok, t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Env["PAIRROOM_CC_SWITCH_GROK_API_KEY"] != "secret-a-new" {
		t.Fatal("profile edit was not applied on the next resolution")
	}
}
