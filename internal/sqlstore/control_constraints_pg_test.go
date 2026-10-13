package sqlstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file is the S4-A1 DB-layer evidence for the enterprise control model:
// real PostgreSQL, raw SQL, no application command path, no authorization
// claim. Every assertion here proves a *database* constraint — a foreign key,
// a uniqueness rule, an immutability guard or a deferred consistency check.
// Application-level authorization (who may do what) is not implemented in this
// slice and is not evidenced here.
//
// SQLSTATE families used below: 23503 foreign_key_violation, 23505
// unique_violation, 23514 check_violation, 23502 not_null_violation.

const (
	sqlstateForeignKey = "23503"
	sqlstateUnique     = "23505"
	sqlstateCheck      = "23514"
)

// expectViolation asserts the exact SQLSTATE and constraint name a statement
// must fail with; a silent success is a failure of the guard under test.
func expectViolation(t *testing.T, err error, sqlstate, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s on %s, the statement succeeded", sqlstate, constraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error (%s on %s), got %v", sqlstate, constraint, err)
	}
	if pgErr.Code != sqlstate || pgErr.ConstraintName != constraint {
		t.Fatalf("expected %s on %s, got %s on %q: %v", sqlstate, constraint, pgErr.Code, pgErr.ConstraintName, err)
	}
}

// mustExec applies one statement that is expected to succeed.
func mustExec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("seed %q: %v", strings.SplitN(sql, "\n", 2)[0], err)
	}
}

// seedControlGraph inserts the legal control rows most tests below build on:
// two devices (one per user), one agent per device carrying the same opaque
// source key, and a trust connection with one exact identity mapping.
func seedControlGraph(t *testing.T, f *fixture) {
	t.Helper()
	for _, statement := range []string{
		`INSERT INTO trust_connections (connection_id, org_id, issuer) VALUES ('conn-1', 'org-test', 'https://idp.example/realms/one')`,
		`INSERT INTO external_identities (identity_id, org_id, connection_id, issuer, subject, user_id) VALUES ('id-1', 'org-test', 'conn-1', 'https://idp.example/realms/one', 'subject-1', 'u-owner')`,
		`INSERT INTO devices (device_id, org_id, user_id) VALUES ('dev-owner', 'org-test', 'u-owner'), ('dev-viewer', 'org-test', 'u-viewer')`,
		`INSERT INTO agents (agent_id, org_id, device_id, stable_source_key) VALUES ('agent-owner', 'org-test', 'dev-owner', 'opaque-key-1'), ('agent-viewer', 'org-test', 'dev-viewer', 'opaque-key-1')`,
	} {
		mustExec(t, f.admin, statement)
	}
}

// controlRows is the normalized snapshot of the seeded control state; the
// relay-side fixture drift guard asserts the identical list, so either seed
// drifting from this documented shape goes red.
func controlRows(t *testing.T, conn *pgx.Conn) []string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT 'org:' || org_id FROM organizations
		UNION ALL SELECT 'configured:' || org_id FROM configured_org
		UNION ALL SELECT 'deployment:' || deployment_id || '/' || org_id FROM deployment
		UNION ALL SELECT 'user:' || user_id || '/' || org_id || '/' || disabled::text FROM users
		UNION ALL SELECT 'room:' || room_id || '/' || org_id || '/' || owner_user_id || '/' || host_mode FROM rooms
		UNION ALL SELECT 'member:' || room_id || '/' || user_id || '/' || role || '/' || disabled::text FROM room_members
		UNION ALL SELECT 'generation:' || room_id || '/' || slot || '/' || generation::text FROM slot_generations
		UNION ALL SELECT 'slot:' || room_id || '/' || slot || '/' || current_generation::text FROM room_slots
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var snapshot []string
	for rows.Next() {
		var entry string
		if err := rows.Scan(&entry); err != nil {
			t.Fatal(err)
		}
		snapshot = append(snapshot, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// canonicalControlRows is the documented seed shape both fixtures must
// produce. The relay-side guard repeats this literal verbatim: sqlstore's
// in-package tests cannot import sqltest (sqltest imports sqlstore), so the
// shared literal plus one assertion on each side is the drift guard.
var canonicalControlRows = []string{
	"configured:org-test",
	"deployment:dep-test/org-test",
	"generation:room-1/slot1/1",
	"generation:room-1/slot2/1",
	"member:room-1/u-owner/owner/false",
	"member:room-1/u-viewer/viewer/false",
	"org:org-test",
	"room:room-1/org-test/u-owner/native",
	"slot:room-1/slot1/1",
	"slot:room-1/slot2/1",
	"user:u-owner/org-test/false",
	"user:u-viewer/org-test/false",
}

// DB-10: the configured organization anchor. Another organization may exist in
// the catalog, but a whole self-consistent resource graph for it is refused at
// its first row, because only the single configured organization is referable.
func TestControlOrgAnchorRejectsForeignOrgGraph(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()

	// The fixture's own shape is the documented canonical one.
	if got := controlRows(t, f.admin); !equalRowLists(got, canonicalControlRows) {
		t.Fatalf("fixture control rows drifted:\n%v\nwant\n%v", got, canonicalControlRows)
	}

	// The catalog itself is a directory and accepts a second organization.
	mustExec(t, f.admin, `INSERT INTO organizations (org_id) VALUES ('org-other')`)

	// A second configured organization is impossible: the anchor table holds
	// exactly one row and its primary key refuses another.
	_, err := f.admin.Exec(ctx, `INSERT INTO configured_org (org_id) VALUES ('org-other')`)
	expectViolation(t, err, sqlstateUnique, "configured_org_pkey")

	// Every resource row of the other organization is refused by that
	// organization's own anchor foreign key, not by a same-organization
	// reference: the graph never gets its first row.
	for _, probe := range []struct {
		sql        string
		constraint string
	}{
		{`INSERT INTO users (user_id, org_id) VALUES ('u-other', 'org-other')`, "users_org_anchor_fkey"},
		{`INSERT INTO trust_connections (connection_id, org_id, issuer) VALUES ('conn-other', 'org-other', 'https://idp.example/other')`, "trust_connections_org_anchor_fkey"},
		{`INSERT INTO projects (project_id, org_id) VALUES ('project-other', 'org-other')`, "projects_org_anchor_fkey"},
		{`INSERT INTO rooms (room_id, org_id, host_mode, owner_user_id) VALUES ('room-other', 'org-other', 'native', 'u-owner')`, "rooms_org_anchor_fkey"},
	} {
		_, err := f.admin.Exec(ctx, probe.sql)
		expectViolation(t, err, sqlstateForeignKey, probe.constraint)
	}

	// The same shape for the configured organization succeeds.
	seedControlGraph(t, f)
	secondRoom := []string{
		`INSERT INTO rooms (room_id, org_id, host_mode, owner_user_id) VALUES ('room-2', 'org-test', 'native', 'u-owner')`,
		`INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-2', 'org-test', 'u-owner', 'owner')`,
		`INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-2', 'slot1', 1), ('org-test', 'room-2', 'slot2', 1)`,
		`INSERT INTO room_slots (room_id, slot, current_generation) VALUES ('room-2', 'slot1', 1), ('room-2', 'slot2', 1)`,
		`INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-2', 'org-test', 'room-2', 'slot1', 1, 'agent-owner')`,
	}
	for _, statement := range secondRoom {
		mustExec(t, f.admin, statement)
	}
}

func equalRowLists(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DB-11: ownership references are structurally consistent. Cross-organization
// references are impossible (the anchor), and every relation inside the
// organization must name rows that exist for the same Room, slot and
// generation. Whether one member may bind *another member's* agent is an
// authorization rule owned by the reviewed application slice, not a database
// constraint, and is not claimed here.
func TestControlOwnershipReferences(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	seedControlGraph(t, f)

	// Legal association succeeds later in this test; the structural probes run
	// first so each probe violates exactly one rule.
	// A generation that does not exist is refused.
	_, err := f.admin.Exec(ctx, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-x', 'org-test', 'room-1', 'slot1', 7, 'agent-owner')`)
	expectViolation(t, err, sqlstateForeignKey, "bindings_generation_fkey")

	// Another slot's generation is refused: the reference is the exact tuple.
	_, err = f.admin.Exec(ctx, `INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-1', 'slot2', 2)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.admin.Exec(ctx, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-y', 'org-test', 'room-1', 'slot1', 2, 'agent-owner')`)
	expectViolation(t, err, sqlstateForeignKey, "bindings_generation_fkey")

	// A membership for a user that does not exist is refused.
	_, err = f.admin.Exec(ctx, `INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', 'u-missing', 'member')`)
	expectViolation(t, err, sqlstateForeignKey, "room_members_user_fkey")

	// A Room owner that is not a user is refused.
	_, err = f.admin.Exec(ctx, `INSERT INTO rooms (room_id, org_id, host_mode, owner_user_id) VALUES ('room-3', 'org-test', 'native', 'u-missing')`)
	expectViolation(t, err, sqlstateForeignKey, "rooms_owner_fkey")

	// An agent for a device that does not exist is refused.
	_, err = f.admin.Exec(ctx, `INSERT INTO agents (agent_id, org_id, device_id, stable_source_key) VALUES ('agent-x', 'org-test', 'dev-missing', 'key-x')`)
	expectViolation(t, err, sqlstateForeignKey, "agents_device_fkey")

	// The legal association succeeds.
	mustExec(t, f.admin, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-1', 'org-test', 'room-1', 'slot1', 1, 'agent-owner')`)
}

// DB-12: exact identity mapping and membership uniqueness, including two real
// concurrent inserts.
func TestControlIdentityUniqueness(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	seedControlGraph(t, f)

	// The same exact triple is refused.
	_, err := f.admin.Exec(ctx, `INSERT INTO external_identities (identity_id, org_id, connection_id, issuer, subject, user_id) VALUES ('id-2', 'org-test', 'conn-1', 'https://idp.example/realms/one', 'subject-1', 'u-viewer')`)
	expectViolation(t, err, sqlstateUnique, "external_identities_subject_key")

	// A forged issuer under an existing connection is refused by the exact
	// triple foreign key: a connection alone does not authorize an issuer.
	_, err = f.admin.Exec(ctx, `INSERT INTO external_identities (identity_id, org_id, connection_id, issuer, subject, user_id) VALUES ('id-3', 'org-test', 'conn-1', 'https://evil.example/', 'subject-1', 'u-viewer')`)
	expectViolation(t, err, sqlstateForeignKey, "external_identities_connection_fkey")

	// The same subject under a different connection is a different identity.
	mustExec(t, f.admin, `INSERT INTO trust_connections (connection_id, org_id, issuer) VALUES ('conn-2', 'org-test', 'https://idp.example/realms/two')`)
	mustExec(t, f.admin, `INSERT INTO external_identities (identity_id, org_id, connection_id, issuer, subject, user_id) VALUES ('id-4', 'org-test', 'conn-2', 'https://idp.example/realms/two', 'subject-1', 'u-viewer')`)

	// Concurrent duplicate membership and identity inserts: exactly one wins.
	t.Run("concurrent", func(t *testing.T) {
		first := controlConn(t, f.dsn, f.schema)
		second := controlConn(t, f.dsn, f.schema)
		// Both probes target rows that do not exist yet, so the first writer
		// succeeds and only the second one meets the constraint.
		mustExec(t, f.admin, `INSERT INTO users (user_id, org_id) VALUES ('u-dupe', 'org-test')`)
		probes := []struct {
			first      string
			second     string
			constraint string
		}{
			{
				`INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', 'u-dupe', 'member')`,
				`INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', 'u-dupe', 'member')`,
				"room_members_pkey",
			},
			{
				`INSERT INTO external_identities (identity_id, org_id, connection_id, issuer, subject, user_id) VALUES ('id-5', 'org-test', 'conn-1', 'https://idp.example/realms/one', 'subject-race', 'u-viewer')`,
				`INSERT INTO external_identities (identity_id, org_id, connection_id, issuer, subject, user_id) VALUES ('id-6', 'org-test', 'conn-1', 'https://idp.example/realms/one', 'subject-race', 'u-viewer')`,
				"external_identities_subject_key",
			},
		}
		for _, probe := range probes {
			tx1, err := first.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			tx2, err := second.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx1.Exec(ctx, probe.first); err != nil {
				t.Fatalf("first writer: %v", err)
			}
			// The second writer blocks on the first writer's uncommitted row;
			// the barrier is that real lock wait, not a sleep.
			blocked := make(chan error, 1)
			go func() {
				_, err := tx2.Exec(ctx, probe.second)
				blocked <- err
			}()
			waitForLockWait(t, f.admin, second.PgConn().PID())
			if err := tx1.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			expectViolation(t, <-blocked, sqlstateUnique, probe.constraint)
			_ = tx2.Rollback(ctx)
		}
	})
}

// controlConn opens a connection pinned to the test schema with bounded
// server-side timeouts, so a blocked writer can never hang the suite.
func controlConn(t *testing.T, dsn, schema string) *pgx.Conn {
	t.Helper()
	conn := schemaConn(t, dsn, schema)
	for _, setting := range []string{`SET lock_timeout = '10s'`, `SET statement_timeout = '30s'`} {
		mustExec(t, conn, setting)
	}
	return conn
}

// DB-13: one canonical agent per (device, opaque source key), one live
// reservation per agent across Rooms including archived ones, and retirement
// as the only release.
func TestControlAgentReservationAndArchive(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	seedControlGraph(t, f)

	// Two devices may carry the same opaque key; they are different agents.
	// (seedControlGraph already inserted both, and this proves the pair is
	// accepted rather than colliding.)
	// The same device may not register the same source key twice: a second
	// random agent ID cannot be minted for one stable source.
	_, err := f.admin.Exec(ctx, `INSERT INTO agents (agent_id, org_id, device_id, stable_source_key) VALUES ('agent-owner-2', 'org-test', 'dev-owner', 'opaque-key-1')`)
	expectViolation(t, err, sqlstateUnique, "agents_source_key")

	// No raw vendor session or path column exists at all.
	var columns []string
	rows, err := f.admin.Query(ctx, `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'agents' ORDER BY column_name`, f.schema)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, column)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, column := range columns {
		lowered := strings.ToLower(column)
		for _, forbidden := range []string{"session", "path", "cwd", "token", "credential"} {
			if strings.Contains(lowered, forbidden) {
				t.Fatalf("agents carries a forbidden column %q", column)
			}
		}
	}

	// One live reservation per agent, across Rooms: a second live binding for
	// the same agent is refused, and archiving the first Room does not release
	// it.
	mustExec(t, f.admin, `INSERT INTO rooms (room_id, org_id, host_mode, owner_user_id) VALUES ('room-2', 'org-test', 'native', 'u-owner')`)
	mustExec(t, f.admin, `INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-2', 'org-test', 'u-owner', 'owner')`)
	mustExec(t, f.admin, `INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-2', 'slot1', 1), ('org-test', 'room-2', 'slot2', 1)`)
	mustExec(t, f.admin, `INSERT INTO room_slots (room_id, slot, current_generation) VALUES ('room-2', 'slot1', 1), ('room-2', 'slot2', 1)`)
	mustExec(t, f.admin, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-1', 'org-test', 'room-1', 'slot1', 1, 'agent-owner')`)

	_, err = f.admin.Exec(ctx, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-2', 'org-test', 'room-2', 'slot1', 1, 'agent-owner')`)
	expectViolation(t, err, sqlstateUnique, "bindings_active_agent_key")

	mustExec(t, f.admin, `UPDATE rooms SET archived_at = now() WHERE room_id = 'room-1'`)
	_, err = f.admin.Exec(ctx, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-2', 'org-test', 'room-2', 'slot1', 1, 'agent-owner')`)
	expectViolation(t, err, sqlstateUnique, "bindings_active_agent_key")

	// A second live binding for the same slot is refused, at a generation that
	// is not already taken so exactly this rule is violated.
	mustExec(t, f.admin, `INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-1', 'slot1', 2)`)
	_, err = f.admin.Exec(ctx, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-3', 'org-test', 'room-1', 'slot1', 2, 'agent-viewer')`)
	expectViolation(t, err, sqlstateUnique, "bindings_active_slot_key")

	// A retired generation is never re-used, even by a different binding ID.
	_, err = f.admin.Exec(ctx, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-4', 'org-test', 'room-1', 'slot1', 1, 'agent-viewer')`)
	expectViolation(t, err, sqlstateUnique, "bindings_generation_key")

	// Retirement must state a reason, and is write-once.
	_, err = f.admin.Exec(ctx, `UPDATE bindings SET retired_at = now() WHERE binding_id = 'binding-1'`)
	expectViolation(t, err, sqlstateCheck, "bindings_retired_reason")
	mustExec(t, f.admin, `UPDATE bindings SET retired_at = now(), retired_reason = 'replaced' WHERE binding_id = 'binding-1'`)
	_, err = f.admin.Exec(ctx, `UPDATE bindings SET retired_at = now() + interval '1 second', retired_reason = 'again' WHERE binding_id = 'binding-1'`)
	expectViolation(t, err, sqlstateCheck, "bindings_retired_once")

	// Only the explicit retirement released the reservation: the same agent may
	// now hold a binding in the other Room.
	mustExec(t, f.admin, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-2', 'org-test', 'room-2', 'slot1', 1, 'agent-owner')`)

	// A retired binding is never relabeled.
	_, err = f.admin.Exec(ctx, `UPDATE bindings SET generation = 2 WHERE binding_id = 'binding-1'`)
	expectViolation(t, err, sqlstateCheck, "bindings_identity_immutable")
}

// The immutability and retention guards: identity columns are never relabeled
// in place, the configuration anchor and immutable tables never change, and
// identity rows are retained rather than deleted.
func TestControlImmutabilityAndRetention(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	seedControlGraph(t, f)
	mustExec(t, f.admin, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-1', 'org-test', 'room-1', 'slot1', 1, 'agent-owner')`)

	for _, probe := range []struct {
		sql        string
		sqlstate   string
		constraint string
	}{
		{`UPDATE configured_org SET org_id = 'org-other'`, sqlstateCheck, "configured_org_immutable"},
		{`DELETE FROM configured_org`, sqlstateCheck, "configured_org_immutable"},
		{`UPDATE deployment SET org_id = 'org-other'`, sqlstateCheck, "deployment_immutable"},
		{`DELETE FROM deployment`, sqlstateCheck, "deployment_immutable"},
		{`UPDATE slot_generations SET generation = 5`, sqlstateCheck, "slot_generations_immutable"},
		{`DELETE FROM slot_generations`, sqlstateCheck, "slot_generations_immutable"},
		{`UPDATE trust_connections SET issuer = 'https://idp.example/renamed'`, sqlstateCheck, "trust_connections_immutable"},
		{`UPDATE external_identities SET subject = 'other-subject'`, sqlstateCheck, "external_identities_identity_immutable"},
		{`UPDATE external_identities SET user_id = 'u-viewer'`, sqlstateCheck, "external_identities_identity_immutable"},
		{`UPDATE devices SET user_id = 'u-viewer' WHERE device_id = 'dev-owner'`, sqlstateCheck, "devices_identity_immutable"},
		{`UPDATE agents SET device_id = 'dev-viewer' WHERE agent_id = 'agent-owner'`, sqlstateCheck, "agents_identity_immutable"},
		{`UPDATE agents SET stable_source_key = 'other-key' WHERE agent_id = 'agent-owner'`, sqlstateCheck, "agents_identity_immutable"},
		{`UPDATE rooms SET host_mode = 'embedded'`, sqlstateCheck, "rooms_identity_immutable"},
		{`UPDATE room_slots SET slot = 'slot2' WHERE slot = 'slot1'`, sqlstateCheck, "room_slots_identity_immutable"},
		{`UPDATE room_slots SET current_generation = 3 WHERE room_id = 'room-1' AND slot = 'slot1'`, sqlstateCheck, "room_slots_head_check"},
		{`DELETE FROM users WHERE user_id = 'u-viewer'`, sqlstateCheck, "users_no_delete"},
		{`DELETE FROM devices WHERE device_id = 'dev-owner'`, sqlstateCheck, "devices_no_delete"},
		{`DELETE FROM agents WHERE agent_id = 'agent-owner'`, sqlstateCheck, "agents_no_delete"},
		{`DELETE FROM external_identities WHERE identity_id = 'id-1'`, sqlstateCheck, "external_identities_no_delete"},
		{`UPDATE users SET tombstoned_at = now() WHERE user_id = 'u-viewer'`, sqlstateCheck, "users_tombstone_check"},
	} {
		_, err := f.admin.Exec(ctx, probe.sql)
		expectViolation(t, err, probe.sqlstate, probe.constraint)
	}

	// A tombstone always carries the disabled flag, so the kernel's existing
	// `disabled` check stays fail-closed for it.
	mustExec(t, f.admin, `UPDATE users SET disabled = true, tombstoned_at = now() WHERE user_id = 'u-viewer'`)
	var disabled bool
	if err := f.admin.QueryRow(ctx, `SELECT disabled FROM users WHERE user_id = 'u-viewer'`).Scan(&disabled); err != nil || !disabled {
		t.Fatalf("tombstoned user is not disabled: %v %v", disabled, err)
	}
}

// The deferred consistency invariants, both ends of each relation, with a
// legal single-transaction replace and transfer and an illegal committed
// state.
func TestControlDeferredInvariants(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	seedControlGraph(t, f)
	mustExec(t, f.admin, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-1', 'org-test', 'room-1', 'slot1', 1, 'agent-owner')`)

	// A legal replacement commits as one transaction: retire, append the next
	// generation, advance the head, insert the new binding. The intermediate
	// state is never mistaken for a violation.
	tx, err := f.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE bindings SET retired_at = now(), retired_reason = 'replaced' WHERE binding_id = 'binding-1'`,
		`INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-1', 'slot1', 2)`,
		`INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-2', 'org-test', 'room-1', 'slot1', 2, 'agent-viewer')`,
		`UPDATE room_slots SET current_generation = 2 WHERE room_id = 'room-1' AND slot = 'slot1'`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("legal replace step: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("legal same-transaction replace was refused: %v", err)
	}

	// A live binding whose generation is not the head is refused at commit. The
	// slot is left without a live binding first, so only the deferred check can
	// object.
	mustExec(t, f.admin, `UPDATE bindings SET retired_at = now(), retired_reason = 'superseded' WHERE binding_id = 'binding-2'`)
	_, err = f.admin.Exec(ctx, `INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-1', 'slot1', 3)`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.admin.Exec(ctx, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-3', 'org-test', 'room-1', 'slot1', 3, 'agent-owner')`)
	expectViolation(t, err, sqlstateCheck, "bindings_active_generation_check")

	// The durable owner record is authoritative: the partial index refuses a
	// second live owner-role membership outright.
	mustExec(t, f.admin, `INSERT INTO users (user_id, org_id) VALUES ('u-dupe2', 'org-test')`)
	_, err = f.admin.Exec(ctx, `INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', 'u-dupe2', 'owner')`)
	expectViolation(t, err, sqlstateUnique, "room_members_owner_key")

	// A legal transfer commits as one transaction and keeps both ends aligned.
	tx, err = f.admin.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE rooms SET owner_user_id = 'u-viewer' WHERE room_id = 'room-1'`,
		`UPDATE room_members SET role = 'member' WHERE room_id = 'room-1' AND user_id = 'u-owner'`,
		`UPDATE room_members SET role = 'owner' WHERE room_id = 'room-1' AND user_id = 'u-viewer'`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("legal transfer step: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("legal same-transaction transfer was refused: %v", err)
	}
	var owner string
	if err := f.admin.QueryRow(ctx, `SELECT owner_user_id FROM rooms WHERE room_id = 'room-1'`).Scan(&owner); err != nil || owner != "u-viewer" {
		t.Fatalf("owner record after transfer: %q %v", owner, err)
	}

	// An owner who loses the owner-role membership keeps the durable record.
	mustExec(t, f.admin, `DELETE FROM room_members WHERE room_id = 'room-1' AND user_id = 'u-viewer'`)
	if err := f.admin.QueryRow(ctx, `SELECT owner_user_id FROM rooms WHERE room_id = 'room-1'`).Scan(&owner); err != nil || owner != "u-viewer" {
		t.Fatalf("owner record did not survive the membership removal: %q %v", owner, err)
	}

	// An owner-role membership for anyone else contradicts the durable record and
	// is refused by the deferred check at commit.
	mustExec(t, f.admin, `INSERT INTO users (user_id, org_id) VALUES ('u-dupe3', 'org-test')`)
	_, err = f.admin.Exec(ctx, `INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', 'u-dupe3', 'owner')`)
	expectViolation(t, err, sqlstateCheck, "owner_record_consistency_check")

	// The check covers the Rooms end too: while a matching owner-role member
	// exists, moving the durable record away fails at commit.
	mustExec(t, f.admin, `INSERT INTO room_members (room_id, org_id, user_id, role) VALUES ('room-1', 'org-test', 'u-viewer', 'owner')`)
	_, err = f.admin.Exec(ctx, `UPDATE rooms SET owner_user_id = 'u-owner' WHERE room_id = 'room-1'`)
	expectViolation(t, err, sqlstateCheck, "owner_record_consistency_check")
}

// The deferred check must also hold under two real concurrent transactions: a
// live binding and a head advance cannot both commit, and the surviving state
// is consistent.
func TestControlConcurrentReplaceWriteSkew(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	seedControlGraph(t, f)

	first := schemaConn(t, f.dsn, f.schema)
	second := schemaConn(t, f.dsn, f.schema)

	tx1, err := first.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx2, err := second.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Exec(ctx, `INSERT INTO bindings (binding_id, org_id, room_id, slot, generation, agent_id) VALUES ('binding-1', 'org-test', 'room-1', 'slot1', 1, 'agent-owner')`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `INSERT INTO slot_generations (org_id, room_id, slot, generation) VALUES ('org-test', 'room-1', 'slot1', 2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx2.Exec(ctx, `UPDATE room_slots SET current_generation = 2 WHERE room_id = 'room-1' AND slot = 'slot1'`); err != nil {
		t.Fatal(err)
	}

	var ready, done sync.WaitGroup
	ready.Add(2)
	done.Add(2)
	start := make(chan struct{})
	results := make([]error, 2)
	for idx, tx := range []pgx.Tx{tx1, tx2} {
		go func(idx int, tx pgx.Tx) {
			defer done.Done()
			ready.Done()
			<-start
			results[idx] = tx.Commit(ctx)
		}(idx, tx)
	}
	ready.Wait()
	close(start)
	done.Wait()

	successes := 0
	for _, err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent head advance and live binding: %d transactions committed (%v), want exactly one", successes, results)
	}
	var skew int64
	if err := f.admin.QueryRow(ctx, `
		SELECT count(*) FROM bindings b JOIN room_slots s ON s.room_id = b.room_id AND s.slot = b.slot
		WHERE b.retired_at IS NULL AND b.generation <> s.current_generation`).Scan(&skew); err != nil {
		t.Fatal(err)
	}
	if skew != 0 {
		t.Fatalf("write skew survived: %d live bindings disagree with their head", skew)
	}
}

// The schema loader carries dollar-quoted function bodies (internal
// semicolons and comment text) and rolls the whole batch back when any
// statement fails.
func TestControlSchemaLoadAndRollback(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	t.Run("batch rolls back whole", func(t *testing.T) {
		schema, bootstrap := createTestSchema(t, dsn)
		conn := schemaConn(t, dsn, schema)
		// A conflicting object makes a late statement of the batch fail.
		mustExec(t, conn, `CREATE TABLE bindings (marker text)`)
		if err := ApplySchema(ctx, conn, schema); err == nil {
			t.Fatal("a conflicting schema was applied")
		}
		var present *string
		if err := bootstrap.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1)::text`, schema+".schema_meta").Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present != nil {
			t.Fatal("a failed batch left schema_meta behind")
		}
		if err := bootstrap.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1)::text`, schema+".organizations").Scan(&present); err != nil {
			t.Fatal(err)
		}
		if present != nil {
			t.Fatal("a failed batch left a partial control table behind")
		}
		var marker *string
		if err := conn.QueryRow(ctx, `SELECT pg_catalog.to_regclass('bindings')::text`).Scan(&marker); err != nil || marker == nil {
			t.Fatalf("the pre-existing object did not survive: %v %v", marker, err)
		}
	})

	t.Run("functions keep their bodies", func(t *testing.T) {
		f := newRawFixture(t)
		var position int
		if err := f.admin.QueryRow(ctx, `
			SELECT position('locked first' in pg_get_functiondef($1::regproc))`,
			f.schema+".bindings_active_generation_check").Scan(&position); err != nil {
			t.Fatal(err)
		}
		if position == 0 {
			t.Fatal("the trigger function body lost its comment text")
		}
		var configured int
		if err := f.admin.QueryRow(ctx, `
			SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = $1 AND p.proconfig @> ARRAY['search_path=pg_catalog']`, f.schema).Scan(&configured); err != nil {
			t.Fatal(err)
		}
		if configured != 7 {
			t.Fatalf("trigger functions without a pinned search_path: %d of 7", configured)
		}
		var deferrable int
		if err := f.admin.QueryRow(ctx, `
			SELECT count(*) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = $1 AND t.tgdeferrable AND NOT t.tgisinternal`, f.schema).Scan(&deferrable); err != nil {
			t.Fatal(err)
		}
		if deferrable != 4 {
			t.Fatalf("deferrable constraint triggers: %d of 4", deferrable)
		}
	})
}

// Version 1 is refused by this build, and an existing schema is never evolved
// in place.
func TestControlSchemaVersionOneRefused(t *testing.T) {
	f := newRawFixture(t)
	ctx := context.Background()
	mustExec(t, f.admin, `UPDATE schema_meta SET version = 1`)
	if _, err := Open(ctx, f.cfg); err == nil || !strings.Contains(err.Error(), "not supported by this build") {
		t.Fatalf("a version 1 schema was accepted: %v", err)
	}
	if err := ApplySchema(ctx, f.admin, f.schema); err == nil || !strings.Contains(err.Error(), "not supported by this build") {
		t.Fatalf("ApplySchema accepted a version 1 schema: %v", err)
	}
}
