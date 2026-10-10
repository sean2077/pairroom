package attachment

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	MaxEvidenceBytes   int64 = 5 << 20
	MaxRoomSharedBytes int64 = 100 << 20
)

// SaveEvidence explicitly uploads a text artifact. References in message text
// never call this method. Scripts remain inert private data, without executable
// permission, automatic extraction, or a write into either user's workspace.
func (s *Store) SaveEvidence(name string, reader io.Reader, source string) (model.Attachment, error) {
	if reader == nil {
		return model.Attachment{}, errors.New("evidence reader is required")
	}
	name = safeEvidenceName(name)
	if name == "" {
		return model.Attachment{}, errors.New("evidence needs a valid display filename")
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxEvidenceBytes+1))
	if err != nil {
		return model.Attachment{}, fmt.Errorf("read evidence: %w", err)
	}
	if err := validateEvidence(data); err != nil {
		return model.Attachment{}, err
	}
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return model.Attachment{}, err
	}
	digest := sha256.Sum256(data)
	if source == "" {
		source = "upload"
	}
	meta := model.Attachment{
		ID: "att-" + hex.EncodeToString(nonce[:]), Name: name,
		Kind: "file", MediaType: "text/plain", Size: int64(len(data)),
		SHA256: hex.EncodeToString(digest[:]), Source: source, CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkSharedQuota(meta.Size); err != nil {
		return model.Attachment{}, err
	}
	return s.commitVerified(meta, data)
}

// ImportVerified installs the authoritative manifest and bytes into an owner's
// private cache. The original opaque ID is retained so a prepared claim can be
// rendered locally without transmitting a path. A repeated download succeeds
// only if every immutable field and the stored bytes still match.
func (s *Store) ImportVerified(expected model.Attachment, reader io.Reader) (model.Attachment, error) {
	if reader == nil || !attachmentIDPattern.MatchString(expected.ID) {
		return model.Attachment{}, errors.New("invalid attachment import")
	}
	if expected.Size <= 0 || expected.Size > MaxImageBytes || expected.CreatedAt.IsZero() {
		return model.Attachment{}, errors.New("invalid attachment manifest size or time")
	}
	if expected.Name == "" || len(expected.Name) > 256 || !utf8.ValidString(expected.Name) || strings.IndexFunc(expected.Name, unicode.IsControl) >= 0 || strings.ContainsAny(expected.Name, "/\\") {
		return model.Attachment{}, errors.New("invalid attachment display filename")
	}
	if len(expected.Source) > 128 || strings.IndexFunc(expected.Source, unicode.IsControl) >= 0 {
		return model.Attachment{}, errors.New("invalid attachment source")
	}
	data, err := io.ReadAll(io.LimitReader(reader, expected.Size+1))
	if err != nil {
		return model.Attachment{}, fmt.Errorf("download attachment: %w", err)
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != expected.Size || hex.EncodeToString(digest[:]) != expected.SHA256 {
		return model.Attachment{}, errors.New("downloaded attachment does not match its manifest")
	}
	switch expected.Kind {
	case "file":
		if expected.MediaType != "text/plain" || expected.Width != 0 || expected.Height != 0 {
			return model.Attachment{}, errors.New("invalid evidence metadata")
		}
		if err := validateEvidence(data); err != nil {
			return model.Attachment{}, err
		}
	case "image":
		if canonicalImageType(expected.MediaType) != expected.MediaType {
			return model.Attachment{}, errors.New("unsupported attachment image type")
		}
		width, height, err := decodeImageMetadata(bytes.NewReader(data), expected.MediaType, expected.Size)
		if err != nil || width != expected.Width || height != expected.Height {
			return model.Attachment{}, errors.New("attachment image dimensions do not match its manifest")
		}
	default:
		return model.Attachment{}, errors.New("unsupported attachment kind")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, _, err := s.Resolve(expected.ID); err == nil {
		if !sameAttachment(old, expected) {
			return model.Attachment{}, errors.New("attachment ID already has different metadata")
		}
		return old, nil
	} else if !errors.Is(err, ErrUnknown) {
		return model.Attachment{}, err
	}
	if err := s.checkSharedQuota(expected.Size); err != nil {
		return model.Attachment{}, err
	}
	return s.commitVerified(expected, data)
}

func sameAttachment(a, b model.Attachment) bool {
	return a.ID == b.ID && a.Name == b.Name && a.MediaType == b.MediaType && a.Kind == b.Kind && a.Size == b.Size && a.SHA256 == b.SHA256 && a.Width == b.Width && a.Height == b.Height && a.Source == b.Source && a.CreatedAt.Equal(b.CreatedAt)
}

func validateEvidence(data []byte) error {
	if len(data) == 0 || int64(len(data)) > MaxEvidenceBytes {
		return errors.New("evidence must contain between 1 byte and 5 MiB")
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return errors.New("evidence must be UTF-8 text without NUL bytes; upload a log, script, config, or patch")
	}
	return nil
}

func safeEvidenceName(name string) string {
	name = strings.TrimSpace(filepath.Base(strings.ReplaceAll(name, "\\", "/")))
	if name == "." || name == ".." || len(name) > 256 || !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return ""
	}
	return name
}

func (s *Store) checkSharedQuota(additional int64) error {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	total := additional
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("attachment store contains a non-regular entry")
		}
		total += info.Size()
		if total > MaxRoomSharedBytes {
			return errors.New("Room attachment storage exceeds 100 MiB; start a new Room for more evidence")
		}
	}
	return nil
}

// commitVerified is called with mu held and fully verified, bounded bytes.
func (s *Store) commitVerified(meta model.Attachment, data []byte) (model.Attachment, error) {
	ext := extensionForType(meta.MediaType)
	if meta.Kind == "file" {
		ext = ".data"
	}
	filename := meta.ID + ext
	path := filepath.Join(s.root, filename)
	if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return model.Attachment{}, errors.New("attachment content already exists; inspect the cache before retrying")
	}
	tmp, err := os.CreateTemp(s.root, ".attachment-*.tmp")
	if err != nil {
		return model.Attachment{}, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return model.Attachment{}, err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return model.Attachment{}, err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return model.Attachment{}, err
	}
	if err := tmp.Close(); err != nil {
		return model.Attachment{}, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return model.Attachment{}, err
	}
	if err := writeManifest(filepath.Join(s.root, meta.ID+".json"), manifest{Attachment: meta, Filename: filename}); err != nil {
		_ = os.Remove(path)
		return model.Attachment{}, err
	}
	return meta, nil
}
