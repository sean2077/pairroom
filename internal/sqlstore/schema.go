package sqlstore

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schemaSQL string

// qualified renders a schema-qualified application table name. The schema
// name is validated by schemaNamePattern before it is ever interpolated, and
// quoting keeps it a plain identifier.
func qualified(schema, name string) string { return `"` + schema + `".` + name }

// table renders this Store's schema-qualified application table name.
func (s *Store) table(name string) string { return qualified(s.cfg.Schema, name) }

// ApplySchema pins the connection to the given schema and applies the schema
// when it is absent; an existing schema at another version is refused. The
// whole creation runs in one transaction.
func ApplySchema(ctx context.Context, conn *pgx.Conn, schema string) error {
	if !schemaNamePattern.MatchString(schema) {
		return fmt.Errorf("sqlstore: invalid schema name %q", schema)
	}
	if err := pinSearchPath(ctx, conn, schema); err != nil {
		return err
	}
	var existing *string
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1)::text`, qualified(schema, "schema_meta")).Scan(&existing); err != nil {
		return fmt.Errorf("sqlstore: inspect schema: %w", err)
	}
	if existing != nil {
		return verifySchemaVersion(ctx, conn, schema)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, statement := range splitStatements(schemaSQL) {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("sqlstore: apply schema: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+qualified(schema, "schema_meta")+` (version) VALUES ($1)`, sqlstoreSchemaVersion); err != nil {
		return fmt.Errorf("sqlstore: record schema version: %w", err)
	}
	return tx.Commit(ctx)
}

// verifySchemaVersion refuses an absent schema or a version this build does
// not implement; there is no in-place migration.
func verifySchemaVersion(ctx context.Context, q rowQuerier, schema string) error {
	var existing *string
	if err := q.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1)::text`, qualified(schema, "schema_meta")).Scan(&existing); err != nil {
		return fmt.Errorf("sqlstore: inspect schema: %w", err)
	}
	if existing == nil {
		return fmt.Errorf("sqlstore: schema is not applied; run ApplySchema")
	}
	var version int
	if err := q.QueryRow(ctx, `SELECT version FROM `+qualified(schema, "schema_meta")).Scan(&version); err != nil {
		return fmt.Errorf("sqlstore: schema metadata unreadable: %w", err)
	}
	if version != sqlstoreSchemaVersion {
		return fmt.Errorf("sqlstore: schema version %d is not supported by this build (implements %d)", version, sqlstoreSchemaVersion)
	}
	return nil
}

// verifyDeployment refuses a bound deployment that differs from the
// configuration; one Store serves exactly one immutable deployment.
func verifyDeployment(ctx context.Context, q rowQuerier, schema, want string) error {
	var got string
	if err := q.QueryRow(ctx, `SELECT deployment_id FROM `+qualified(schema, "deployment")).Scan(&got); err != nil {
		return fmt.Errorf("sqlstore: deployment row unreadable: %w", err)
	}
	if got != want {
		return fmt.Errorf("sqlstore: bound deployment %q does not match the configured %q", got, want)
	}
	return nil
}

func pinSearchPath(ctx context.Context, conn *pgx.Conn, schema string) error {
	if !schemaNamePattern.MatchString(schema) {
		return fmt.Errorf("sqlstore: invalid schema name %q", schema)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_catalog.set_config('search_path', $1, false)`, schema+", pg_catalog"); err != nil {
		return fmt.Errorf("sqlstore: pin search_path: %w", err)
	}
	return nil
}

// splitStatements strips line comments, then splits the embedded DDL on
// semicolons. The DDL must not contain a literal "--" (it has none).
func splitStatements(sql string) []string {
	var uncommented strings.Builder
	for _, line := range strings.Split(sql, "\n") {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		uncommented.WriteString(line)
		uncommented.WriteString("\n")
	}
	var statements []string
	for _, chunk := range strings.Split(uncommented.String(), ";") {
		trimmed := strings.TrimSpace(chunk)
		if trimmed != "" {
			statements = append(statements, trimmed)
		}
	}
	return statements
}
