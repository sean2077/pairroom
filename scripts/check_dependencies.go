//go:build ignore

// check_dependencies is the private implementation behind make check's
// dependency gate. It permits only the reviewed modernc SQLite closure and
// pins every selected module version so an indirect drift fails visibly.
//
// With -write (make deps-sync, run by a maintainer, never by CI) it records
// version-only changes of the already-approved module set in this allowlist
// and THIRD_PARTY_NOTICES.md and reports license-file differences for review.
// Added, removed, or replaced modules still require a deliberate manual edit.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	checkerPath = "scripts/check_dependencies.go"
	noticesPath = "THIRD_PARTY_NOTICES.md"
	syncHint    = "If this is a reviewed version update (for example a Dependabot PR), run `make deps-sync`, review the license report, add a CHANGELOG entry, and commit the result; see CONTRIBUTING.md#dependency-updates."
)

var allowed = map[string]string{
	"github.com/sean2077/pairroom":       "",
	"github.com/dustin/go-humanize":      "v1.0.1",
	"github.com/google/pprof":            "v0.0.0-20260802141513-ef3492d7dac3",
	"github.com/google/uuid":             "v1.6.0",
	"github.com/hashicorp/golang-lru/v2": "v2.0.7",
	"github.com/mattn/go-isatty":         "v0.0.24",
	"github.com/ncruces/go-strftime":     "v1.0.0",
	"github.com/remyoudompheng/bigfft":   "v0.0.0-20230129092748-24d4a6f8daec",
	"golang.org/x/mod":                   "v0.38.0",
	"golang.org/x/sync":                  "v0.22.0",
	"golang.org/x/sys":                   "v0.47.0",
	"golang.org/x/tools":                 "v0.48.0",
	"modernc.org/cc/v4":                  "v4.29.2",
	"modernc.org/ccgo/v4":                "v4.35.0",
	"modernc.org/fileutil":               "v1.4.0",
	"modernc.org/gc/v2":                  "v2.6.5",
	"modernc.org/gc/v3":                  "v3.1.5",
	"modernc.org/goabi0":                 "v0.2.0",
	"modernc.org/libc":                   "v1.75.7",
	"modernc.org/mathutil":               "v1.7.1",
	"modernc.org/memory":                 "v1.12.1",
	"modernc.org/opt":                    "v0.2.0",
	"modernc.org/sortutil":               "v1.2.1",
	"modernc.org/sqlite":                 "v1.59.0",
	"modernc.org/strutil":                "v1.2.1",
	"modernc.org/token":                  "v1.1.0",
}

type resolvedModule struct {
	Path    string
	Version string
	Replace *resolvedModule
}

func verifyModules(input io.Reader, approved map[string]string) []string {
	seen := make(map[string]bool, len(approved))
	var violations []string
	decoder := json.NewDecoder(input)
	for {
		var module resolvedModule
		err := decoder.Decode(&module)
		if err == io.EOF {
			break
		}
		if err != nil {
			violations = append(violations, "decode module graph: "+err.Error())
			break
		}
		if seen[module.Path] {
			violations = append(violations, "duplicate module "+module.Path)
		}
		seen[module.Path] = true
		if module.Replace != nil {
			violations = append(violations, fmt.Sprintf("unapproved replacement for %s: %s %s", module.Path, module.Replace.Path, module.Replace.Version))
		}
		want, ok := approved[module.Path]
		if !ok {
			violations = append(violations, "unapproved module "+module.Path+" "+module.Version)
			continue
		}
		if module.Version != want {
			violations = append(violations, fmt.Sprintf("module %s resolved to %s; want %s", module.Path, module.Version, want))
		}
	}
	for path := range approved {
		if !seen[path] {
			violations = append(violations, "approved module missing from graph: "+path)
		}
	}
	sort.Strings(violations)
	return violations
}

// versionChange is a newly resolved version of an already-approved module.
type versionChange struct{ Path, Old, New string }

// planVersionSync returns the version-only differences between the approved
// set and the graph. Anything else (an unapproved, missing, duplicate, or
// replaced module, or an unreadable graph) is a review decision, not a sync.
func planVersionSync(graph []byte, approved map[string]string) ([]versionChange, error) {
	var blocking []string
	for _, violation := range verifyModules(bytes.NewReader(graph), approved) {
		if !strings.Contains(violation, " resolved to ") {
			blocking = append(blocking, violation)
		}
	}
	if len(blocking) > 0 {
		return nil, fmt.Errorf("not a version-only update; edit the allowlist deliberately after review:\n  %s", strings.Join(blocking, "\n  "))
	}
	var changes []versionChange
	decoder := json.NewDecoder(bytes.NewReader(graph))
	for {
		var module resolvedModule
		if err := decoder.Decode(&module); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if want := approved[module.Path]; module.Version != want {
			changes = append(changes, versionChange{module.Path, want, module.Version})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// rewriteAllowlist replaces each changed version inside its own allowlist
// entry, failing rather than guessing when an entry is not found exactly once.
func rewriteAllowlist(source string, changes []versionChange) (string, error) {
	lines := strings.Split(source, "\n")
	for _, change := range changes {
		key, old := strconv.Quote(change.Path)+":", strconv.Quote(change.Old)
		hits := 0
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), key) && strings.Count(line, old) == 1 {
				lines[i] = strings.Replace(line, old, strconv.Quote(change.New), 1)
				hits++
			}
		}
		if hits != 1 {
			return "", fmt.Errorf("%s: found %d allowlist entries for %s %s", checkerPath, hits, change.Path, change.Old)
		}
	}
	formatted, err := format.Source([]byte(strings.Join(lines, "\n")))
	return string(formatted), err
}

// rewriteNotices moves the versioned link, table cell, and license heading of
// modules that THIRD_PARTY_NOTICES.md names individually; it returns their paths.
func rewriteNotices(source string, changes []versionChange) (string, []string) {
	var touched []string
	for _, change := range changes {
		oldPlain, newPlain := strings.TrimPrefix(change.Old, "v"), strings.TrimPrefix(change.New, "v")
		lines := strings.Split(source, "\n")
		hit := false
		for i, line := range lines {
			switch {
			case strings.Contains(line, change.Path+"@"+change.Old):
				line = strings.ReplaceAll(line, change.Path+"@"+change.Old, change.Path+"@"+change.New)
				lines[i] = strings.ReplaceAll(line, "| "+oldPlain+" |", "| "+newPlain+" |")
				hit = true
			case line == "## "+change.Path+" "+oldPlain:
				lines[i] = "## " + change.Path + " " + newPlain
				hit = true
			}
		}
		source = strings.Join(lines, "\n")
		if hit {
			touched = append(touched, change.Path)
		}
	}
	return source, touched
}

// licenseFiles returns the module-root license texts keyed by file name.
func licenseFiles(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		name := strings.ToUpper(entry.Name())
		for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "NOTICE", "PATENTS"} {
			if !entry.IsDir() && strings.HasPrefix(name, prefix) {
				if files[entry.Name()], err = os.ReadFile(filepath.Join(dir, entry.Name())); err != nil {
					return nil, err
				}
			}
		}
	}
	return files, nil
}

// licenseReport compares license files between the approved and new versions.
// It informs the maintainer's review; it never approves anything by itself.
func licenseReport(changes []versionChange) []string {
	var report []string
	for _, change := range changes {
		label := fmt.Sprintf("%s %s -> %s", change.Path, change.Old, change.New)
		var texts [2]map[string][]byte
		for i, version := range []string{change.Old, change.New} {
			output, err := exec.Command("go", "mod", "download", "-json", change.Path+"@"+version).Output()
			var module struct{ Dir, Error string }
			if err == nil {
				err = json.Unmarshal(output, &module)
			}
			if err == nil && module.Error != "" {
				err = errors.New(module.Error)
			}
			if err == nil {
				texts[i], err = licenseFiles(module.Dir)
			}
			if err != nil {
				report = append(report, fmt.Sprintf("REVIEW %s: cannot compare license files: %v", label, err))
				break
			}
		}
		if texts[0] == nil || texts[1] == nil {
			continue
		}
		var names []string
		changed := len(texts[0]) != len(texts[1]) || len(texts[1]) == 0
		for name, text := range texts[1] {
			names = append(names, name)
			if !bytes.Equal(texts[0][name], text) {
				changed = true
			}
		}
		sort.Strings(names)
		if changed {
			report = append(report, fmt.Sprintf("REVIEW %s: license files differ or are missing (now: %s)", label, strings.Join(names, ", ")))
		} else {
			report = append(report, fmt.Sprintf("ok     %s: license files unchanged (%s)", label, strings.Join(names, ", ")))
		}
	}
	return report
}

func syncApproved(graph []byte) error {
	changes, err := planVersionSync(graph, allowed)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		fmt.Println("dependency allowlist already matches the module graph")
		return nil
	}
	checker, err := os.ReadFile(checkerPath)
	if err != nil {
		return fmt.Errorf("run from the repository root: %w", err)
	}
	notices, err := os.ReadFile(noticesPath)
	if err != nil {
		return err
	}
	newChecker, err := rewriteAllowlist(string(checker), changes)
	if err != nil {
		return err
	}
	newNotices, touched := rewriteNotices(string(notices), changes)
	fmt.Println("License comparison against the approved versions:")
	for _, line := range licenseReport(changes) {
		fmt.Println("  " + line)
	}
	if err := os.WriteFile(checkerPath, []byte(newChecker), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(noticesPath, []byte(newNotices), 0o644); err != nil {
		return err
	}
	fmt.Println("Updated the reviewed allowlist in " + checkerPath + ":")
	for _, change := range changes {
		fmt.Printf("  %s %s -> %s\n", change.Path, change.Old, change.New)
	}
	if len(touched) > 0 {
		fmt.Printf("Updated %s for %s.\n", noticesPath, strings.Join(touched, ", "))
	}
	fmt.Println("Next: review the diff and any REVIEW lines above, add a CHANGELOG.md entry, and commit.")
	return nil
}

func main() {
	write := flag.Bool("write", false, "record version-only updates of approved modules (maintainer use; see make deps-sync)")
	flag.Parse()
	command := exec.Command("go", "list", "-mod=readonly", "-m", "-json", "all")
	output, err := command.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dependency check: go list -m all:", err)
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			fmt.Fprintln(os.Stderr, strings.TrimSpace(string(exit.Stderr)))
		}
		os.Exit(1)
	}
	if *write {
		if err := syncApproved(output); err != nil {
			fmt.Fprintln(os.Stderr, "dependency sync:", err)
			os.Exit(1)
		}
		return
	}
	if violations := verifyModules(bytes.NewReader(output), allowed); len(violations) > 0 {
		for _, violation := range violations {
			fmt.Fprintln(os.Stderr, "dependency check:", violation)
		}
		fmt.Fprintln(os.Stderr, "dependency check:", syncHint)
		os.Exit(1)
	}
	fmt.Printf("dependency allowlist ok (%d modules; no replacements)\n", len(allowed))
}
