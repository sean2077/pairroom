package attachment

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/sean2077/pairroom/internal/model"
)

// Staged contains verified private bytes which are not yet an attachment.
// Network ingress must bound concurrent stages before it calls StageShared.
// Close discards an uncommitted stage, including rejected multipart requests.
type Staged struct {
	mu    sync.Mutex
	store *Store
	meta  model.Attachment
	path  string
}

// StageShared streams one bounded upload to a private temporary file without
// holding the Store or Room engine lock. The caller must finish parsing the
// request and revalidate admission before committing the staged attachment.
func (s *Store) StageShared(kind, name string, reader io.Reader, source string) (*Staged, error) {
	if reader == nil || kind != "image" && kind != "file" {
		return nil, errors.New("an image or evidence stream is required")
	}
	if kind == "file" {
		name = safeEvidenceName(name)
		if name == "" {
			return nil, errors.New("evidence needs a valid display filename")
		}
	}
	file, err := s.createTemporaryStage()
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = s.discardTemporary(file.Name())
		}
	}()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(reader, MaxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("stage attachment: %w", err)
	}
	if size <= 0 || size > MaxImageBytes {
		return nil, errors.New("attachment must contain between 1 byte and 5 MiB")
	}
	if err := file.Truncate(size); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if source == "" {
		source = "upload"
	}
	meta := model.Attachment{ID: model.NewID("att"), Name: name, Kind: kind,
		Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), Source: source, CreatedAt: time.Now().UTC()}
	if kind == "file" {
		meta.MediaType = "text/plain"
		if err := validateEvidenceStream(file); err != nil {
			return nil, err
		}
	} else {
		var header [512]byte
		n, err := io.ReadFull(file, header[:])
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, err
		}
		meta.MediaType = canonicalImageType(http.DetectContentType(header[:n]))
		if meta.MediaType == "" {
			return nil, errors.New("only PNG, JPEG, GIF, and WebP images are supported")
		}
		meta.Name = safeDisplayName(name, meta.MediaType)
		meta.Width, meta.Height, err = decodeImageMetadata(file, meta.MediaType, size)
		if err != nil {
			return nil, fmt.Errorf("decode image metadata: %w", err)
		}
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	keep = true
	return &Staged{store: s, meta: meta, path: file.Name()}, nil
}

func validateEvidenceStream(reader io.Reader) error {
	text := bufio.NewReader(reader)
	for {
		r, width, err := text.ReadRune()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if r == 0 || r == utf8.RuneError && width == 1 {
			return errors.New("evidence must be UTF-8 text without NUL bytes; upload a log, script, config, or patch")
		}
	}
}

// Commit checks the shared quota and publishes the already-verified files.
// Only local filesystem operations occur here, so callers can serialize this
// short effect with membership revocation without locking around network I/O.
func (s *Staged) Commit() (model.Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return model.Attachment{}, errors.New("attachment stage is already closed or committed")
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	unlock, err := s.store.lockTemporaryDirectory()
	if err != nil {
		return model.Attachment{}, err
	}
	defer unlock()
	ext := extensionForType(s.meta.MediaType)
	if s.meta.Kind == "file" {
		ext = ".data"
	}
	encoded, err := json.MarshalIndent(manifest{Attachment: s.meta, Filename: s.meta.ID + ext}, "", "  ")
	if err != nil {
		return model.Attachment{}, err
	}
	if err := s.store.checkTemporaryQuota(int64(len(encoded)) + 1); err != nil {
		return model.Attachment{}, err
	}
	if err := s.store.checkSharedQuota(s.meta.Size + int64(len(encoded)) + 1); err != nil {
		return model.Attachment{}, err
	}
	meta, err := s.store.commitFile(s.meta, s.path)
	if err != nil {
		return model.Attachment{}, err
	}
	s.path = ""
	return meta, nil
}

func (s *Staged) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" {
		return nil
	}
	err := s.store.discardTemporary(s.path)
	if err == nil {
		s.path = ""
	}
	return err
}
