package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/daemon"
)

// The pre-parse must select the same --config file that the flag package
// later accepts, or flag defaults silently come from a different file.
func TestFlagValueAgreesWithFlagPackage(t *testing.T) {
	for _, args := range [][]string{
		{"-config", "single.json"},
		{"-config=single-equals.json"},
		{"--config", "double.json"},
		{"--config=double-equals.json"},
		{"--config=first.json", "--config", "last.json"},
		{"-config", "first.json", "--config=last.json"},
		{"--mock", "--config", "after-bool.json"},
		{"--listen", "127.0.0.1:1", "-config=contains=equals.json"},
	} {
		flags := flag.NewFlagSet("oracle", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		want := flags.String("config", "", "")
		flags.Bool("mock", false, "")
		flags.String("listen", "", "")
		if err := flags.Parse(args); err != nil {
			t.Fatalf("oracle rejected %q: %v", args, err)
		}
		if got := flagValue(args, "--config"); got != *want {
			t.Errorf("flagValue(%q) = %q, flag package selects %q", args, got, *want)
		}
	}
}

func TestServiceHonorsSingleDashConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.json")
	if err := os.WriteFile(path, []byte(`{"listen":"0.0.0.0:7332"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"service", "-config", path, "--runtime-limit=0"},
		{"service", "-config=" + path, "--runtime-limit=0"},
		{"serve", "-config", path, "--repo=missing-repository"},
	} {
		err := run(args)
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("run(%q) ignored the configured non-loopback listener: %v", args, err)
		}
	}
}

func TestNormalizeDaemonServiceArgsGuardsSingleDashForms(t *testing.T) {
	for _, argument := range []string{"-recover-stale-lock", "-recover-stale-lock=true", "-daemon-control-file=/tmp/x"} {
		cfg := daemon.Config{WorkDir: t.TempDir(), ControlFile: filepath.Join(t.TempDir(), "daemon.stop"), Args: []string{"service", argument}}
		if err := normalizeDaemonServiceArgs(&cfg); err == nil {
			t.Fatalf("single-dash %q was persisted: %#v", argument, cfg.Args)
		}
	}
	root := t.TempDir()
	cfg := daemon.Config{WorkDir: root, ControlFile: filepath.Join(root, "daemon.stop"), Args: []string{"service", "-data-root", "state", "-shutdown-timeout=2m"}}
	if err := normalizeDaemonServiceArgs(&cfg); err != nil {
		t.Fatal(err)
	}
	if got := flagValue(cfg.Args, "--data-root"); got != filepath.Join(root, "state") {
		t.Fatalf("single-dash data root = %q, want absolute under the work dir", got)
	}
	if cfg.StopTimeout != 3*60*1e9 {
		t.Fatalf("stop timeout = %s, want shutdown-timeout plus one minute", cfg.StopTimeout)
	}
}
