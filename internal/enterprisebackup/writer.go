package enterprisebackup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
)

type RowSource func(emit func(json.RawMessage) error) error
type Writer struct {
	root     string
	manifest Manifest
	tables   map[string]bool
	objects  map[string]bool
	failed   error
	sealed   bool
	chunks   int
}

// NewWriter requires a fresh directory. Failed bundles remain private and
// incomplete; reusing a partial directory never overwrites accepted evidence.
func NewWriter(root string, header Header) (*Writer, error) {
	if !header.valid() {
		return nil, ErrInvalid
	}
	path, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if _, err = os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return nil, ErrInvalid
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, e := os.Lstat(parent)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrInvalid
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	if err = privatefile.Mkdir(path); err != nil {
		return nil, err
	}
	for _, name := range []string{"tables", "objects"} {
		if err = privatefile.Mkdir(filepath.Join(path, name)); err != nil {
			return nil, err
		}
	}
	return &Writer{root: path, manifest: Manifest{Version: FormatVersion, Header: header, Tables: []Table{}, Objects: []model.Attachment{}}, tables: map[string]bool{}, objects: map[string]bool{}}, nil
}
func (w *Writer) fail(err error) error {
	if err != nil {
		w.failed = err
	}
	return err
}
func (w *Writer) ready() error {
	if w == nil || w.sealed {
		return ErrInvalid
	}
	return w.failed
}

func (w *Writer) WriteTable(ctx context.Context, name string, source RowSource) error {
	if err := w.ready(); err != nil {
		return err
	}
	if _, ok := tableColumns[name]; !ok || w.tables[name] || source == nil {
		return w.fail(ErrInvalid)
	}
	table := Table{Name: name, Columns: Columns(name), Chunks: []Chunk{}}
	var file *os.File
	var digest hash.Hash
	var size int64
	var rows uint64
	closeChunk := func() error {
		if file == nil {
			return nil
		}
		defer os.Remove(file.Name())
		if err := file.Sync(); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		chunk := Chunk{Index: len(table.Chunks) + 1, Rows: rows, Size: size, SHA256: hex.EncodeToString(digest.Sum(nil))}
		if err := privatefile.Install(file.Name(), filepath.Join(w.root, "tables", chunkName(name, chunk.Index))); err != nil {
			return err
		}
		table.Chunks = append(table.Chunks, chunk)
		w.chunks++
		file = nil
		return nil
	}
	var emissionErr error
	err := source(func(raw json.RawMessage) (emitError error) {
		if emissionErr != nil {
			return emissionErr
		}
		defer func() {
			if emitError != nil {
				emissionErr = emitError
			}
		}()
		if err := ctx.Err(); err != nil {
			return err
		}
		canonical, err := CanonicalRow(name, raw)
		if err != nil {
			return err
		}
		length := int64(len(canonical) + 1)
		if w.manifest.Bytes+length > MaxBundleBytes || w.manifest.Rows >= MaxRows {
			return ErrInvalid
		}
		if file != nil && size+length > MaxChunkBytes {
			if err = closeChunk(); err != nil {
				return err
			}
		}
		if file == nil {
			if w.chunks >= MaxChunks {
				return ErrInvalid
			}
			file, err = privatefile.CreateTemp(filepath.Join(w.root, "tables"))
			if err != nil {
				return err
			}
			digest = sha256.New()
			size, rows = 0, 0
		}
		canonical = append(canonical, '\n')
		if _, err = io.MultiWriter(file, digest).Write(canonical); err != nil {
			return err
		}
		size += length
		rows++
		table.Rows++
		w.manifest.Rows++
		w.manifest.Bytes += length
		return nil
	})
	if err == nil {
		err = emissionErr
	}
	if err == nil {
		err = closeChunk()
	}
	if file != nil {
		file.Close()
		os.Remove(file.Name())
	}
	if err != nil {
		return w.fail(err)
	}
	w.tables[name] = true
	w.manifest.Tables = append(w.manifest.Tables, table)
	return nil
}

func (w *Writer) WriteObject(ctx context.Context, meta model.Attachment, reader io.Reader) error {
	if err := w.ready(); err != nil {
		return err
	}
	if reader == nil || !attachment.ValidObjectID(meta.ID) || w.objects[meta.ID] || len(w.objects) >= MaxObjects || meta.CreatedAt.IsZero() || meta.Source != "upload" || attachment.ValidateObjectSpec(meta.Name, meta.Kind, meta.Size, meta.SHA256) != nil {
		return w.fail(ErrInvalid)
	}
	data, err := io.ReadAll(io.LimitReader(reader, meta.Size+1))
	if err != nil {
		return w.fail(err)
	}
	if err = ctx.Err(); err != nil {
		return w.fail(err)
	}
	actual, err := attachment.ValidateObjectBytes(meta.Name, meta.Kind, data)
	actual.ID, actual.Source, actual.CreatedAt = meta.ID, meta.Source, meta.CreatedAt
	if err != nil || actual != meta || w.manifest.Bytes+int64(len(data)) > MaxBundleBytes {
		return w.fail(ErrInvalid)
	}
	if err = writePrivate(filepath.Join(w.root, "objects", meta.ID), data); err != nil {
		return w.fail(err)
	}
	w.objects[meta.ID] = true
	w.manifest.Objects = append(w.manifest.Objects, meta)
	w.manifest.Bytes += int64(len(data))
	return nil
}
func writePrivate(path string, data []byte) error {
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return ErrInvalid
	}
	f, err := privatefile.CreateTemp(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return privatefile.Install(f.Name(), path)
}
func (w *Writer) Seal(ctx context.Context) (Manifest, error) {
	if err := w.ready(); err != nil {
		return Manifest{}, err
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, w.fail(err)
	}
	if len(w.tables) != len(tableColumns) {
		return Manifest{}, w.fail(ErrInvalid)
	}
	sort.Slice(w.manifest.Tables, func(i, j int) bool { return w.manifest.Tables[i].Name < w.manifest.Tables[j].Name })
	sort.Slice(w.manifest.Objects, func(i, j int) bool { return w.manifest.Objects[i].ID < w.manifest.Objects[j].ID })
	encoded, err := json.Marshal(w.manifest)
	if err != nil || len(encoded) > MaxManifestBytes {
		return Manifest{}, w.fail(ErrInvalid)
	}
	if err = writePrivate(filepath.Join(w.root, "manifest.json"), encoded); err != nil {
		return Manifest{}, w.fail(err)
	}
	w.sealed = true
	return cloneManifest(w.manifest), nil
}
func cloneManifest(m Manifest) Manifest {
	m.Objects = append([]model.Attachment(nil), m.Objects...)
	tables := make([]Table, len(m.Tables))
	for i, t := range m.Tables {
		t.Columns = append([]string(nil), t.Columns...)
		t.Chunks = append([]Chunk(nil), t.Chunks...)
		tables[i] = t
	}
	m.Tables = tables
	return m
}
