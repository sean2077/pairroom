package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sean2077/pairroom/internal/model"
)

func TestNativeBindOnlyReportsActualIgnoreChange(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		t.Run(map[bool]string{false: "add-entry", true: "existing-entry"}[preexisting], func(t *testing.T) {
			f := nativeHTTP(t)
			path := filepath.Join(f.project.Root, ".gitignore")
			if preexisting {
				if err := os.WriteFile(path, []byte("/.pairroom/\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.run(t, []string{"install", "--runtime", "claude"}, nil); err != nil {
				t.Fatal(err)
			}
			args := []string{"bind", "--room", f.room.ID, "--slot", "1", "--service-file", f.endpoint}
			bind := func() string {
				out, err := f.runAs(t, model.RuntimeClaude, "notice-fixture", args, nil)
				if err != nil {
					t.Fatal(err)
				}
				var result struct{ Notice string }
				if err := json.Unmarshal(out, &result); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(result.Notice, "Relay ready.") {
					t.Fatalf("missing readiness: %s", out)
				}
				return result.Notice
			}
			if added := strings.Contains(bind(), "Added .pairroom/ to .gitignore."); added == preexisting {
				t.Fatalf("changed notice=%v, existing=%v", added, preexisting)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(bind(), ".gitignore") {
				t.Fatal("idempotent bind claimed another gitignore change")
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(before) != string(after) {
				t.Fatal("repeat bind rewrote ignore contents")
			}
		})
	}
}
