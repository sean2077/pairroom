package relay_test

import (
	"context"
	"testing"

	"github.com/sean2077/pairroom/internal/sqlstore/sqltest"
)

// The enterprise control model has two fixture seeds: sqltest.SeedRoom (used
// by every relay-side PostgreSQL test) and the sqlstore package's own in-package
// fixture. sqlstore's tests cannot import sqltest (sqltest imports sqlstore),
// so the two seeds cannot share code; they are held together by this drift
// guard. Both sides assert the identical canonical literal, so whichever seed
// drifts from the documented control state fails on its own side.
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

// TestControlFixtureDriftGuard pins the seeded control state.
func TestControlFixtureDriftGuard(t *testing.T) {
	dsn := sqltest.RequireDSN(t)
	schema, _ := sqltest.CreateSchema(t, dsn)
	admin := sqltest.SeedRoom(t, dsn, schema)

	rows, err := admin.Query(context.Background(), `
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
	if !equalStrings(snapshot, canonicalControlRows) {
		t.Fatalf("sqltest.SeedRoom drifted from the canonical control rows:\n%v\nwant\n%v", snapshot, canonicalControlRows)
	}
}
