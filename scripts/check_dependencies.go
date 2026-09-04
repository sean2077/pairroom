//go:build ignore

// check_dependencies is the private implementation behind make check's
// dependency gate. It permits only the reviewed modernc SQLite closure and
// pins every selected module version so an indirect drift fails visibly.
package main

import (
	"bufio"
	"fmt"
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

func main() {
	command := exec.Command("go", "list", "-m", "-f", "{{.Path}} {{.Version}}", "all")
	output, err := command.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dependency check: go list -m all:", err)
		os.Exit(1)
	}
	seen := make(map[string]bool, len(allowed))
	var violations []string
	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		path := fields[0]
		version := ""
		if len(fields) > 1 {
			version = fields[1]
		}
		want, ok := allowed[path]
		if !ok {
			violations = append(violations, "unapproved module "+path+" "+version)
			continue
		}
		seen[path] = true
		if version != want {
			violations = append(violations, fmt.Sprintf("module %s resolved to %s; want %s", path, version, want))
		}
	}
	if err := scanner.Err(); err != nil {
		violations = append(violations, "scan module list: "+err.Error())
	}
	for path := range allowed {
		if !seen[path] {
			violations = append(violations, "approved module missing from graph: "+path)
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		for _, violation := range violations {
			fmt.Fprintln(os.Stderr, "dependency check:", violation)
		}
		os.Exit(1)
	}
	fmt.Printf("dependency allowlist ok (%d modules)\n", len(seen))
}
