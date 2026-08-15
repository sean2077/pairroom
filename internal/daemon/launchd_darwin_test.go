//go:build darwin

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBuildLaunchdPlistEscapesArgumentsAndPaths(t *testing.T) {
	cfg := Config{
		BinaryPath: "/Applications/Pair&Room/pairroom", WorkDir: "/tmp/a<b",
		LogFile: "/tmp/pair&room.log", Args: []string{"service", "--token", `a<b&c`},
		LogMaxSize: 10 * 1024 * 1024, LogBackups: 3, StopTimeout: 31 * time.Minute,
		EnvPATH: "/usr/bin:/bin",
	}
	plist := buildLaunchdPlist(cfg)
	for _, expected := range []string{
		`<string>/Applications/Pair&amp;Room/pairroom</string>`,
		`<string>service</string>`,
		`<string>a&lt;b&amp;c</string>`,
		`<string>/tmp/a&lt;b</string>`,
		`<string>/tmp/pair&amp;room.log</string>`,
		`<key>PAIRROOM_LOG_FILE</key>`,
		`<key>ExitTimeOut</key>`,
		`<integer>1860</integer>`,
	} {
		if !strings.Contains(plist, expected) {
			t.Fatalf("plist missing %q:\n%s", expected, plist)
		}
	}
}

func TestLaunchdUninstallPreservesPlistWhenStopFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := launchdPlistPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("<plist/>\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := runLaunchctl
	t.Cleanup(func() { runLaunchctl = original })
	runLaunchctl = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "print" {
			if args[1] == launchdGUIDomain() || args[1] == launchdTarget(launchdGUIDomain()) {
				return "state = running", nil
			}
		}
		if len(args) >= 1 && args[0] == "bootout" {
			return "permission denied", errors.New("failed")
		}
		return "", errors.New("not found")
	}
	if err := (&launchdManager{}).Uninstall(); err == nil {
		t.Fatal("Uninstall succeeded after launchd stop failure")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("plist was removed despite stop failure: %v", err)
	}
}
