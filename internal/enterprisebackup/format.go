// Package enterprisebackup stores typed owner-private backup bundles. It never
// executes SQL or accepts SQL identifiers or paths from a backup as authority.
package enterprisebackup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	FormatVersion             = 1
	SchemaVersion             = 2
	FactCodecVersion          = 2
	ObjectSchemaVersion       = 1
	MaxRowBytes               = 8 << 20
	MaxChunkBytes             = 64 << 20
	MaxManifestBytes          = 8 << 20
	MaxChunks                 = 512
	MaxObjects                = 16384
	MaxRows                   = 1000000
	MaxBundleBytes      int64 = 32 << 30
)

var ErrInvalid = errors.New("invalid or unsupported enterprise backup bundle")

type Header struct {
	BackupID            string    `json:"backup_id"`
	DeploymentID        string    `json:"deployment_id"`
	OrgID               string    `json:"org_id"`
	RestoreEpoch        string    `json:"restore_epoch"`
	CreatedAt           time.Time `json:"created_at"`
	SchemaVersion       int       `json:"schema_version"`
	FactCodecVersion    int       `json:"fact_codec_version"`
	ObjectSchemaVersion int       `json:"object_schema_version"`
}
type Chunk struct {
	Index  int    `json:"index"`
	Rows   uint64 `json:"rows"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Table struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    uint64   `json:"rows"`
	Chunks  []Chunk  `json:"chunks"`
}
type Manifest struct {
	Version int                `json:"version"`
	Header  Header             `json:"header"`
	Tables  []Table            `json:"tables"`
	Objects []model.Attachment `json:"objects"`
	Bytes   int64              `json:"bytes"`
	Rows    uint64             `json:"rows"`
}

func validID(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "/\\\x00\r\n") && strings.TrimSpace(value) == value
}
func (h Header) valid() bool {
	return validID(h.BackupID) && validID(h.DeploymentID) && validID(h.OrgID) && validID(h.RestoreEpoch) && !h.CreatedAt.IsZero() && h.SchemaVersion == SchemaVersion && h.FactCodecVersion == FactCodecVersion && h.ObjectSchemaVersion == ObjectSchemaVersion
}

// Tables and Columns return copies of compiled metadata. Runtime ownership and
// startup-recovery markers are deliberately absent, even when epochs repeat.
func Tables() []string {
	names := make([]string, 0, len(tableColumns))
	for name := range tableColumns {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
func Columns(table string) []string            { return append([]string(nil), tableColumns[table]...) }
func chunkName(table string, index int) string { return fmt.Sprintf("%s-%06d.jsonl", table, index) }

// CanonicalRow accepts exactly the fixed columns for one table and detects
// duplicate keys before decoding values. Values remain data, never SQL text.
func CanonicalRow(table string, data []byte) (json.RawMessage, error) {
	columns, ok := tableColumns[table]
	if !ok || len(data) == 0 || len(data) > MaxRowBytes {
		return nil, ErrInvalid
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrInvalid
	}
	row := make(map[string]json.RawMessage, len(columns))
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, ErrInvalid
		}
		name, ok := key.(string)
		if !ok {
			return nil, ErrInvalid
		}
		if _, exists := row[name]; exists {
			return nil, ErrInvalid
		}
		var value json.RawMessage
		if err = dec.Decode(&value); err != nil {
			return nil, ErrInvalid
		}
		row[name] = value
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrInvalid
	}
	if _, err = dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrInvalid
	}
	if len(row) != len(columns) {
		return nil, ErrInvalid
	}
	for _, name := range columns {
		if _, ok := row[name]; !ok {
			return nil, ErrInvalid
		}
	}
	canonical, err := json.Marshal(row)
	if err != nil || len(canonical) > MaxRowBytes {
		return nil, ErrInvalid
	}
	return canonical, nil
}
