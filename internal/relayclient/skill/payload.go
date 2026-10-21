// Package skill exposes the bundled product payload without importing the relay CLI.
package skill

import (
	"embed"
	"io/fs"
	"strings"
)

// Content is the projection checked against skills/pairroom-relay/SKILL.md.
//
//go:embed pairroom-relay/SKILL.md
var Content string

// ReferenceMarker identifies reference files distributed by PairRoom. The
// user-scoped installer refuses to overwrite unrelated files at these paths.
const ReferenceMarker = "<!-- pairroom-relay:reference -->"

//go:embed pairroom-relay
var bundle embed.FS

// File is one bundled payload, with a slash-separated path relative to the skill.
type File struct {
	Path    string
	Content string
}

// Files returns the whole skill in stable path order. Callers must install its
// references as well as SKILL.md; the manifest loads references only on demand.
func Files() []File {
	var files []File
	err := fs.WalkDir(bundle, "pairroom-relay", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := bundle.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, File{Path: strings.TrimPrefix(path, "pairroom-relay/"), Content: string(data)})
		return nil
	})
	if err != nil {
		panic(err) // The embedded, immutable bundle cannot fail at runtime.
	}
	return files
}
