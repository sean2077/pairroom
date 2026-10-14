package sqlstore

import (
	"crypto/sha256"
	"testing"
)

func TestApprovedDeviceEnrollmentDoesNotReviveAfterUserEnable(t *testing.T) {
	f := identityPGFixture(t, nil)
	_, admin := identityPGLogin(t, f, "operator-selected", "enrollment-admin")
	_, owner := identityPGLogin(t, f, "member-one", "enrollment-owner")
	c := devicePGCommand("approved-before-disable")
	if _, e := f.store.BeginDeviceEnrollment(t.Context(), c); e != nil {
		t.Fatal(e)
	}
	code := sha256.Sum256([]byte("old-device-approval"))
	if _, e := f.store.DecideDeviceEnrollment(t.Context(), DeviceEnrollmentDecision{Auth: owner.Auth, ClientID: "approve-before-disable", RequestID: c.RequestID, Approve: true, CodeHash: code}); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetUserEnabled(t.Context(), admin.Auth, "disable-approved-device-owner", owner.Auth.UserID, false); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SetUserEnabled(t.Context(), admin.Auth, "enable-approved-device-owner", owner.Auth.UserID, true); e != nil {
		t.Fatal(e)
	}
	r, e := f.store.CompleteDeviceEnrollment(t.Context(), DeviceEnrollmentCompletion{RequestID: c.RequestID, ClientID: "complete-old-approved-device", Origin: c.Origin, ProofHash: c.ProofHash, CodeHash: code})
	if e == nil || r.Outcome != OutcomeRefused {
		t.Fatalf("old approved Device request revived after enable: %+v %v", r, e)
	}
	var status, approved string
	var count int
	if e = f.admin.QueryRow(t.Context(), `SELECT status,approved_user_id,approval_count FROM device_enrollments WHERE request_id=$1`, c.RequestID).Scan(&status, &approved, &count); e != nil || status != "invalidated" || approved != owner.Auth.UserID || count != 1 {
		t.Fatal("approval provenance was fabricated or lost", e)
	}
}
