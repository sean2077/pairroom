//go:build ignore

package main

import (
	"strings"
	"testing"
)

func TestVerifyModules(t *testing.T) {
	const root = `{"Path":"root","Main":true}`
	const dependency = `{"Path":"dep","Version":"v1.0.0"}`
	approved := map[string]string{"root": "", "dep": "v1.0.0"}
	for _, test := range []struct{ name, graph, problem string }{
		{"approved", root + dependency, ""},
		{"reordered", dependency + root, ""},
		{"version drift", root + `{"Path":"dep","Version":"v1.1.0"}`, "resolved to"},
		{"local replacement", root + `{"Path":"dep","Version":"v1.0.0","Replace":{"Path":"../unreviewed"}}`, "replacement"},
		{"remote replacement", root + `{"Path":"dep","Version":"v1.0.0","Replace":{"Path":"fork","Version":"v1.0.0"}}`, "replacement"},
		{"missing", root, "missing"},
		{"unapproved", root + dependency + `{"Path":"extra"}`, "unapproved module"},
		{"duplicate", root + dependency + dependency, "duplicate"},
		{"truncated", root + `{"Path":`, "decode"},
		{"trailing junk", root + dependency + `trailing`, "decode"},
		{"null", root + `null`, "unapproved module"},
	} {
		t.Run(test.name, func(t *testing.T) {
			violations := verifyModules(strings.NewReader(test.graph), approved)
			if test.problem == "" {
				if len(violations) != 0 {
					t.Fatal(violations)
				}
				return
			}
			if !strings.Contains(strings.Join(violations, "\n"), test.problem) {
				t.Fatalf("wanted %q: %v", test.problem, violations)
			}
		})
	}
}

func TestPlanVersionSyncAcceptsOnlyVersionChanges(t *testing.T) {
	const root = `{"Path":"root","Main":true}`
	approved := map[string]string{"root": "", "dep": "v1.0.0", "same": "v2.0.0"}
	changes, err := planVersionSync([]byte(root+`{"Path":"dep","Version":"v1.1.0"}{"Path":"same","Version":"v2.0.0"}`), approved)
	if err != nil || len(changes) != 1 || changes[0] != (versionChange{"dep", "v1.0.0", "v1.1.0"}) {
		t.Fatalf("changes = %v, err = %v", changes, err)
	}
	for _, test := range []struct{ name, graph, problem string }{
		{"added module", root + `{"Path":"dep","Version":"v1.0.0"}{"Path":"same","Version":"v2.0.0"}{"Path":"extra","Version":"v1.0.0"}`, "unapproved module"},
		{"removed module", root + `{"Path":"dep","Version":"v1.0.0"}`, "missing"},
		{"replacement with drift", root + `{"Path":"dep","Version":"v1.1.0","Replace":{"Path":"fork","Version":"v1.1.0"}}{"Path":"same","Version":"v2.0.0"}`, "replacement"},
		{"truncated", root + `{"Path":`, "decode"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := planVersionSync([]byte(test.graph), approved); err == nil || !strings.Contains(err.Error(), test.problem) {
				t.Fatalf("wanted %q, got %v", test.problem, err)
			}
		})
	}
}

func TestRewriteAllowlistChangesOnlyTheNamedEntry(t *testing.T) {
	source := "package main\n\nvar allowed = map[string]string{\n\t\"a/sqlite\": \"v1.0.0\",\n\t\"a/sqlite/x\": \"v1.0.0\",\n}\n"
	got, err := rewriteAllowlist(source, []versionChange{{"a/sqlite", "v1.0.0", "v1.1.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "\"a/sqlite\":   \"v1.1.0\"") || !strings.Contains(got, "\"a/sqlite/x\": \"v1.0.0\"") {
		t.Fatalf("unexpected rewrite:\n%s", got)
	}
	if _, err := rewriteAllowlist(source, []versionChange{{"a/sqlite", "v0.9.0", "v1.1.0"}}); err == nil {
		t.Fatal("a stale approved version must not be rewritten")
	}
}

func TestRewriteNoticesMovesLinkCellAndHeading(t *testing.T) {
	source := "| [m/sqlite](https://pkg.go.dev/m/sqlite@v1.58.0) | 1.58.0 | BSD |\n\n## m/sqlite 1.58.0\n\n## other 1.58.0\n"
	got, touched := rewriteNotices(source, []versionChange{{"m/sqlite", "v1.58.0", "v1.59.0"}, {"m/libc", "v1.0.0", "v1.1.0"}})
	want := "| [m/sqlite](https://pkg.go.dev/m/sqlite@v1.59.0) | 1.59.0 | BSD |\n\n## m/sqlite 1.59.0\n\n## other 1.58.0\n"
	if got != want || len(touched) != 1 || touched[0] != "m/sqlite" {
		t.Fatalf("got %q touched %v", got, touched)
	}
}
