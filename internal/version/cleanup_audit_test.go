package version

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Temporary refactoring inventory; removed when the compatibility cleanup is complete.
func TestCleanupInventory(t *testing.T) {
	root := filepath.Join("..", "..")
	patterns := []string{"importedDirs", "CompleteBindings", "EventRoomBindingsCompleted", "EventLegacyImported", "roomBindingsCompletedPayload", "legacyImportedPayload", "serveCommand", "runServe", "\"serve\"", "pairroom serve", "recover-stale-lock", "Workspaces", "SwapRoles", "TargetRole", "Schema: 1", "Schema != 1", "Schema == 1"}
	for _, dir := range []string{"internal", "cmd", "desktop", "scripts", "docs"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "node_modules" || entry.Name() == "dist" {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Name() == "cleanup_audit_test.go" || strings.HasSuffix(entry.Name(), ".min.js") {
				return nil
			}
			switch filepath.Ext(path) {
			case ".go", ".js", ".html", ".py", ".md", ".sh":
			default:
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			for index, line := range strings.Split(string(data), "\n") {
				for _, pattern := range patterns {
					if strings.Contains(strings.ToLower(line), strings.ToLower(pattern)) {
						t.Logf("%s:%d: %s", filepath.ToSlash(rel), index+1, strings.TrimSpace(line))
						break
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("temporary compatibility inventory: review these references before removing this test")
}
