// Package enterpriseobject owns private immutable enterprise attachment bytes.
// SQL owns authorization, lifecycle and references; this package has no Local
// Room paths, content-addressed capabilities or HTTP surface.
package enterpriseobject

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
)

var (
	ErrInvalid  = errors.New("invalid enterprise object")
	ErrDeleted  = errors.New("enterprise object is permanently deleted")
	ErrConflict = errors.New("enterprise object identity conflicts with immutable bytes")
	ErrBackup   = errors.New("enterprise object deletion blocked by backup retention")
)

type Spec struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (s Spec) Validate() error {
	return attachment.ValidateObjectSpec(s.Name, s.Kind, s.Size, s.SHA256)
}

// Verified cannot be constructed from an HTTP assertion. Only a full private
// object read/installation can produce this value; SQL still verifies its
// deployment, object identity and frozen reservation specification.
type Verified struct {
	deployment string
	meta       model.Attachment
}

func (v Verified) DeploymentID() string       { return v.deployment }
func (v Verified) Metadata() model.Attachment { return v.meta }

type Store struct{ root, deployment string }
type rootMarker struct {
	Version    int    `json:"version"`
	Deployment string `json:"deployment"`
}
type objectManifest struct {
	Version    int              `json:"version"`
	Deployment string           `json:"deployment"`
	Metadata   model.Attachment `json:"metadata"`
}
type backupBarriers struct {
	IDs []string `json:"ids"`
}

func Open(root, deployment string) (*Store, error) {
	if deployment == "" || len(deployment) > 256 || strings.ContainsAny(deployment, "/\\\x00\r\n") {
		return nil, ErrInvalid
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Parent directories may be shared, but none may redirect traversal.
	for parent := filepath.Dir(absolute); ; parent = filepath.Dir(parent) {
		info, e := os.Lstat(parent)
		if e != nil {
			return nil, e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrInvalid
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	if err = privatefile.Mkdir(absolute); err != nil {
		return nil, err
	}
	unlock, err := privatelock.Lock(context.Background(), absolute)
	if err != nil {
		return nil, err
	}
	defer unlock()
	marker := rootMarker{}
	path := filepath.Join(absolute, "object-store.json")
	if err = privatefile.ReadJSON(path, 4096, &marker); errors.Is(err, os.ErrNotExist) {
		entries, e := os.ReadDir(absolute)
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".lock") {
				return nil, ErrInvalid
			}
		}
		marker = rootMarker{Version: 1, Deployment: deployment}
		err = privatefile.WriteJSON(path, marker)
	}
	if err != nil {
		return nil, err
	}
	if marker.Version != 1 || marker.Deployment != deployment {
		return nil, ErrInvalid
	}
	return &Store{root: absolute, deployment: deployment}, nil
}
func (s *Store) DeploymentID() string { return s.deployment }

// Pristine permits only the private root marker and lock infrastructure. It
// never follows paths and refuses preexisting objects or retention journals.
func (s *Store) Pristine(ctx context.Context) (bool, error) {
	unlock, err := privatelock.Lock(ctx, s.root)
	if err != nil {
		return false, err
	}
	defer unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != "object-store.json" {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) objectDir(id string, create bool) (string, error) {
	if s == nil || !attachment.ValidObjectID(id) {
		return "", ErrInvalid
	}
	if err := privatefile.CheckDirectory(s.root); err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, id)
	var err error
	if create {
		err = privatefile.Mkdir(dir)
	} else {
		err = privatefile.CheckDirectory(dir)
	}
	return dir, err
}
func isDeleted(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, "deleted.json"))
	return !errors.Is(err, os.ErrNotExist)
}

func (s *Store) PutImmutable(ctx context.Context, id string, spec Spec, reader io.Reader) (Verified, error) {
	if reader == nil || spec.Validate() != nil {
		return Verified{}, ErrInvalid
	}
	dir, err := s.objectDir(id, true)
	if err != nil {
		return Verified{}, err
	}
	unlock, err := privatelock.Lock(ctx, dir)
	if err != nil {
		return Verified{}, err
	}
	defer unlock()
	if isDeleted(dir) {
		return Verified{}, ErrDeleted
	}
	data, err := io.ReadAll(io.LimitReader(reader, spec.Size+1))
	if err != nil {
		return Verified{}, err
	}
	if err = ctx.Err(); err != nil {
		return Verified{}, err
	}
	meta, err := attachment.ValidateObjectBytes(spec.Name, spec.Kind, data)
	if err != nil || meta.Size != spec.Size || meta.SHA256 != spec.SHA256 {
		return Verified{}, ErrInvalid
	}
	meta.ID = id
	meta.Source = "upload"
	if old, e := s.inspectLocked(dir, id); e == nil {
		if old.meta != meta {
			return Verified{}, ErrConflict
		}
		return old, nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return Verified{}, e
	}
	// A crash between data and manifest is recovered only by identical bytes.
	path := filepath.Join(dir, "data")
	if existing, e := privatefile.Open(path, attachment.MaxImageBytes); e == nil {
		b, re := io.ReadAll(existing)
		ce := existing.Close()
		if re != nil {
			return Verified{}, re
		}
		if ce != nil {
			return Verified{}, ce
		}
		actual, ve := attachment.ValidateObjectBytes(spec.Name, spec.Kind, b)
		if ve != nil || actual.SHA256 != spec.SHA256 || actual.Size != spec.Size {
			return Verified{}, ErrConflict
		}
	} else if errors.Is(e, os.ErrNotExist) {
		f, e := privatefile.CreateTemp(dir)
		if e != nil {
			return Verified{}, e
		}
		defer os.Remove(f.Name())
		if _, e = f.Write(data); e != nil {
			f.Close()
			return Verified{}, e
		}
		if e = f.Sync(); e != nil {
			f.Close()
			return Verified{}, e
		}
		if e = f.Close(); e != nil {
			return Verified{}, e
		}
		if e = privatefile.Install(f.Name(), path); e != nil {
			return Verified{}, e
		}
	} else {
		return Verified{}, e
	}
	if err = privatefile.WriteJSON(filepath.Join(dir, "manifest.json"), objectManifest{1, s.deployment, meta}); err != nil {
		return Verified{}, err
	}
	return Verified{s.deployment, meta}, nil
}

func (s *Store) inspectLocked(dir, id string) (Verified, error) {
	if isDeleted(dir) {
		return Verified{}, ErrDeleted
	}
	var m objectManifest
	if err := privatefile.ReadJSON(filepath.Join(dir, "manifest.json"), 8192, &m); err != nil {
		return Verified{}, err
	}
	if m.Version != 1 || m.Deployment != s.deployment || m.Metadata.ID != id || !m.Metadata.CreatedAt.IsZero() || m.Metadata.Source != "upload" {
		return Verified{}, ErrInvalid
	}
	f, err := privatefile.Open(filepath.Join(dir, "data"), attachment.MaxImageBytes)
	if err != nil {
		return Verified{}, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, attachment.MaxImageBytes+1))
	if err != nil {
		return Verified{}, err
	}
	actual, err := attachment.ValidateObjectBytes(m.Metadata.Name, m.Metadata.Kind, b)
	if err != nil {
		return Verified{}, err
	}
	actual.ID = id
	actual.Source = "upload"
	if actual != m.Metadata {
		return Verified{}, ErrInvalid
	}
	return Verified{s.deployment, actual}, nil
}
func (s *Store) Inspect(ctx context.Context, id string) (Verified, error) {
	dir, err := s.objectDir(id, false)
	if err != nil {
		return Verified{}, err
	}
	unlock, err := privatelock.Lock(ctx, dir)
	if err != nil {
		return Verified{}, err
	}
	defer unlock()
	return s.inspectLocked(dir, id)
}

// ReadVerified buffers the bounded object before returning it. The caller must
// perform a final fenced SQL authorization immediately before releasing bytes.
func (s *Store) ReadVerified(ctx context.Context, expected model.Attachment) ([]byte, error) {
	dir, err := s.objectDir(expected.ID, false)
	if err != nil {
		return nil, err
	}
	unlock, err := privatelock.Lock(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	v, err := s.inspectLocked(dir, expected.ID)
	if err != nil {
		return nil, err
	}
	meta := expected
	meta.CreatedAt = v.meta.CreatedAt
	if meta != v.meta {
		return nil, ErrInvalid
	}
	f, err := privatefile.Open(filepath.Join(dir, "data"), attachment.MaxImageBytes)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, attachment.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	check, err := attachment.ValidateObjectBytes(expected.Name, expected.Kind, data)
	if err != nil || check.Size != expected.Size || check.SHA256 != expected.SHA256 {
		return nil, ErrInvalid
	}
	return data, nil
}

func validBarrierID(id string) bool {
	return id != "" && len(id) <= 128 && !strings.ContainsAny(id, "/\\\x00\r\n")
}
func (s *Store) barriers() (backupBarriers, error) {
	var b backupBarriers
	err := privatefile.ReadJSON(filepath.Join(s.root, "backup-barriers.json"), 16384, &b)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return b, err
}
func (s *Store) BeginBackup(ctx context.Context, id string) error {
	return s.changeBackup(ctx, id, true)
}
func (s *Store) EndBackup(ctx context.Context, id string) error {
	return s.changeBackup(ctx, id, false)
}
func (s *Store) changeBackup(ctx context.Context, id string, add bool) error {
	if !validBarrierID(id) {
		return ErrInvalid
	}
	unlock, err := privatelock.Lock(ctx, s.root)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := s.barriers()
	if err != nil {
		return err
	}
	for i, v := range b.IDs {
		if v == id {
			if add {
				return nil
			}
			b.IDs = append(b.IDs[:i], b.IDs[i+1:]...)
			return privatefile.WriteJSON(filepath.Join(s.root, "backup-barriers.json"), b)
		}
	}
	if !add {
		return nil
	}
	if len(b.IDs) >= 64 {
		return ErrBackup
	}
	b.IDs = append(b.IDs, id)
	return privatefile.WriteJSON(filepath.Join(s.root, "backup-barriers.json"), b)
}

// Delete first installs a durable tombstone. A delayed uploader can never
// recreate this identity after SQL acknowledges deletion and releases quota.
func (s *Store) Delete(ctx context.Context, id string) error {
	rootUnlock, err := privatelock.Lock(ctx, s.root)
	if err != nil {
		return err
	}
	defer rootUnlock()
	b, err := s.barriers()
	if err != nil {
		return err
	}
	if len(b.IDs) != 0 {
		return ErrBackup
	}
	dir, err := s.objectDir(id, true)
	if err != nil {
		return err
	}
	unlock, err := privatelock.Lock(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	if err = privatefile.WriteJSON(filepath.Join(dir, "deleted.json"), rootMarker{1, s.deployment}); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "deleted.json" {
			continue
		}
		if name != "data" && name != "manifest.json" && !strings.HasPrefix(name, ".identity-") {
			return ErrInvalid
		}
		path := filepath.Join(dir, name)
		if info, e := os.Lstat(path); e == nil {
			if !info.Mode().IsRegular() {
				return ErrInvalid
			}
			f, e := privatefile.Open(path, attachment.MaxImageBytes)
			if e != nil {
				return e
			}
			if e = f.Close(); e != nil {
				return e
			}
			if e = os.Remove(path); e != nil {
				return e
			}
		} else if !errors.Is(e, os.ErrNotExist) {
			return e
		}
	}
	return privatefile.SyncDirectory(dir)
}
