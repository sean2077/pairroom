package enterprisebackup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
)

// Bundle is available only after every manifest, table row, checksum and
// required object has passed validation. Accessors revalidate file contents;
// importers must keep all row effects inside their one atomic transaction.
type Bundle struct {
	root     string
	manifest Manifest
	digest   [32]byte
}

func (b *Bundle) Manifest() Manifest { return cloneManifest(b.manifest) }
func (b *Bundle) Digest() [32]byte   { return b.digest }

func Open(ctx context.Context, root, deployment, org string) (*Bundle, error) {
	path, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	for dir := path; ; dir = filepath.Dir(dir) {
		info, e := os.Lstat(dir)
		if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrInvalid
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	for _, dir := range []string{path, filepath.Join(path, "tables"), filepath.Join(path, "objects")} {
		if err = privatefile.CheckDirectory(dir); err != nil {
			return nil, err
		}
	}
	f, err := privatefile.Open(filepath.Join(path, "manifest.json"), MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxManifestBytes+1))
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	var m Manifest
	if len(data) > MaxManifestBytes || json.Unmarshal(data, &m) != nil {
		return nil, ErrInvalid
	}
	canonical, err := json.Marshal(m)
	if err != nil || !bytes.Equal(data, canonical) || m.Version != FormatVersion || !m.Header.valid() || m.Header.DeploymentID != deployment || m.Header.OrgID != org {
		return nil, ErrInvalid
	}
	b := &Bundle{root: path, manifest: m, digest: sha256.Sum256(data)}
	if err = b.validate(ctx); err != nil {
		return nil, err
	}
	return b, nil
}
func (b *Bundle) validate(ctx context.Context) error {
	m := b.manifest
	names := Tables()
	if len(m.Tables) != len(names) || len(m.Objects) > MaxObjects || m.Bytes < 0 || m.Bytes > MaxBundleBytes || m.Rows > MaxRows {
		return ErrInvalid
	}
	files := map[string]bool{}
	objectFiles := map[string]bool{}
	var totalBytes int64
	var totalRows uint64
	chunks := 0
	for i, t := range m.Tables {
		if t.Name != names[i] || !slices.Equal(t.Columns, Columns(t.Name)) || t.Rows > MaxRows || len(t.Chunks) > MaxChunks {
			return ErrInvalid
		}
		var rows uint64
		for j, c := range t.Chunks {
			if c.Index != j+1 || c.Rows == 0 || c.Size <= 0 || c.Size > MaxChunkBytes || c.Rows > MaxRows || !validDigest(c.SHA256) {
				return ErrInvalid
			}
			files[chunkName(t.Name, c.Index)] = true
			chunks++
			rows += c.Rows
			totalBytes += c.Size
			if chunks > MaxChunks || rows > MaxRows || totalBytes > MaxBundleBytes {
				return ErrInvalid
			}
		}
		if rows != t.Rows {
			return ErrInvalid
		}
		totalRows += rows
		if totalRows > MaxRows {
			return ErrInvalid
		}
		if err := b.TableRows(ctx, t.Name, func(json.RawMessage) error { return nil }); err != nil {
			return err
		}
	}
	previous := ""
	for _, meta := range m.Objects {
		if !attachment.ValidObjectID(meta.ID) || meta.ID <= previous || meta.CreatedAt.IsZero() || meta.Source != "upload" || attachment.ValidateObjectSpec(meta.Name, meta.Kind, meta.Size, meta.SHA256) != nil {
			return ErrInvalid
		}
		previous = meta.ID
		objectFiles[meta.ID] = true
		totalBytes += meta.Size
		if totalBytes > MaxBundleBytes {
			return ErrInvalid
		}
		if _, err := b.ReadObject(ctx, meta.ID); err != nil {
			return err
		}
	}
	if totalRows != m.Rows || totalBytes != m.Bytes {
		return ErrInvalid
	}
	if err := exactDirectory(b.root, map[string]bool{"manifest.json": false, "tables": true, "objects": true}); err != nil {
		return err
	}
	if err := exactFiles(filepath.Join(b.root, "tables"), files); err != nil {
		return err
	}
	return exactFiles(filepath.Join(b.root, "objects"), objectFiles)
}
func validDigest(value string) bool {
	data, err := hex.DecodeString(value)
	return err == nil && len(data) == 32 && hex.EncodeToString(data) == value
}
func exactFiles(dir string, files map[string]bool) error {
	expected := make(map[string]bool, len(files))
	for name := range files {
		expected[name] = false
	}
	return exactDirectory(dir, expected)
}
func exactDirectory(dir string, expected map[string]bool) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	if len(entries) != len(expected) {
		return ErrInvalid
	}
	for _, entry := range entries {
		directory, ok := expected[entry.Name()]
		if !ok || entry.Type()&os.ModeSymlink != 0 || entry.IsDir() != directory {
			return ErrInvalid
		}
		if !directory {
			info, e := entry.Info()
			if e != nil || !info.Mode().IsRegular() {
				return ErrInvalid
			}
		}
	}
	return nil
}

func (b *Bundle) TableRows(ctx context.Context, name string, consume func(json.RawMessage) error) error {
	if consume == nil || tableColumns[name] == nil {
		return ErrInvalid
	}
	var table *Table
	for i := range b.manifest.Tables {
		if b.manifest.Tables[i].Name == name {
			table = &b.manifest.Tables[i]
			break
		}
	}
	if table == nil {
		return ErrInvalid
	}
	for _, chunk := range table.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		f, err := privatefile.Open(filepath.Join(b.root, "tables", chunkName(name, chunk.Index)), MaxChunkBytes)
		if err != nil {
			return err
		}
		digest := sha256.New()
		counted := &countReader{reader: io.TeeReader(io.LimitReader(f, MaxChunkBytes+1), digest)}
		scanner := bufio.NewScanner(counted)
		scanner.Buffer(make([]byte, 64<<10), MaxRowBytes+2)
		var rows uint64
		for scanner.Scan() {
			if err = ctx.Err(); err != nil {
				break
			}
			raw := scanner.Bytes()
			canonical, e := CanonicalRow(name, raw)
			if e != nil || !bytes.Equal(canonical, raw) {
				err = ErrInvalid
				break
			}
			rows++
			if rows > chunk.Rows {
				err = ErrInvalid
				break
			}
			if err = consume(canonical); err != nil {
				break
			}
		}
		if err == nil {
			err = scanner.Err()
		}
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if rows != chunk.Rows || counted.bytes != chunk.Size || hex.EncodeToString(digest.Sum(nil)) != chunk.SHA256 {
			return ErrInvalid
		}
	}
	return nil
}

type countReader struct {
	reader io.Reader
	bytes  int64
}

func (r *countReader) Read(p []byte) (int, error) {
	n, e := r.reader.Read(p)
	r.bytes += int64(n)
	return n, e
}

func (b *Bundle) ReadObject(ctx context.Context, id string) ([]byte, error) {
	if !attachment.ValidObjectID(id) {
		return nil, ErrInvalid
	}
	var expected model.Attachment
	for _, meta := range b.manifest.Objects {
		if meta.ID == id {
			expected = meta
			break
		}
	}
	if expected.ID == "" {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := privatefile.Open(filepath.Join(b.root, "objects", id), attachment.MaxImageBytes)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, attachment.MaxImageBytes+1))
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	actual, err := attachment.ValidateObjectBytes(expected.Name, expected.Kind, data)
	actual.ID, actual.Source, actual.CreatedAt = expected.ID, expected.Source, expected.CreatedAt
	if err != nil || actual != expected {
		return nil, ErrInvalid
	}
	return data, nil
}
