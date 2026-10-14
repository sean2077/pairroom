package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sean2077/pairroom/internal/model"
)

// testDSN requires an isolated local PostgreSQL. A missing DSN is an explicit
// skip, and a skip is not a pass: the contract suite must actually run.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("PAIRROOM_SQLSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("PAIRROOM_SQLSTORE_TEST_DSN is not set: the contract tests need an isolated PostgreSQL DSN; a skip here is not a pass")
	}
	return dsn
}

// createTestSchema creates a uniquely named schema and tears it down after the
// test; the kernel never reuses the public schema.
func createTestSchema(t *testing.T, dsn string) (string, *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	schema := fmt.Sprintf("sqlstore_test_%d", time.Now().UnixNano())
	bootstrap, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bootstrap.Close(context.Background()) })
	if _, err := bootstrap.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dropCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := bootstrap.Exec(dropCtx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})
	return schema, bootstrap
}

// schemaConn opens a connection pinned to the test schema for fixtures and
// assertions.
func schemaConn(t *testing.T, dsn, schema string) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `SELECT set_config('search_path', $1, false)`, schema+", pg_catalog"); err != nil {
		t.Fatal(err)
	}
	return conn
}

type fixture struct {
	store  *Store
	cfg    Config
	dsn    string
	schema string
	admin  *pgx.Conn
}

// newRawFixture creates a uniquely schemed, seeded fixture without opening a
// Store; tests that compose their own Store over these rows (driver tracers)
// use it.
func newRawFixture(t *testing.T) *fixture {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()
	schema, _ := createTestSchema(t, dsn)
	conn := schemaConn(t, dsn, schema)
	if err := ApplySchema(ctx, conn, schema); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []string{
		`INSERT INTO organizations (org_id) VALUES ('org-test')`,
		`INSERT INTO configured_org (org_id) VALUES ('org-test')`,
		`INSERT INTO deployment (deployment_id, org_id) VALUES ('dep-test', 'org-test')`,
		`INSERT INTO users (user_id, org_id) VALUES ('u-owner', 'org-test'), ('u-viewer', 'org-test')`,
		`INSERT INTO rooms (room_id, org_id, host_mode, owner_user_id) VALUES ('room-1', 'org-test', 'native', 'u-owner')`,
		`INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', 'u-owner', 'owner'), ('room-1', 'org-test', 'u-viewer', 'viewer')`,
		`INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-1', 'slot1', 1), ('org-test', 'room-1', 'slot2', 1)`,
		`INSERT INTO room_slots (room_id, slot, current_generation) VALUES ('room-1', 'slot1', 1), ('room-1', 'slot2', 1)`,
	} {
		if _, err := conn.Exec(ctx, seed); err != nil {
			t.Fatal(err)
		}
	}
	return &fixture{
		cfg:    Config{DSN: dsn, Schema: schema, DeploymentID: "dep-test"},
		dsn:    dsn,
		schema: schema,
		admin:  conn,
	}
}

// newFixture seeds a fixture and opens its Store on the direct DSN.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	return openFixture(t, nil)
}

// newFixtureProxy opens the fixture Store through a transformed DSN (the
// protocol proxy) while fixtures and assertions stay on the direct connection.
func newFixtureProxy(t *testing.T, transform func(string) string) *fixture {
	t.Helper()
	return openFixture(t, transform)
}

func openFixture(t *testing.T, transform func(string) string) *fixture {
	t.Helper()
	f := newRawFixture(t)
	cfg := f.cfg
	if transform != nil {
		cfg.DSN = transform(cfg.DSN)
	}
	store, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	t.Cleanup(func() { _ = store.Close() })
	return f
}

// waitForLockWait is a real database barrier: it polls pg_stat_activity until
// the given backend is waiting on a lock, so tests synchronize on observable
// database state instead of sleeps.
func waitForLockWait(t *testing.T, admin *pgx.Conn, pid uint32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waitType *string
		if err := admin.QueryRow(context.Background(), `SELECT wait_event_type FROM pg_catalog.pg_stat_activity WHERE pid = $1`, int32(pid)).Scan(&waitType); err != nil {
			t.Fatal(err)
		}
		if waitType != nil && *waitType == "Lock" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("backend never reached a lock wait")
}

// command builds a valid append_fact command for the fixture Room.
func (f *fixture) command(identifier string) Command {
	return Command{
		Auth: AuthContext{Deployment: "dep-test", UserID: "u-owner"},
		Key: OpKey{
			Namespace:  namespaceHuman,
			Deployment: "dep-test",
			RoomID:     "room-1",
			OpKind:     opKindAppendFact,
			Identifier: identifier,
			Principal:  "u-owner",
		},
		Mutation: Mutation{
			Version: mutationVersionV1,
			Fact: model.Event{
				ID:        "evt-" + identifier,
				RoomID:    "room-1",
				Kind:      "room.note",
				Actor:     model.ActorUser,
				CreatedAt: time.Now().UTC(),
				Data:      []byte(`{"text":"note ` + identifier + `"}`),
			},
			Audit: AuditRecord{Kind: "fact.append", Outcome: "accepted"},
		},
	}
}

func (f *fixture) roomCounts(t *testing.T) (events, operations, audits, currentSeq, revision int64) {
	t.Helper()
	err := f.admin.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM room_events),
		       (SELECT count(*) FROM operations),
		       (SELECT count(*) FROM audit),
		       current_seq, revision
		  FROM rooms WHERE room_id = 'room-1'`).Scan(&events, &operations, &audits, &currentSeq, &revision)
	if err != nil {
		t.Fatal(err)
	}
	return events, operations, audits, currentSeq, revision
}

// terminate kills a backend with a confirmable wait and reports when the
// signal could not be confirmed.
func (f *fixture) terminate(t *testing.T, pid uint32, what string) {
	t.Helper()
	var ok bool
	if err := f.admin.QueryRow(context.Background(), `SELECT pg_terminate_backend($1, 5000)`, int32(pid)).Scan(&ok); err != nil {
		t.Fatalf("terminate %s backend: %v", what, err)
	}
	if !ok {
		t.Fatalf("terminate %s backend: not confirmed", what)
	}
}

// kernelErr asserts the error form matches the result's outcome and class.
func kernelErr(t *testing.T, res Result, err error) *KernelError {
	t.Helper()
	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	var kerr *KernelError
	if !errors.As(err, &kerr) {
		t.Fatalf("error is not a KernelError: %v", err)
	}
	if kerr.Outcome != res.Outcome || kerr.Class != res.Class {
		t.Fatalf("result/error mismatch: %+v vs %v", res, err)
	}
	return kerr
}
