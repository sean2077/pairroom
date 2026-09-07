package archive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Archive outputs must live outside the source Room directory. Otherwise a
// valid backup/diagnostics operation could replace events.jsonl or an attachment
// with an archive. Resolve existing ancestors before creating output directories
// so a symlinked parent cannot disguise a destination inside the source.
func archiveOutputPath(dataDir, output string) (string, error) {
	if strings.TrimSpace(output) == "" {
		return "", errors.New("archive output path is required")
	}
	absolute, err := filepath.Abs(output)
	if err != nil {
		return "", err
	}
	root, err := resolveExistingAncestors(dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve archive source: %w", err)
	}
	destination, err := resolveExistingAncestors(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve archive destination: %w", err)
	}
	relative, err := filepath.Rel(root, destination)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative) {
		return "", errors.New("archive output must be outside the source Room data directory")
	}
	return absolute, nil
}

// Diagnostics may inspect a missing data directory, and a new output may have
// missing parents. Resolve the existing prefix without creating either path.
func resolveExistingAncestors(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(absolute)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(absolute) == absolute {
			return "", err
		}
		suffix = append(suffix, filepath.Base(absolute))
		absolute = filepath.Dir(absolute)
	}
}
