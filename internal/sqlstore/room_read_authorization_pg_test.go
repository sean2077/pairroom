package sqlstore

import "testing"

func TestRoomReadReleaseRechecksLiveACLWithoutEventAllocation(t *testing.T) {
	b := bindingPGFixture(t, nil)
	if e := b.f.store.AuthorizeRoomRead(t.Context(), b.member.Auth, "binding-room"); e != nil {
		t.Fatal(e)
	}
	// Even a hostile metadata sequence cannot cause this release check to read
	// or allocate a log. The normal snapshot reader still rejects that Room.
	mustExec(t, b.f.admin, `UPDATE rooms SET current_seq=9223372036854775807 WHERE room_id='binding-room'`)
	if e := b.f.store.AuthorizeRoomRead(t.Context(), b.member.Auth, "binding-room"); e != nil {
		t.Fatal("metadata-only check replayed events", e)
	}
	if _, e := b.f.store.ReadonlySnapshot(t.Context(), b.member.Auth, "binding-room"); e == nil {
		t.Fatal("snapshot accepted hostile oversized log metadata")
	}
	if _, e := b.f.admin.Exec(t.Context(), `UPDATE room_members SET disabled=true WHERE room_id='binding-room' AND user_id=$1`, b.member.Auth.UserID); e != nil {
		t.Fatal(e)
	}
	if e := b.f.store.AuthorizeRoomRead(t.Context(), b.member.Auth, "binding-room"); e == nil {
		t.Fatal("removed Member passed final release")
	}
	if e := b.f.store.AuthorizeRoomRead(t.Context(), b.owner.Auth, "binding-room"); e != nil {
		t.Fatal("other Member removal changed Owner authority", e)
	}
	if _, e := b.f.admin.Exec(t.Context(), `UPDATE browser_sessions SET revoked_at=now() WHERE session_id=$1`, b.owner.Auth.SessionID); e != nil {
		t.Fatal(e)
	}
	if e := b.f.store.AuthorizeRoomRead(t.Context(), b.owner.Auth, "binding-room"); e == nil {
		t.Fatal("revoked session passed final release")
	}
	if e := b.f.store.AuthorizeRoomRead(t.Context(), b.device.Auth, "binding-room"); e == nil {
		t.Fatal("broad Device grant passed final release")
	}
}
