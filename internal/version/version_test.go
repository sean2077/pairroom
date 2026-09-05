package version

import (
	"os"
	"path/filepath"
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
