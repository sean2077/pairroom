//go:build ignore

// check_dependencies is the private implementation behind make check's
// dependency gate. It permits only the reviewed modernc SQLite closure and
// pins every selected module version so an indirect drift fails visibly.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
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
	"modernc.org/libc":                   "v1.75.6",
	"modernc.org/mathutil":               "v1.7.1",
	"modernc.org/memory":                 "v1.12.1",
	"modernc.org/opt":                    "v0.2.0",
	"modernc.org/sortutil":               "v1.2.1",
	"modernc.org/sqlite":                 "v1.58.0",
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

func main() {
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
	if violations := verifyModules(bytes.NewReader(output), allowed); len(violations) > 0 {
		for _, violation := range violations {
			fmt.Fprintln(os.Stderr, "dependency check:", violation)
		}
		os.Exit(1)
	}
	fmt.Printf("dependency allowlist ok (%d modules; no replacements)\n", len(allowed))
}
