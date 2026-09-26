package service

import (
	"context"
	"strings"
	"testing"
)

// Doctor is a read-only diagnosis of an already active Native Room: it must
// not activate a suspended one (activation resumes Service-managed wake). Its
// error instead names the activating read, and doctor works once it ran.
func TestNativeDoctorOnSuspendedRoomIsActionableAndDoesNotActivate(t *testing.T) {
	f := nativeHTTP(t)
	f.bind(t, "slot1")
	f.bind(t, "slot2")
	if err := f.manager.Suspend(context.Background(), f.room.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.run(t, []string{"doctor", "--room", f.room.ID, "--slot", "1"}, nil)
	if err == nil || !strings.Contains(err.Error(), "room runtime is not active") || !strings.Contains(err.Error(), "doctor never activates it") || !strings.Contains(err.Error(), "relay status --brief --room "+f.room.ID) {
		t.Fatalf("suspended doctor error is not actionable: %v", err)
	}
	if phase := f.manager.Status(f.room.ID).Phase; phase != RuntimeSuspended {
		t.Fatalf("doctor activated the Room: %s", phase)
	}
	if _, err := f.run(t, []string{"status", "--brief", "--room", f.room.ID, "--slot", "1"}, nil); err != nil {
		t.Fatal(err)
	}
	if phase := f.manager.Status(f.room.ID).Phase; phase != RuntimeActive {
		t.Fatalf("status did not activate the Room: %s", phase)
	}
	report, err := f.run(t, []string{"doctor", "--room", f.room.ID, "--slot", "1"}, nil)
	if err != nil || !strings.Contains(string(report), `"hook_installation":"installed"`) {
		t.Fatalf("doctor after activation: %s %v", report, err)
	}
}
