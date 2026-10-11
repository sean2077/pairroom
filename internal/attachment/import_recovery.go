package attachment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/model"
)

func importedManifest(meta model.Attachment) (manifest, int64, error) {
	ext := extensionForType(meta.MediaType)
	if meta.Kind == "file" {
		ext = ".data"
	}
	m := manifest{Attachment: meta, Filename: meta.ID + ext}
	encoded, err := json.MarshalIndent(m, "", "  ")
	return m, int64(len(encoded)) + 1, err
}

// recoverImportedContent completes only the exact content-before-manifest
// interruption identified by a fresh authorized and fully verified import.
// The caller holds mu and the direct client's exclusive evidence-cache lock.
// No existing manifest, mismatched file or ambiguous ID may be overwritten.
func (s *Store) recoverImportedContent(m manifest, manifestBytes int64) (bool, error) {
	for _, ext := range contentExtensions {
		name := m.Attachment.ID + ext
		if name == m.Filename {
			continue
		}
		if _, err := os.Lstat(filepath.Join(s.root, name)); !errors.Is(err, os.ErrNotExist) {
			return false, errors.New("attachment import has conflicting existing content")
		}
	}
	path := filepath.Join(s.root, m.Filename)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != m.Attachment.Size {
		return false, errors.New("incomplete attachment content does not match its manifest")
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return false, errors.New("incomplete attachment changed while opening")
	}
	hash := sha256.New()
	size, copyErr := io.Copy(hash, io.LimitReader(file, MaxImageBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || size != m.Attachment.Size || hex.EncodeToString(hash.Sum(nil)) != m.Attachment.SHA256 {
		return false, errors.New("incomplete attachment does not match verified content")
	}
	// The content is already charged on disk. Only the missing manifest is
	// additional, so a retry near the Room limit does not double-charge bytes.
	if err := s.checkSharedQuota(manifestBytes); err != nil {
		return false, err
	}
	if err := s.checkTemporaryQuota(manifestBytes); err != nil {
		return false, err
	}
	if err := writeManifest(filepath.Join(s.root, m.Attachment.ID+".json"), m); err != nil {
		return false, err
	}
	return true, nil
}
