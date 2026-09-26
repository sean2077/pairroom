package clilink

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// bundle creates <dir>/<name>.app with a host executable and a bundled CLI.
func bundle(t *testing.T, dir, name string) (executable, cli string) {
	t.Helper()
	contents := filepath.Join(dir, name+".app", "Contents")
	for _, sub := range []string{"MacOS", "Helpers"} {
		if err := os.MkdirAll(filepath.Join(contents, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	executable = filepath.Join(contents, "MacOS", "PairRoom")
	cli = filepath.Join(contents, "Helpers", "pairroom")
	for _, path := range []string{executable, cli} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return executable, cli
}

func requireSymlinks(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation commonly requires elevated Windows privileges; the feature is macOS-only")
	}
}

func TestBundledCLIRequiresAnAppBundleWithHelpers(t *testing.T) {
	dir := t.TempDir()
	executable, cli := bundle(t, dir, "PairRoom with spaces")
	if got := BundledCLI(executable); got != cli {
		t.Fatalf("BundledCLI = %q, want %q", got, cli)
	}
	loose := filepath.Join(dir, "bin", "PairRoom")
	if got := BundledCLI(loose); got != "" {
		t.Fatalf("a loose binary must not be treated as bundled: %q", got)
	}
	if err := os.Remove(cli); err != nil {
		t.Fatal(err)
	}
	if got := BundledCLI(executable); got != "" {
		t.Fatalf("a bundle without Helpers/pairroom must be unsupported: %q", got)
	}
}

func TestInspectClassifiesTheLink(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	_, cli := bundle(t, dir, "PairRoom")
	_, oldCLI := bundle(t, filepath.Join(dir, "old"), "PairRoom")
	link := filepath.Join(dir, "bin", "pairroom")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Inspect("", link); got != StateUnsupported {
		t.Fatalf("no bundled CLI: %s", got)
	}
	if got := Inspect(cli, link); got != StateMissing {
		t.Fatalf("missing: %s", got)
	}
	for _, tc := range []struct {
		name   string
		target string
		want   State
	}{
		{"this bundle", cli, StateInstalled},
		{"another bundle", oldCLI, StateStale},
		{"deleted bundle", filepath.Join(dir, "gone", "PairRoom.app", "Contents", "Helpers", "pairroom"), StateStale},
		{"unrelated tool", filepath.Join(dir, "elsewhere", "pairroom"), StateForeign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(link)
			if err := os.Symlink(tc.target, link); err != nil {
				t.Fatal(err)
			}
			if got := Inspect(cli, link); got != tc.want {
				t.Fatalf("Inspect = %s, want %s", got, tc.want)
			}
		})
	}
	_ = os.Remove(link)
	if err := os.WriteFile(link, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := Inspect(cli, link); got != StateForeign {
		t.Fatalf("a regular file must be foreign: %s", got)
	}
}

// stubOSAScript runs the privileged shell command directly, without a prompt.
func stubOSAScript(t *testing.T, fail func(script string) ([]byte, error)) *[]string {
	t.Helper()
	var calls []string
	before := runOSAScript
	runOSAScript = func(script string) ([]byte, error) {
		calls = append(calls, script)
		if fail != nil {
			return fail(script)
		}
		return nil, nil
	}
	t.Cleanup(func() { runOSAScript = before })
	return &calls
}

func TestInstallLinksMissingAndStaleButNeverForeign(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	_, cli := bundle(t, dir, "PairRoom")
	_, oldCLI := bundle(t, filepath.Join(dir, "old"), "PairRoom")
	link := filepath.Join(dir, "usr local", "bin", "pairroom")
	calls := stubOSAScript(t, func(string) ([]byte, error) {
		out, err := exec.Command("/bin/sh", "-c", Script(cli, link)).CombinedOutput()
		return out, err
	})

	if err := Install(cli, link); err != nil || Inspect(cli, link) != StateInstalled {
		t.Fatalf("install over missing: err=%v state=%s", err, Inspect(cli, link))
	}
	if err := Install(cli, link); err != nil || len(*calls) != 1 {
		t.Fatalf("an installed link must not prompt again: err=%v calls=%d", err, len(*calls))
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(oldCLI, link); err != nil {
		t.Fatal(err)
	}
	if err := Install(cli, link); err != nil || Inspect(cli, link) != StateInstalled {
		t.Fatalf("stale link must be repointed: err=%v state=%s", err, Inspect(cli, link))
	}

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("user tool"), 0o755); err != nil {
		t.Fatal(err)
	}
	prompts := len(*calls)
	if err := Install(cli, link); err == nil || len(*calls) != prompts {
		t.Fatalf("foreign entry must be refused without a prompt: err=%v", err)
	}
	if data, _ := os.ReadFile(link); string(data) != "user tool" {
		t.Fatal("a foreign file was modified")
	}
}

func TestInstallReportsCancelledPrompt(t *testing.T) {
	dir := t.TempDir()
	_, cli := bundle(t, dir, "PairRoom")
	stubOSAScript(t, func(string) ([]byte, error) {
		return []byte("execution error: User canceled. (-128)"), errors.New("exit status 1")
	})
	if err := Install(cli, filepath.Join(dir, "bin", "pairroom")); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel must be distinguishable: %v", err)
	}
}

func TestDeclinedOfferIsRememberedOutsideTheServiceRoot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "PairRoom Desktop")
	if Declined(dir) {
		t.Fatal("a fresh profile must be offered the link")
	}
	if err := RecordDeclined(dir); err != nil {
		t.Fatal(err)
	}
	if !Declined(dir) {
		t.Fatal("a declined offer must not repeat")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir())
	state, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(state) != "PairRoom Desktop" || filepath.Base(filepath.Dir(state)) == "pairroom" {
		t.Fatalf("preference directory must not be the Service data root: %s", state)
	}
}

func TestAppleScriptQuotesPathsSafely(t *testing.T) {
	cli := `/Applications/Pair "Room" 's.app/Contents/Helpers/pairroom`
	script := AppleScript(cli, "/usr/local/bin/pairroom")
	if !strings.HasPrefix(script, `do shell script "`) || !strings.HasSuffix(script, `" with administrator privileges`) {
		t.Fatalf("unexpected AppleScript: %s", script)
	}
	if !strings.Contains(script, `\"Room\"`) || !strings.Contains(script, `'\\''s.app`) {
		t.Fatalf("quotes were not escaped for both AppleScript and sh: %s", script)
	}
	if shell := Script(cli, "/usr/local/bin/pairroom"); !strings.Contains(shell, `'/Applications/Pair "Room" '\''s.app/Contents/Helpers/pairroom'`) {
		t.Fatalf("shell quoting: %s", shell)
	}
}

func TestScriptRechecksOwnershipBeforeReplacing(t *testing.T) {
	shell := Script("/Applications/PairRoom.app/Contents/Helpers/pairroom", "/usr/local/bin/pairroom")
	check := strings.Index(shell, "*.app/Contents/Helpers/pairroom)")
	link := strings.Index(shell, "/bin/ln -sfn")
	if check < 0 || link < 0 || check > link {
		t.Fatalf("the privileged script must recheck ownership before ln: %s", shell)
	}
	for _, want := range []string{"[ ! -L '/usr/local/bin/pairroom' ]", "/usr/bin/readlink '/usr/local/bin/pairroom'", "exit 3", foreignMarker} {
		if !strings.Contains(shell, want) {
			t.Fatalf("privileged script lacks %q: %s", want, shell)
		}
	}
}

func TestInstallRefusesAForeignEntryThatAppearsDuringThePrompt(t *testing.T) {
	requireSymlinks(t)
	dir := t.TempDir()
	_, cli := bundle(t, dir, "PairRoom")
	link := filepath.Join(dir, "bin", "pairroom")
	for name, appear := range map[string]func() error{
		"file": func() error { return os.WriteFile(link, []byte("user tool"), 0o755) },
		"link": func() error { return os.Symlink(filepath.Join(dir, "elsewhere", "pairroom"), link) },
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(link)
			stubOSAScript(t, func(string) ([]byte, error) {
				// The entry appears after Install's unprivileged check, while the
				// administrator prompt is open.
				if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := appear(); err != nil {
					t.Fatal(err)
				}
				out, err := exec.Command("/bin/sh", "-c", Script(cli, link)).CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != foreignExitCode {
					t.Fatalf("privileged script exit = %v, want %d: %s", err, foreignExitCode, out)
				}
				return out, err
			})
			err := Install(cli, link)
			if err == nil || !strings.Contains(err.Error(), "appeared while waiting for authorization") {
				t.Fatalf("Install = %v, want a foreign-entry refusal", err)
			}
			if Inspect(cli, link) != StateForeign {
				t.Fatalf("foreign entry was replaced: state=%s", Inspect(cli, link))
			}
		})
	}
}
