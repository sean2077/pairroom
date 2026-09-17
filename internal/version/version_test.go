package version

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestVersionFileMatchesBinaryVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != Current {
		t.Fatalf("VERSION=%q Current=%q", got, Current)
	}
	info := BuildInfo()
	if info.Version != Current || info.StoreSchema != StoreSchema || info.RepositoryURL != RepositoryURL || info.Commit == "" || info.BuildDate == "" || info.LastTag == "" || info.CommitsSinceTag == "" {
		t.Fatalf("invalid build info: %#v", info)
	}
	if RepositoryURL != "https://github.com/sean2077/pairroom" {
		t.Fatalf("unexpected repository URL: %q", RepositoryURL)
	}
}

// TestDesktopBuildConfigMatchesBinaryVersion keeps the Desktop (Wails) build
// version in sync with the release version; prepare-build.py enforces the same
// equality at package time, and drift there fails the desktop release workflow
// only after a tag is already immutable.
func TestDesktopBuildConfigMatchesBinaryVersion(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "desktop", "build", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`(?m)^\s*version:\s*"([^"]+)"\s*$`).FindStringSubmatch(string(data))
	if match == nil {
		t.Fatal(`desktop/build/config.yml has no double-quoted version field`)
	}
	if match[1] != Current {
		t.Fatalf("desktop build config version %q does not match Current %q", match[1], Current)
	}
}
