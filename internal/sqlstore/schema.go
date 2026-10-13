package sqlstore

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed schema.sql
var schemaSQL string

const (
	// Server-side session budgets. They bound a wedged backend on the server
	// even when this process cannot cancel anything: an idle in-transaction
	// backend is closed, lock waits and single statements time out, and no
	// transaction can outlive transaction_timeout (PostgreSQL 17). Each is
	// shorter than the Go-side operation budget so the server is the outer
	// bound and a stalled process can never pin ownership or Room locks.
	sessionIdleInTransactionTimeout = "15s"
	sessionTransactionTimeout       = "55s"
	sessionLockTimeout              = "10s"
	sessionStatementTimeout         = "30s"
)

// cleanupCtx is the one bounded context every rollback and connection close
// uses; cleanup must never wait on a cancelled caller context or on the
// network without a deadline.
func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// qualified renders a schema-qualified application table name. The schema
// name is validated by schemaNamePattern before it is ever interpolated, and
// quoting keeps it a plain identifier.
func qualified(schema, name string) string { return `"` + schema + `".` + name }

// table renders this Store's schema-qualified application table name.
func (s *Store) table(name string) string { return qualified(s.cfg.Schema, name) }

// configureSession pins the search path and installs the bounded server-side
// session budgets on a connection. Pinning search_path is defense in depth —
// application tables are addressed schema-qualified regardless.
func configureSession(ctx context.Context, conn *pgx.Conn, schema string) error {
	if !schemaNamePattern.MatchString(schema) {
		return fmt.Errorf("sqlstore: invalid schema name %q", schema)
	}
	settings := [][2]string{
		{"search_path", schema + ", pg_catalog"},
		{"idle_in_transaction_session_timeout", sessionIdleInTransactionTimeout},
		{"transaction_timeout", sessionTransactionTimeout},
		{"lock_timeout", sessionLockTimeout},
		{"statement_timeout", sessionStatementTimeout},
	}
	for _, setting := range settings {
		if _, err := conn.Exec(ctx, `SELECT pg_catalog.set_config($1, $2, false)`, setting[0], setting[1]); err != nil {
			return fmt.Errorf("sqlstore: configure session %s: %w", setting[0], err)
		}
	}
	return nil
}

// ApplySchema pins the connection to the given schema and applies the schema
// when it is absent; an existing schema at another version is refused. The
// whole creation runs in one bounded transaction.
func ApplySchema(ctx context.Context, conn *pgx.Conn, schema string) error {
	ctx, cancel := context.WithTimeout(ctx, schemaBudget)
	defer cancel()
	if !schemaNamePattern.MatchString(schema) {
		return fmt.Errorf("sqlstore: invalid schema name %q", schema)
	}
	if err := configureSession(ctx, conn, schema); err != nil {
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
	defer func() {
		cctx, ccancel := cleanupCtx()
		defer ccancel()
		_ = tx.Rollback(cctx)
	}()
	if err := applySchemaInTransaction(ctx, tx, schema); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// applySchemaInTransaction lets explicit deployment initialization include
// namespace, schema and immutable anchors in one transaction. It contains no
// lifecycle or commit policy: callers own the connection, budget and commit.
// Existing schemas are checked, never silently migrated in place.
func applySchemaInTransaction(ctx context.Context, tx pgx.Tx, schema string) error {
	var existing *string
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1)::text`, qualified(schema, "schema_meta")).Scan(&existing); err != nil {
		return fmt.Errorf("sqlstore: inspect schema: %w", err)
	}
	if existing != nil {
		return verifySchemaVersion(ctx, tx, schema)
	}
	// The embedded DDL is static and parameterless, so it is sent as one
	// simple-protocol query: a statement splitter would break the
	// dollar-quoted trigger function bodies (internal semicolons and "--"
	// text inside them are data, not separators).
	if _, err := tx.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("sqlstore: apply schema: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+qualified(schema, "schema_meta")+` (version) VALUES ($1)`, sqlstoreSchemaVersion); err != nil {
		return fmt.Errorf("sqlstore: record schema version: %w", err)
	}
	return nil
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
	var version, codec, objects int
	if err := q.QueryRow(ctx, `SELECT version,fact_codec_version,object_schema_version FROM `+qualified(schema, "schema_meta")).Scan(&version, &codec, &objects); err != nil {
		return fmt.Errorf("sqlstore: schema metadata unreadable: %w", err)
	}
	if version != sqlstoreSchemaVersion || codec != factCodecVersion || objects != 1 {
		return fmt.Errorf("sqlstore: schema version %d is not supported by this build (implements %d)", version, sqlstoreSchemaVersion)
	}
	var shape bool
	if err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='room_events' AND ((column_name='origin_kind' AND is_nullable='NO' AND data_type='text') OR (column_name='origin_user' AND is_nullable='YES' AND data_type='text')))=2 AND pg_catalog.to_regclass($2) IS NOT NULL AND pg_catalog.to_regclass($3) IS NOT NULL`, schema, qualified(schema, "startup_room_recoveries"), qualified(schema, "startup_recovery_runs")).Scan(&shape); err != nil || !shape {
		return fmt.Errorf("sqlstore: unsupported provenance/recovery schema shape")
	}
	for _, table := range []string{"attachments", "attachment_operations", "attachment_references", "attachment_usage", "attachment_backup_barriers", "restore_runs"} {
		var present bool
		if err := q.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1) IS NOT NULL`, qualified(schema, table)).Scan(&present); err != nil || !present {
			return fmt.Errorf("sqlstore: unsupported enterprise object schema shape")
		}
	}
	var restoreShape bool
	if err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='attachments' AND column_name='restore_epoch' AND is_nullable='NO' AND data_type='text')=1 AND (SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='governance_actions' AND column_name='invalidated_at' AND data_type='timestamp with time zone')=1`, schema).Scan(&restoreShape); err != nil || !restoreShape {
		return fmt.Errorf("sqlstore: unsupported restore authority schema shape")
	}
	var auditShape bool
	if err := q.QueryRow(ctx, `SELECT count(*)=3 FROM information_schema.columns WHERE table_schema=$1 AND table_name='audit' AND is_nullable='YES' AND column_default IS NULL AND ((column_name IN ('target_id','operation_id') AND data_type='text') OR (column_name='invalidated_approval_count' AND data_type='bigint'))`, schema).Scan(&auditShape); err != nil || !auditShape {
		return fmt.Errorf("sqlstore: unsupported mutation audit schema shape")
	}
	var governanceShape bool
	if err := q.QueryRow(ctx, `SELECT count(*)=1 FROM information_schema.columns WHERE table_schema=$1 AND table_name='governance_actions' AND column_name='reason' AND is_nullable='NO' AND data_type='text' AND column_default IS NULL`, schema).Scan(&governanceShape); err != nil || !governanceShape {
		return fmt.Errorf("sqlstore: unsupported governance reason schema shape")
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

// schemaBudget bounds the whole schema application, which is slower than an
// ordinary operation.
const schemaBudget = 60 * time.Second
