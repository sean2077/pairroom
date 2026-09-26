package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type doctorJSON struct {
	Repo string `json:"repository"`
	Git  struct {
		Available bool   `json:"available"`
		Output    string `json:"output"`
		Error     string `json:"error"`
	} `json:"git"`
	Runtimes map[string]struct {
		Checks []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
		Probe *struct {
			Runtime  string `json:"runtime"`
			Version  string `json:"version"`
			Protocol string `json:"protocol"`
		} `json:"probe"`
		Error string `json:"error"`
	} `json:"runtimes"`
	RetiredRelaySlotDirectories int  `json:"retired_relay_slot_directories"`
	OK                          bool `json:"ok"`
}

// fakeDoctorTools puts a fake git on PATH and returns fake runtime commands.
func fakeDoctorTools(t *testing.T) (claude, codex, grok string) {
	t.Helper()
	isolateUserDirs(t)
	bin := t.TempDir()
	fakeExecutable(t, bin, "git")
	t.Setenv("PATH", bin)
	return fakeExecutable(t, bin, "claude"), fakeExecutable(t, bin, "codex"), fakeExecutable(t, bin, "grok")
}

func runDoctorJSON(t *testing.T, args ...string) (doctorJSON, error) {
	t.Helper()
	output, err := captureRun(t, append([]string{"doctor", "--json"}, args...)...)
	var report doctorJSON
	if decodeErr := json.Unmarshal([]byte(output), &report); decodeErr != nil {
		t.Fatalf("doctor --json is not JSON: %v\n%s", decodeErr, output)
	}
	return report, err
}

func TestDoctorPassesWhenGitAndBothRuntimesProbe(t *testing.T) {
	claude, codex, grok := fakeDoctorTools(t)
	repo := t.TempDir()
	report, err := runDoctorJSON(t, "--repo", repo, "--claude-command", claude, "--codex-command", codex, "--grok-command", grok)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.Repo != repo || !report.Git.Available || report.Git.Output != "git version 2.99.0.fake" {
		t.Fatalf("doctor report = %+v", report)
	}
	for actor, want := range map[string]string{"slot1": "claude", "slot2": "codex"} {
		entry := report.Runtimes[actor]
		if entry.Error != "" || entry.Probe == nil || entry.Probe.Runtime != want || entry.Probe.Version != "2.1.300" || len(entry.Checks) != 0 {
			t.Fatalf("%s runtime = %+v", actor, entry)
		}
	}

	output, err := captureRun(t, "doctor", "--repo", repo, "--claude-command", claude, "--codex-command", codex)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PairRoom doctor ",
		"Repository       " + repo + "\n",
		"Git              ✓ git version 2.99.0.fake (",
		"Claude Code      ✓ 2.1.300 (" + claude + ")\n",
		"Codex            ✓ 2.1.300 (" + codex + ")\n",
		"protocol: claude-stream-json\n",
		"protocol: codex-app-server-jsonrpc\n",
		"model response: not checked (use doctor --live; may consume quota)\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("doctor text missing %q:\n%s", want, output)
		}
	}
	if strings.Contains(output, "Retired relay dirs") {
		t.Fatalf("doctor reported retired relay directories for a clean repository:\n%s", output)
	}
}

func TestDoctorFailsAndExplainsMissingGitAndRuntime(t *testing.T) {
	claude, _, _ := fakeDoctorTools(t)
	t.Setenv("PATH", t.TempDir())
	repo := t.TempDir()
	missing := filepath.Join(t.TempDir(), "codex-not-installed")
	report, err := runDoctorJSON(t, "--repo", repo, "--claude-command", claude, "--codex-command", missing)
	requireErrorContains(t, err, "runtime checks failed; pairroom serve --mock remains available")
	if report.OK || report.Git.Available || report.Git.Error == "" {
		t.Fatalf("doctor report = %+v", report)
	}
	if entry := report.Runtimes["slot2"]; entry.Probe != nil || !strings.Contains(entry.Error, "locate Codex runtime") {
		t.Fatalf("missing runtime entry = %+v", entry)
	}
	if entry := report.Runtimes["slot1"]; entry.Probe == nil || entry.Error != "" {
		t.Fatalf("available runtime entry = %+v", entry)
	}

	output, err := captureRun(t, "doctor", "--repo", repo, "--claude-command", claude, "--codex-command", missing)
	requireErrorContains(t, err, "runtime checks failed")
	for _, want := range []string{"Git              ✗ ", "Agent 2          ✗ locate Codex runtime"} {
		if !strings.Contains(output, want) {
			t.Fatalf("doctor text missing %q:\n%s", want, output)
		}
	}
}

// Duplicate Runtimes use the documented slot-order suffixes (PROTOCOL.md).
func TestDoctorLabelsDuplicateRuntimesAndReportsRetiredRelayDirectories(t *testing.T) {
	claude, codex, grok := fakeDoctorTools(t)
	repo := t.TempDir()
	for _, slot := range []string{"claude", "codex"} {
		if err := os.MkdirAll(filepath.Join(repo, ".pairroom", "rooms", "legacy", "slots", slot), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(t.TempDir(), "pairroom.json")
	writeTestFile(t, configPath, `{"claude":{"runtime":"grok"},"codex":{"runtime":"grok"}}`)
	output, err := captureRun(t, "doctor", "-config", configPath, "--repo", repo, "--claude-command", claude, "--codex-command", codex, "--grok-command", grok)
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, output)
	}
	for _, want := range []string{
		"Retired relay dirs 2 ignored (re-bind canonical slots; legacy credentials were not read)\n",
		"Grok Build 0     ✓ 2.1.300 (" + grok + ")\n",
		"Grok Build 1     ✓ 2.1.300 (" + grok + ")\n",
		"protocol: grok-acp-v1\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("doctor text missing %q:\n%s", want, output)
		}
	}
}

func TestDoctorRejectsInvalidRepositoryAndConfiguration(t *testing.T) {
	requireErrorContains(t, run([]string{"doctor", "--repo", filepath.Join(t.TempDir(), "missing")}), "stat repository")
	requireErrorContains(t, run([]string{"doctor", "--config", filepath.Join(t.TempDir(), "missing.json")}), "read config")
}

func TestDoctorLiveReportsFailedModelChecksAsFailure(t *testing.T) {
	claude, codex, grok := fakeDoctorTools(t)
	// The fake runtimes answer only version/help probes, so an explicit live
	// check must start, fail, and turn the whole report red.
	report, err := runDoctorJSON(t, "--live", "--repo", t.TempDir(), "--claude-command", claude, "--codex-command", codex, "--grok-command", grok)
	requireErrorContains(t, err, "runtime checks failed")
	if report.OK {
		t.Fatalf("live doctor passed with non-responsive runtimes: %+v", report)
	}
	for _, actor := range []string{"slot1", "slot2"} {
		entry := report.Runtimes[actor]
		if entry.Probe == nil || len(entry.Checks) == 0 {
			t.Fatalf("%s live entry = %+v", actor, entry)
		}
		passed := true
		for _, check := range entry.Checks {
			passed = passed && check.Status == "pass"
		}
		if passed {
			t.Fatalf("%s live checks passed against a fake runtime: %+v", actor, entry.Checks)
		}
	}
}
