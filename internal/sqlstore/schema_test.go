package sqlstore

import (
	"context"
	"strings"
	"testing"
)

func TestSchemaApplyIsIdempotentAndVersionGated(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	schema, _ := createTestSchema(t, dsn)
	conn := schemaConn(t, dsn, schema)

	if err := ApplySchema(ctx, conn, schema); err != nil {
		t.Fatal(err)
	}
	if err := ApplySchema(ctx, conn, schema); err != nil {
		t.Fatalf("second apply of the same schema version failed: %v", err)
	}
	if _, err := conn.Exec(ctx, `UPDATE schema_meta SET version = 99`); err != nil {
		t.Fatal(err)
	}
	if err := ApplySchema(ctx, conn, schema); err == nil {
		t.Fatal("a future schema version was accepted")
	}
}

func TestOpenRefusesUnappliedSchemaAndWrongDeployment(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	schema, _ := createTestSchema(t, dsn)
	cfg := Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"}

	if _, err := Open(ctx, cfg); err == nil || !strings.Contains(err.Error(), "not applied") {
		t.Fatalf("unapplied schema: %v", err)
	}

	conn := schemaConn(t, dsn, schema)
	if err := ApplySchema(ctx, conn, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO organizations (org_id) VALUES ('org-test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO configured_org (org_id) VALUES ('org-test')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO deployment (deployment_id, org_id) VALUES ('dep-test', 'org-test')`); err != nil {
		t.Fatal(err)
	}
	wrong := cfg
	wrong.DeploymentID = "dep-other"
	if _, err := Open(ctx, wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong deployment: %v", err)
	}
	if store, err := Open(ctx, cfg); err != nil {
		t.Fatalf("matching deployment refused: %v", err)
	} else if err := store.Close(); err != nil {
		t.Fatalf("close matching store: %v", err)
	}
}
