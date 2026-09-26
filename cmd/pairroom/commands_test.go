package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/ccswitch"
	"github.com/sean2077/pairroom/internal/daemon"
	"github.com/sean2077/pairroom/internal/version"
	_ "modernc.org/sqlite"
)

func TestRunWithoutArgumentsPrintsTopLevelHelp(t *testing.T) {
	output, err := captureRun(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"daemon", "service", "serve", "doctor", "providers", "verify", "backup", "restore", "diagnostics", "relay", "protocol", "version"} {
		if !strings.Contains(output, "pairroom "+command+" ") {
			t.Errorf("top-level help omits %q:\n%s", command, output)
		}
	}
}

func TestUnknownCommandNamesItAndPointsToHelp(t *testing.T) {
	err := run([]string{"frobnicate"})
	requireErrorContains(t, err, `unknown command "frobnicate"`)
	requireErrorContains(t, err, "pairroom help")
}

func TestVersionAliasesPrintOneSummaryLine(t *testing.T) {
	for _, alias := range []string{"version", "--version", "-v"} {
		output, err := captureRun(t, alias)
		if err != nil {
			t.Fatalf("%s: %v", alias, err)
		}
		if output != versionSummary()+"\n" {
			t.Fatalf("%s printed %q", alias, output)
		}
	}
}

func TestVersionJSONReportsBuildMetadata(t *testing.T) {
	output, err := captureRun(t, "version", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var info map[string]any
	if err := json.Unmarshal([]byte(output), &info); err != nil {
		t.Fatalf("version --json is not JSON: %v\n%s", err, output)
	}
	if info["version"] != version.Current || info["store_schema"] != float64(version.StoreSchema) {
		t.Fatalf("version --json = %v", info)
	}
}

// Every simple subcommand shares one argument contract: unknown flags and
// positional arguments are rejected before any state is resolved or opened.
func TestSubcommandsRejectStrayArgumentsBeforeTouchingState(t *testing.T) {
	isolateUserDirs(t)
	missing := filepath.Join(t.TempDir(), "never-created")
	commands := map[string][]string{
		"version":     nil,
		"providers":   nil,
		"verify":      {"--data-dir", missing},
		"backup":      {"--data-dir", missing, "--output", filepath.Join(missing, "out.tar.gz")},
		"restore":     {"--data-dir", missing, "--input", filepath.Join(missing, "in.tar.gz")},
		"diagnostics": {"--data-dir", missing, "--output", filepath.Join(missing, "out.tar.gz")},
		"doctor":      {"--repo", missing},
		"service":     {"--data-root", missing, "--no-browser"},
		"serve":       {"--repo", missing, "--no-browser"},
	}
	for command, base := range commands {
		t.Run(command, func(t *testing.T) {
			args := append(append([]string{command}, base...), "stray")
			requireErrorContains(t, run(args), "unexpected arguments: stray")
			args = append(append([]string{command}, base...), "--no-such-flag")
			requireErrorContains(t, run(args), "no-such-flag")
			assertNotExist(t, missing)
		})
	}
}

func TestArchiveCommandsRequireTheirPathFlag(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "never-created")
	for _, tc := range []struct{ command, flag string }{{"backup", "--output"}, {"diagnostics", "--output"}, {"restore", "--input"}} {
		for _, value := range []string{"", "   "} {
			err := run([]string{tc.command, "--data-dir", missing, tc.flag + "=" + value})
			requireErrorContains(t, err, tc.flag+" is required")
		}
	}
	assertNotExist(t, missing)
}

func TestServiceValidatesOptionsBeforeClaimingTheDataRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "service-root")
	absoluteControl := filepath.Join(t.TempDir(), "daemon.stop")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--runtime-limit=0"}, "runtime-limit must be between 1 and 128"},
		{[]string{"--runtime-limit=129"}, "runtime-limit must be between 1 and 128"},
		{[]string{"--idle-timeout=-1s"}, "idle-timeout must be greater than zero"},
		{[]string{"--shutdown-timeout=0s"}, "shutdown-timeout must be greater than zero"},
		{[]string{"--daemon-control-file=relative.stop"}, "daemon-control-file must be absolute"},
		{[]string{"--stall-warning-seconds=29"}, "stall-warning-seconds must be -1 or between 30 and 86400"},
		{[]string{"--stall-warning-seconds=86401"}, "stall-warning-seconds must be -1 or between 30 and 86400"},
		{[]string{"--stall-warning-seconds=0"}, "stall-warning-seconds must be -1 or between 30 and 86400"},
		{[]string{"--listen=127.0.0.1"}, "numeric loopback"},
	} {
		args := append([]string{"service", "--data-root", root, "--no-browser", "--daemon-control-file", absoluteControl}, tc.args...)
		requireErrorContains(t, run(args), tc.want)
	}
	assertNotExist(t, root)
}

func TestServiceRejectsInvalidConfigurationBeforeOpeningState(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "service-root")
	requireErrorContains(t, run([]string{"service", "--config", filepath.Join(dir, "missing.json"), "--data-root", root}), "read config")
	badJSON := filepath.Join(dir, "bad.json")
	writeTestFile(t, badJSON, "[1]")
	requireErrorContains(t, run([]string{"service", "--config", badJSON, "--data-root", root}), "decode config")
	assertNotExist(t, root)
}

func TestServiceRejectsRelativeDataRoot(t *testing.T) {
	requireErrorContains(t, run([]string{"service", "--data-root", "relative-root", "--no-browser"}), "service data root must be absolute")
	assertNotExist(t, "relative-root")
}

func TestServiceRefusesAnOccupiedDataRoot(t *testing.T) {
	root := t.TempDir()
	lock, err := json.Marshal(map[string]any{"pid": os.Getpid(), "started_at": "2026-01-02T03:04:05Z", "nonce": "held-by-test"})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "service.lock")
	writeTestFile(t, lockPath, string(lock)+"\n")
	err = run([]string{"service", "--data-root", root, "--no-browser", "--mock"})
	requireErrorContains(t, err, "another PairRoom Service owns this data root")
	data, readErr := os.ReadFile(lockPath)
	if readErr != nil || !strings.Contains(string(data), "held-by-test") {
		t.Fatalf("occupied lock was replaced: %q %v", data, readErr)
	}
	assertNotExist(t, filepath.Join(root, "rooms"))
}

func TestVerifyReportsInvalidRoomDataAsFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-room")
	output, err := captureRun(t, "verify", "--data-dir", missing)
	requireErrorContains(t, err, "room data verification failed")
	for _, want := range []string{"PairRoom data verification\n", "  data:        " + missing + "\n", "  error:       stat data directory", "  result:      FAILED\n"} {
		if !strings.Contains(output, want) {
			t.Fatalf("verify output missing %q:\n%s", want, output)
		}
	}
	output, err = captureRun(t, "verify", "--data-dir", missing, "--json")
	requireErrorContains(t, err, "room data verification failed")
	var report struct {
		DataDir string   `json:"data_dir"`
		OK      bool     `json:"ok"`
		Errors  []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("verify --json is not JSON: %v\n%s", err, output)
	}
	if report.OK || report.DataDir != missing || len(report.Errors) == 0 {
		t.Fatalf("verify --json = %+v", report)
	}
	assertNotExist(t, missing)
}

func TestBackupOfMissingRoomFailsWithoutWritingOutput(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "backup.tar.gz")
	requireErrorContains(t, run([]string{"backup", "--data-dir", filepath.Join(dir, "missing-room"), "--output", output}), "data verification failed")
	assertNotExist(t, output)
}

func TestRestoreOfMissingArchiveFailsWithoutCreatingRoom(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "restored")
	requireErrorContains(t, run([]string{"restore", "--data-dir", target, "--input", filepath.Join(dir, "missing.tar.gz")}), "open backup")
	assertNotExist(t, target)
}

func TestDiagnosticsRejectsOutputInsideTheRoomDataDirectory(t *testing.T) {
	dataDir := t.TempDir()
	output := filepath.Join(dataDir, "diag.tar.gz")
	requireErrorContains(t, run([]string{"diagnostics", "--data-dir", dataDir, "--output", output}), "outside the source Room data directory")
	assertNotExist(t, output)
}

func TestDiagnosticsForMissingRoomStillWritesABundle(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "out", "diag.tar.gz")
	stdout, err := captureRun(t, "diagnostics", "--data-dir", filepath.Join(dir, "missing-room"), "--output", output)
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "PairRoom diagnostics created\n  output: "+output+"\n" {
		t.Fatalf("diagnostics output = %q", stdout)
	}
	if info, err := os.Stat(output); err != nil || info.Size() == 0 {
		t.Fatalf("diagnostics bundle missing: %v", err)
	}
}

func TestDataDirDefaultsToAPerRepositoryUserConfigDirectory(t *testing.T) {
	base := isolateUserDirs(t)
	repo := filepath.Join(t.TempDir(), "My Repo!")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := resolveDataDir(repo, "")
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(base, "pairroom", "rooms", "my-repo") + "-"
	suffix := strings.TrimPrefix(got, prefix)
	if suffix == got || len(suffix) != 12 || strings.Trim(suffix, "0123456789abcdef") != "" {
		t.Fatalf("default data dir = %q, want %q + 12 lowercase hex digits", got, prefix)
	}
	again, err := resolveDataDir(repo+string(filepath.Separator)+".", "")
	if err != nil || again != got {
		t.Fatalf("equivalent repository spelling resolved to %q (%v), want %q", again, err, got)
	}
	other := filepath.Join(t.TempDir(), "My Repo!")
	if err := os.Mkdir(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if different, err := resolveDataDir(other, ""); err != nil || different == got {
		t.Fatalf("same-named repositories share a data dir: %q %v", different, err)
	}
	assertNotExist(t, got)
}

func TestDataDirOverrideIsAbsolutizedWithoutRepositoryCheck(t *testing.T) {
	override := filepath.Join("relative", "room", "..")
	got, err := resolveDataDir(filepath.Join(t.TempDir(), "missing-repo"), override)
	if err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(cwd, "relative") {
		t.Fatalf("data dir = %q", got)
	}
}

func TestDefaultDataDirRejectsMissingOrFileRepository(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveDataDir(filepath.Join(dir, "missing"), "")
	requireErrorContains(t, err, "stat repository")
	file := filepath.Join(dir, "file")
	writeTestFile(t, file, "x")
	_, err = resolveDataDir(file, "")
	requireErrorContains(t, err, "repository is not a directory")
}

func TestSanitizeRoomDirectoryName(t *testing.T) {
	for input, want := range map[string]string{
		"PairRoom": "pairroom",
		"My Repo!": "my-repo",
		"feat_x-1": "feat_x-1",
		"--dash--": "dash",
		"日本":       "room",
		"":         "room",
		"a.b":      "a-b",
		"Ünï 2":    "n--2",
	} {
		if got := sanitize(input); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestFirstLine(t *testing.T) {
	for input, want := range map[string]string{"": "ok", "one": "one", "one\ntwo": "one", "\nsecond": ""} {
		if got := firstLine(input); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestConfigureProcessLoggingRedirectsNonRelayCommands(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "service.log")
	t.Setenv(daemon.LogFileEnvironment, logPath)
	t.Setenv(daemon.ConsoleDetachEnvironment, "")
	stdout := os.Stdout
	cleanup, err := configureProcessLogging([]string{"version"})
	if err != nil {
		t.Fatal(err)
	}
	if os.Stdout == stdout {
		_ = cleanup()
		t.Fatal("non-relay command kept the original stdout despite PAIRROOM_LOG_FILE")
	}
	runErr := run([]string{"version"})
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if os.Stdout != stdout {
		t.Fatal("cleanup did not restore stdout")
	}
	data, err := os.ReadFile(logPath)
	if err != nil || string(data) != versionSummary()+"\n" {
		t.Fatalf("log file = %q, %v", data, err)
	}
}

func TestConfigureProcessLoggingWithoutEnvironmentIsANoOp(t *testing.T) {
	t.Setenv(daemon.LogFileEnvironment, "")
	stdout := os.Stdout
	cleanup, err := configureProcessLogging([]string{"service"})
	if err != nil {
		t.Fatal(err)
	}
	if os.Stdout != stdout {
		t.Fatal("stdout redirected without PAIRROOM_LOG_FILE")
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureProcessLoggingRejectsInvalidRotationSettings(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "service.log")
	t.Setenv(daemon.LogFileEnvironment, logPath)
	t.Setenv(daemon.LogSizeEnvironment, "ten")
	t.Setenv(daemon.ConsoleDetachEnvironment, "")
	stdout := os.Stdout
	if _, err := configureProcessLogging([]string{"service"}); err == nil {
		t.Fatal("invalid log size was accepted")
	}
	if os.Stdout != stdout {
		t.Fatal("stdout redirected despite an invalid logging configuration")
	}
	assertNotExist(t, logPath)
}

func TestRelayDispatchReachesTheRelayClient(t *testing.T) {
	requireErrorContains(t, run([]string{"relay"}), "use pairroom relay")
}

func writeCCSwitchFixture(t *testing.T, schema int, rows ...[5]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cc-switch.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
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
	for _, row := range rows {
		if _, err := db.Exec(`INSERT INTO providers (id, app_type, name, settings_config, meta) VALUES (?, ?, ?, ?, ?)`, row[0], row[1], row[2], row[3], row[4]); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestProvidersPrintsSanitizedCatalogWithoutSecrets(t *testing.T) {
	const secret = "cli-fixture-secret"
	database := writeCCSwitchFixture(t, ccswitch.SupportedSchemaVersion,
		[5]string{"c", "claude", "Claude direct", `{"env":{"ANTHROPIC_AUTH_TOKEN":"` + secret + `","ANTHROPIC_MODEL":"claude-test"}}`, `{"apiFormat":"anthropic"}`},
		[5]string{"oauth", "codex", "Managed OAuth", `{"auth":{"auth_mode":"chatgpt","tokens":{"access_token":"` + secret + `"}}}`, `{}`},
	)
	output, err := captureRun(t, "providers", "--database", database)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, secret) {
		t.Fatalf("providers leaked a credential:\n%s", output)
	}
	header := "PairRoom CC Switch profiles (" + ccswitch.SupportedCCSwitchVersion + ", schema " + strconv.Itoa(ccswitch.SupportedSchemaVersion) + ")"
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 3 || lines[0] != header {
		t.Fatalf("providers output:\n%s", output)
	}
	if !strings.Contains(lines[1], "Claude direct") || !strings.Contains(lines[1], "models=claude-test") || !strings.HasSuffix(lines[1], " selectable") {
		t.Fatalf("supported profile line = %q", lines[1])
	}
	if !strings.Contains(lines[2], "Managed OAuth") || !strings.Contains(lines[2], " disabled: ") {
		t.Fatalf("disabled profile line = %q", lines[2])
	}

	output, err = captureRun(t, "providers", "--database", database, "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, secret) {
		t.Fatalf("providers --json leaked a credential:\n%s", output)
	}
	var catalog ccswitch.Catalog
	if err := json.Unmarshal([]byte(output), &catalog); err != nil {
		t.Fatalf("providers --json is not a catalog: %v\n%s", err, output)
	}
	if catalog.Schema != ccswitch.SupportedSchemaVersion || len(catalog.Profiles) != 2 {
		t.Fatalf("providers --json = %+v", catalog)
	}
}

func TestProvidersUsesConfiguredDatabaseAndReportsEmptyCatalog(t *testing.T) {
	database := writeCCSwitchFixture(t, ccswitch.SupportedSchemaVersion)
	configPath := filepath.Join(t.TempDir(), "pairroom.json")
	encoded, err := json.Marshal(map[string]any{"cc_switch": map[string]string{"database": database}})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, configPath, string(encoded))
	output, err := captureRun(t, "providers", "-config", configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(output, ")\n  no profiles found\n") {
		t.Fatalf("empty providers output = %q", output)
	}
}

func TestProvidersFailsClosedOnMissingRelativeOrUnsupportedDatabase(t *testing.T) {
	var ccErr *ccswitch.Error
	err := run([]string{"providers", "--database", filepath.Join(t.TempDir(), "missing.db")})
	if !errors.As(err, &ccErr) || ccErr.Code != ccswitch.CodeDatabaseMissing {
		t.Fatalf("missing database error = %v", err)
	}
	requireErrorContains(t, run([]string{"providers", "--database", "relative.db"}), "absolute path")
	err = run([]string{"providers", "--database", writeCCSwitchFixture(t, ccswitch.SupportedSchemaVersion+1)})
	if !errors.As(err, &ccErr) || ccErr.Code != ccswitch.CodeSchemaMismatch {
		t.Fatalf("unsupported schema error = %v", err)
	}
	requireErrorContains(t, run([]string{"providers", "--config", filepath.Join(t.TempDir(), "missing.json")}), "read config")
}
