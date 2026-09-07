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
