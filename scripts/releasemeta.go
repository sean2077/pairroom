package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

type artifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type provenance struct {
	Schema       string     `json:"schema"`
	Project      string     `json:"project"`
	Version      string     `json:"version"`
	SourceCommit string     `json:"source_commit"`
	BuildDate    string     `json:"build_date"`
	GoVersion    string     `json:"go_version"`
	Module       string     `json:"module"`
	Dependencies []string   `json:"dependencies"`
	Artifacts    []artifact `json:"artifacts"`
}

func main() {
	dist := flag.String("dist", "dist", "release artifact directory")
	version := flag.String("version", "", "release version")
	commit := flag.String("commit", "", "source commit")
	buildDate := flag.String("build-date", "", "RFC3339 build date")
	flag.Parse()
	if strings.TrimSpace(*version) == "" || strings.TrimSpace(*commit) == "" {
		fatal("version and commit are required")
	}
	if *buildDate == "" {
		*buildDate = time.Now().UTC().Format(time.RFC3339)
	}
	if _, err := time.Parse(time.RFC3339, *buildDate); err != nil {
		fatal("invalid build date: %v", err)
	}

	dependencies, err := moduleDependencies()
	if err != nil {
		fatal("resolve module dependencies: %v", err)
	}
	if err := writeSBOM(*dist, *version, *commit, *buildDate, dependencies); err != nil {
		fatal("write SBOM: %v", err)
	}
	artifacts, err := scanArtifacts(*dist, map[string]bool{"SHA256SUMS": true, "pairroom-v" + *version + "-provenance.json": true})
	if err != nil {
		fatal("scan artifacts: %v", err)
	}
	value := provenance{
		Schema:  "https://github.com/sean2077/pairroom/schemas/release-provenance-v1",
		Project: "PairRoom", Version: *version, SourceCommit: *commit, BuildDate: *buildDate,
		GoVersion: runtime.Version(), Module: "github.com/sean2077/pairroom", Dependencies: dependencies, Artifacts: artifacts,
	}
	if err := writeJSON(filepath.Join(*dist, "pairroom-v"+*version+"-provenance.json"), value); err != nil {
		fatal("write provenance: %v", err)
	}
	all, err := scanArtifacts(*dist, map[string]bool{"SHA256SUMS": true})
	if err != nil {
		fatal("scan checksums: %v", err)
	}
	var lines strings.Builder
	for _, item := range all {
		fmt.Fprintf(&lines, "%s  %s\n", item.SHA256, item.Name)
	}
	if err := os.WriteFile(filepath.Join(*dist, "SHA256SUMS"), []byte(lines.String()), 0o644); err != nil {
		fatal("write checksums: %v", err)
	}
}

func writeSBOM(dist, version, commit, buildDate string, dependencies []string) error {
	namespace := "https://github.com/sean2077/pairroom/releases/tag/v" + version + "#" + commit
	packages := []any{map[string]any{
		"name": "PairRoom", "SPDXID": "SPDXRef-Package-PairRoom", "versionInfo": version,
		"downloadLocation": "https://github.com/sean2077/pairroom",
		"filesAnalyzed":    false, "licenseConcluded": "MIT", "licenseDeclared": "MIT",
		"copyrightText": "Copyright (c) 2026 Sean",
		"externalRefs": []any{map[string]any{
			"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl",
			"referenceLocator": "pkg:golang/github.com/sean2077/pairroom@v" + version,
		}},
	}}
	relationships := []any{map[string]any{
		"spdxElementId": "SPDXRef-DOCUMENT", "relationshipType": "DESCRIBES", "relatedSpdxElement": "SPDXRef-Package-PairRoom",
	}}
	for index, dependency := range dependencies {
		path, dependencyVersion, _ := strings.Cut(dependency, "@")
		id := fmt.Sprintf("SPDXRef-Package-Dependency-%d", index+1)
		packages = append(packages, map[string]any{
			"name": path, "SPDXID": id, "versionInfo": dependencyVersion,
			"downloadLocation": "NOASSERTION", "filesAnalyzed": false,
			"licenseConcluded": "NOASSERTION", "licenseDeclared": "NOASSERTION", "copyrightText": "NOASSERTION",
			"externalRefs": []any{map[string]any{"referenceCategory": "PACKAGE-MANAGER", "referenceType": "purl", "referenceLocator": "pkg:golang/" + path + "@" + dependencyVersion}},
		})
		relationships = append(relationships, map[string]any{"spdxElementId": "SPDXRef-Package-PairRoom", "relationshipType": "DEPENDS_ON", "relatedSpdxElement": id})
	}
	doc := map[string]any{
		"spdxVersion":       "SPDX-2.3",
		"dataLicense":       "CC0-1.0",
		"SPDXID":            "SPDXRef-DOCUMENT",
		"name":              "PairRoom-v" + version,
		"documentNamespace": namespace,
		"creationInfo": map[string]any{
			"created":  buildDate,
			"creators": []string{"Tool: PairRoom-releasemeta", "Organization: PairRoom contributors"},
		},
		"packages":      packages,
		"relationships": relationships,
	}
	return writeJSON(filepath.Join(dist, "pairroom-v"+version+"-SBOM.spdx.json"), doc)
}

func moduleDependencies() ([]string, error) {
	command := exec.Command("go", "list", "-m", "-f", "{{if not .Main}}{{.Path}}@{{.Version}}{{end}}", "all")
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	var result []string
	for _, line := range strings.Split(string(output), "\n") {
		if value := strings.TrimSpace(line); value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result, nil
}

func scanArtifacts(dir string, skip map[string]bool) ([]artifact, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]artifact, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || skip[entry.Name()] {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		hash, err := fileHash(path)
		if err != nil {
			return nil, err
		}
		out = append(out, artifact{Name: entry.Name(), SHA256: hash, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func fileHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
