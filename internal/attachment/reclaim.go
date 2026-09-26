package attachment

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// contentExtensions are the only content names SaveImage writes.
var contentExtensions = []string{".png", ".jpg", ".gif", ".webp"}

// ReclaimCandidates lists, oldest first, every attachment ID last written
// before cutoff. A manifest counts by the later of its recorded creation time
// and its file time; a content file left without a manifest (a crash inside
// SaveImage or Remove) counts by its file time. The list is unbounded so old
// referenced attachments cannot hide newer unreferenced ones from a caller
// that bounds its removals. It reads no image content and removes nothing.
// Being old is not permission to remove: the caller must prove under its
// admission lock that no durable message references an ID before Discard.
func (s *Store) ReclaimCandidates(cutoff time.Time) ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("read attachment directory: %w", err)
	}
	names := make(map[string]bool, len(entries))
	for _, entry := range entries {
		names[entry.Name()] = true
	}
	type candidate struct {
		id string
		at time.Time
	}
	var found []candidate
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		ext := filepath.Ext(name)
		id := strings.TrimSuffix(name, ext)
		if !attachmentIDPattern.MatchString(id) {
			continue
		}
		info, err := os.Lstat(filepath.Join(s.root, name))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		at := info.ModTime()
		if ext == ".json" {
			m, err := readManifest(filepath.Join(s.root, name), info)
			if err != nil || m.Attachment.ID != id {
				// Leave metadata this build cannot interpret for inspection.
				continue
			}
			if m.Attachment.CreatedAt.After(at) {
				at = m.Attachment.CreatedAt
			}
		} else if !knownContentExtension(ext) || names[id+".json"] {
			continue
		}
		if at.Before(cutoff) {
			found = append(found, candidate{id: id, at: at})
		}
	}
	sort.Slice(found, func(i, j int) bool {
		if !found[i].at.Equal(found[j].at) {
			return found[i].at.Before(found[j].at)
		}
		return found[i].id < found[j].id
	})
	ids := make([]string, 0, len(found))
	seen := make(map[string]bool, len(found))
	for _, value := range found {
		if !seen[value.id] {
			seen[value.id] = true
			ids = append(ids, value.id)
		}
	}
	return ids, nil
}

// Discard removes one attachment's manifest and content without hashing the
// content, so a reclamation pass stays cheap while its caller holds an
// admission lock. It reports whether any file was removed. The caller must
// already have proven that no durable message references id. As in Remove,
// the manifest goes first so an interruption leaves only an unreferenced
// content file, which a later pass reclaims. It does not take the upload
// lock, which SaveImage holds while reading a request body: an upload in
// progress is a hidden temporary file, and a just-committed one is younger
// than any reclamation cutoff, so neither is ever a candidate.
func (s *Store) Discard(id string) (bool, error) {
	if !attachmentIDPattern.MatchString(id) {
		return false, errors.New("invalid attachment id")
	}
	manifestPath := filepath.Join(s.root, id+".json")
	var content []string
	info, err := os.Lstat(manifestPath)
	switch {
	case err == nil:
		m, err := readManifest(manifestPath, info)
		if err != nil {
			return false, err
		}
		// A manifest may name only its own content; never another upload's.
		if m.Attachment.ID != id || filepath.Base(m.Filename) != m.Filename || !knownContentExtension(strings.TrimPrefix(m.Filename, id)) {
			return false, errors.New("invalid attachment metadata")
		}
		content = []string{m.Filename}
	case errors.Is(err, os.ErrNotExist):
		for _, ext := range contentExtensions {
			content = append(content, id+ext)
		}
	default:
		return false, fmt.Errorf("inspect attachment metadata: %w", err)
	}
	removed := false
	if err == nil {
		if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("remove attachment metadata: %w", err)
		}
		removed = true
	}
	for _, name := range content {
		path := filepath.Join(s.root, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return removed, fmt.Errorf("inspect attachment content: %w", err)
		}
		if info.IsDir() {
			return removed, errors.New("attachment content is a directory")
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("remove attachment content: %w", err)
		}
		removed = true
	}
	return removed, nil
}

func readManifest(path string, info os.FileInfo) (manifest, error) {
	if !info.Mode().IsRegular() {
		return manifest{}, errors.New("attachment metadata is not a regular file")
	}
	if info.Size() > maxManifestBytes {
		return manifest{}, errors.New("attachment metadata is too large")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest{}, fmt.Errorf("read attachment metadata: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return manifest{}, fmt.Errorf("decode attachment metadata: %w", err)
	}
	return m, nil
}

func knownContentExtension(ext string) bool {
	for _, known := range contentExtensions {
		if ext == known {
			return true
		}
	}
	return false
}
