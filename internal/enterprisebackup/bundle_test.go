package enterprisebackup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/privatefile"
)

func testHeader() Header {
	return Header{BackupID: "backup-one", DeploymentID: "dep-test", OrgID: "org-test", RestoreEpoch: "before-restore", CreatedAt: time.Unix(1000000000, 0).UTC(), SchemaVersion: SchemaVersion, FactCodecVersion: FactCodecVersion, ObjectSchemaVersion: ObjectSchemaVersion}
}

var testUser = json.RawMessage(`{"user_id":"u-one","org_id":"org-test","disabled":false,"tombstoned_at":null,"created_at":"2026-10-13T00:00:00Z"}`)

func testBundle(t *testing.T) (string, Manifest) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "bundle")
	w, e := NewWriter(root, testHeader())
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range Tables() {
		if e = w.WriteTable(t.Context(), name, func(emit func(json.RawMessage) error) error {
			if name == "users" {
				return emit(testUser)
			}
			return nil
		}); e != nil {
			t.Fatal(e)
		}
	}
	meta, e := attachment.ValidateObjectBytes("evidence.txt", "file", []byte("evidence"))
	if e != nil {
		t.Fatal(e)
	}
	meta.ID = "att-0123456789abcdef01234567"
	meta.Source = "upload"
	meta.CreatedAt = testHeader().CreatedAt
	if e = w.WriteObject(t.Context(), meta, bytes.NewBufferString("evidence")); e != nil {
		t.Fatal(e)
	}
	manifest, e := w.Seal(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	return root, manifest
}
func TestBundlePrivateRoundTripAndRuntimeAuthorityExclusion(t *testing.T) {
	root, manifest := testBundle(t)
	b, e := Open(t.Context(), root, "dep-test", "org-test")
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(b.Manifest(), manifest) || b.Digest() == ([32]byte{}) {
		t.Fatal("manifest changed")
	}
	var users []json.RawMessage
	if e = b.TableRows(t.Context(), "users", func(row json.RawMessage) error { users = append(users, row); return nil }); e != nil || len(users) != 1 {
		t.Fatal(users, e)
	}
	canonical, _ := CanonicalRow("users", testUser)
	if !bytes.Equal(users[0], canonical) {
		t.Fatal("row changed")
	}
	data, e := b.ReadObject(t.Context(), manifest.Objects[0].ID)
	if e != nil || string(data) != "evidence" {
		t.Fatal(e)
	}
	for _, runtimeTable := range []string{"ownership", "schema_meta", "startup_recovery_runs", "startup_room_recoveries"} {
		if Columns(runtimeTable) != nil {
			t.Fatal("runtime authority leaked into backup registry")
		}
	}
	copy := b.Manifest()
	copy.Tables[0].Columns[0] = "forged"
	copy.Objects[0].Name = "forged"
	if reflect.DeepEqual(copy, b.Manifest()) {
		t.Fatal("caller mutated validated manifest")
	}
	for _, scope := range [][2]string{{"other", "org-test"}, {"dep-test", "other"}} {
		if _, e = Open(t.Context(), root, scope[0], scope[1]); e == nil {
			t.Fatal("accepted wrong backup scope")
		}
	}
}
func TestBundleRejectsUnknownColumnsDuplicateKeysAndIncompleteSeal(t *testing.T) {
	for _, raw := range []string{strings.Replace(string(testUser), `"disabled":false`, `"disabled":false,"disabled":true`, 1), strings.Replace(string(testUser), `"disabled":false`, `"admin":true`, 1), string(testUser) + ` {}`, `[]`} {
		if _, e := CanonicalRow("users", []byte(raw)); e == nil {
			t.Fatal("accepted ambiguous row")
		}
	}
	if _, e := CanonicalRow("users;DROP TABLE users", testUser); e == nil {
		t.Fatal("accepted SQL table text")
	}
	root := filepath.Join(t.TempDir(), "incomplete")
	w, e := NewWriter(root, testHeader())
	if e != nil {
		t.Fatal(e)
	}
	e = w.WriteTable(t.Context(), "users", func(emit func(json.RawMessage) error) error { _ = emit([]byte(`{"bad":1}`)); return nil })
	if e == nil {
		t.Fatal("source suppressed validation error")
	}
	if _, e = w.Seal(t.Context()); e == nil {
		t.Fatal("published partial manifest")
	}
	if _, e = os.Stat(filepath.Join(root, "manifest.json")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("partial manifest exists")
	}
	if _, e = NewWriter(root, testHeader()); e == nil {
		t.Fatal("overwrote partial backup directory")
	}
}
func TestBundleRejectsCorruptionUnknownFilesAndFutureVersions(t *testing.T) {
	for _, kind := range []string{"row", "object", "extra", "version", "path"} {
		t.Run(kind, func(t *testing.T) {
			root, m := testBundle(t)
			switch kind {
			case "row":
				if e := os.WriteFile(filepath.Join(root, "tables", chunkName("users", 1)), append(testUser, '\n'), 0600); e != nil {
					t.Fatal(e)
				}
			case "object":
				if e := os.WriteFile(filepath.Join(root, "objects", m.Objects[0].ID), []byte("tampered"), 0600); e != nil {
					t.Fatal(e)
				}
			case "extra":
				if e := writePrivate(filepath.Join(root, "objects", "extra"), []byte("x")); e != nil {
					t.Fatal(e)
				}
			case "version":
				m.Version++
				data, _ := json.Marshal(m)
				if e := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0600); e != nil {
					t.Fatal(e)
				}
			case "path":
				m.Tables[0].Name = "../outside"
				data, _ := json.Marshal(m)
				if e := os.WriteFile(filepath.Join(root, "manifest.json"), data, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := Open(t.Context(), root, "dep-test", "org-test"); e == nil {
				t.Fatal("accepted damaged bundle")
			}
		})
	}
}
func TestBundleRevalidatesRowsAndRejectsSymlink(t *testing.T) {
	root, _ := testBundle(t)
	b, e := Open(t.Context(), root, "dep-test", "org-test")
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "tables", chunkName("users", 1))
	if e = os.WriteFile(path, []byte("{}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = b.TableRows(t.Context(), "users", func(json.RawMessage) error { return nil }); e == nil {
		t.Fatal("trusted data changed after validation")
	}
	root, _ = testBundle(t)
	path = filepath.Join(root, "tables", chunkName("users", 1))
	outsideDir := filepath.Join(t.TempDir(), "private")
	if e = privatefile.Mkdir(outsideDir); e != nil {
		t.Fatal(e)
	}
	outside := filepath.Join(outsideDir, "outside")
	if e = writePrivate(outside, []byte("{}\n")); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(path); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(outside, path); e != nil {
		t.Skipf("symlink unavailable: %v", e)
	}
	if _, e = Open(t.Context(), root, "dep-test", "org-test"); e == nil {
		t.Fatal("read redirected table")
	}
}
func TestBundleCancellationCannotPublish(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cancelled")
	w, e := NewWriter(root, testHeader())
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if e = w.WriteTable(ctx, "users", func(emit func(json.RawMessage) error) error { return emit(testUser) }); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e = w.Seal(t.Context()); e == nil {
		t.Fatal("sealed cancelled bundle")
	}
}
func TestBackupRegistryMatchesCurrentSQLColumns(t *testing.T) {
	data, e := os.ReadFile(filepath.Join("..", "sqlstore", "schema.sql"))
	if e != nil {
		t.Fatal(e)
	}
	sql := string(data)
	actual := map[string][]string{}
	tables := regexp.MustCompile(`(?s)CREATE TABLE ([a-z_][a-z0-9_]*) \((.*?)\n\);`)
	column := regexp.MustCompile(`^\s{2}([a-z_][a-z0-9_]*)\s+(?:text|boolean|bigserial|bigint|integer|bytea|timestamptz)\b`)
	for _, match := range tables.FindAllStringSubmatch(sql, -1) {
		name := match[1]
		if name == "schema_meta" || name == "ownership" || name == "startup_room_recoveries" || name == "startup_recovery_runs" || name == "restore_runs" {
			continue
		}
		for _, line := range strings.Split(match[2], "\n") {
			if c := column.FindStringSubmatch(line); c != nil {
				actual[name] = append(actual[name], c[1])
			}
		}
	}
	for _, match := range regexp.MustCompile(`ALTER TABLE ([a-z_]+) ADD COLUMN ([a-z_][a-z0-9_]*)`).FindAllStringSubmatch(sql, -1) {
		actual[match[1]] = append(actual[match[1]], match[2])
	}
	if !reflect.DeepEqual(actual, tableColumns) {
		t.Fatal("backup fixed table/column registry drifted from supported SQL shape")
	}
}
