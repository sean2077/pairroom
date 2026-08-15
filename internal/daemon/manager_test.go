package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func setTestUserConfigDir(t *testing.T, root string) {
	t.Helper()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", root)
	case "darwin":
		t.Setenv("HOME", root)
	default:
		t.Setenv("XDG_CONFIG_HOME", root)
	}
}

func TestResolveProducesStableAbsolutePathsAndCapturesProxyEnvironment(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, filepath.Join(root, "config"))
	t.Setenv("PATH", "test-path")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	binary := filepath.Join(root, "pairroom")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	if err := os.WriteFile(binary, []byte("test"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BinaryPath: binary, WorkDir: root, Args: []string{"service", "--mock"}}
	if err := Resolve(&cfg); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.BinaryPath, cfg.WorkDir, cfg.LogFile, cfg.ControlFile} {
		if !filepath.IsAbs(path) {
			t.Fatalf("path is not absolute: %q", path)
		}
	}
	if cfg.EnvPATH != "test-path" || cfg.EnvExtra["HTTPS_PROXY"] != "http://127.0.0.1:7890" {
		t.Fatalf("environment was not captured: %#v", cfg)
	}
}

func TestSortedEnvironmentDropsInvalidAndTemplateOwnedValues(t *testing.T) {
	values := SortedEnvironment(Config{
		EnvPATH: "expected-path",
		EnvExtra: map[string]string{
			"PATH":    "wrong-path",
			"VALID_1": "ok",
			"BAD KEY": "drop",
			"EMPTY":   "",
		},
	})
	got := make(map[string]string)
	for _, pair := range values {
		got[pair[0]] = pair[1]
	}
	if got["PATH"] != "expected-path" || got["VALID_1"] != "ok" || len(got) != 2 {
		t.Fatalf("unexpected environment: %#v", got)
	}
}

func TestMetadataDoesNotContainServiceArguments(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, root)
	meta := &Meta{
		LogFile: "service.log", ControlFile: "daemon.stop", DataRoot: "data",
		WorkDir: "work", BinaryPath: "pairroom", Platform: "test", InstalledAt: "now",
	}
	if err := SaveMeta(meta); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.DataRoot != "data" || loaded.Platform != "test" {
		t.Fatalf("unexpected metadata: %#v", loaded)
	}
	path, _ := MetaPath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token") || !json.Valid(data) {
		t.Fatalf("metadata leaked arguments or is invalid: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("metadata mode = %o, want 0600", info.Mode().Perm())
		}
	}
}

func TestRequestStopUsesInstalledControlFile(t *testing.T) {
	root := t.TempDir()
	setTestUserConfigDir(t, filepath.Join(root, "config"))
	control := filepath.Join(root, "custom", "stop.request")
	if err := SaveMeta(&Meta{ControlFile: control}); err != nil {
		t.Fatal(err)
	}
	if err := RequestStop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("control file was not created: %v", err)
	}
}
