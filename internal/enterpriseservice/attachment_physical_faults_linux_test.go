package enterpriseservice

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sean2077/pairroom/internal/enterpriseobject"
)

func TestAttachmentHTTPPhysicalENOSPCRetainsReservationUntilExplicitGC(t *testing.T) {
	base := os.Getenv("PAIRROOM_OBJECT_FAULT_ROOT")
	if base == "" {
		t.Skip("PAIRROOM_OBJECT_FAULT_ROOT requires a real isolated bounded tmpfs; skip is not physical fault proof")
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(base, &fs); err != nil || fs.Type != 0x01021994 || uint64(fs.Bsize)*fs.Blocks > 256<<10 {
		t.Fatal("physical HTTP fault requires an actual tmpfs no larger than 256 KiB", err)
	}
	root, err := os.MkdirTemp(base, "http-enospc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	f := newObjectHTTPFixture(t)
	objects := attachmentFaultObjectRoot(t, f, root)
	client := f.browser("member-one")
	f.login(client)
	session := f.session(client)
	const room = "physical-enospc-room"
	f.createRoom(client, session.CSRF, room)
	data := bytes.Repeat([]byte("s"), 256<<10)
	reserved := objectHTTPReserve(t, f, client, room, "enospc-reserve", data, "", session.CSRF)
	before := attachmentFaultState(t, f, room, reserved.ObjectID)
	if before.bytes != int64(len(data)) || before.pending != 1 || before.state != "reserved" {
		t.Fatal("physical failure fixture did not reserve original bytes/pending quota")
	}
	path := "/enterprise/api/v1/rooms/" + room + "/attachments/" + reserved.ObjectID
	status, body, _, err := objectHTTPRaw(t, f, client, http.MethodPut, path+"/content", data, "", session.CSRF, nil)
	if err != nil || status != http.StatusServiceUnavailable || !bytes.Contains(body, []byte(`"error":"object_unavailable"`)) {
		t.Fatalf("physical full filesystem upload did not fail conservatively: %d %v %s", status, err, body)
	}
	for _, name := range []string{"data", "manifest.json"} {
		if _, err := os.Lstat(filepath.Join(root, reserved.ObjectID, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("physical failure installed final %s: %v", name, err)
		}
	}
	attachmentFaultRejectUnverified(t, f, client, session.CSRF, room, reserved.ObjectID, "enospc", objects, before)
	status, body = f.post(client, path+"/cancel", map[string]string{"client_id": "enospc-cancel"}, session.CSRF)
	if status != http.StatusOK {
		t.Fatalf("explicit cancellation failed: %d %s", status, body)
	}
	state := attachmentFaultState(t, f, room, reserved.ObjectID)
	if state.state != "abandoned" || state.bytes != int64(len(data)) || state.pending != 0 || state.references != 0 {
		t.Fatalf("cancel refunded bytes before physical deletion/grace: %+v", state)
	}
	collection, err := f.store.CollectAttachments(t.Context(), objects, 1)
	if err != nil || collection.Deleted != 0 || attachmentFaultState(t, f, room, reserved.ObjectID).bytes != int64(len(data)) {
		t.Fatal("pre-grace collection released retained byte quota", collection, err)
	}
	// Existing fixture-only clock adjustment; no production seam or global clock.
	if _, err = f.admin.Exec(t.Context(), `ALTER TABLE attachments DISABLE TRIGGER attachments_times_once; UPDATE attachments SET abandoned_at=pg_catalog.clock_timestamp()-interval '8 days'; ALTER TABLE attachments ENABLE TRIGGER attachments_times_once`); err != nil {
		t.Fatal(err)
	}
	collection, err = f.store.CollectAttachments(t.Context(), objects, 1)
	if err != nil || collection.Deleted != 1 {
		t.Fatal("post-grace explicit collection did not complete deletion", collection, err)
	}
	state = attachmentFaultState(t, f, room, reserved.ObjectID)
	if state.bytes != 0 || state.pending != 0 || state.references != 0 || state.state != "deletion_pending" {
		t.Fatalf("physical deletion did not release quota exactly once: %+v", state)
	}
	spec := enterpriseobject.Spec{Name: "evidence.txt", Kind: "file", Size: int64(len(data)), SHA256: reserved.Metadata.SHA256}
	if _, err = objects.PutImmutable(t.Context(), reserved.ObjectID, spec, bytes.NewReader(data)); !errors.Is(err, enterpriseobject.ErrDeleted) {
		t.Fatalf("late upload after GC resurrected deleted identity: %v", err)
	}
	collection, err = f.store.CollectAttachments(t.Context(), objects, 1)
	if err != nil || collection.Deleted != 0 || attachmentFaultState(t, f, room, reserved.ObjectID).bytes != 0 {
		t.Fatal("repeat collection refunded quota twice", collection, err)
	}
	t.Logf("real tmpfs capacity=%d < valid upload=%d: final object absent, verify object_missing, zero unverified references; cancel releases pending only; explicit aged GC refunds bytes once", uint64(fs.Bsize)*fs.Blocks, len(data))
}
