package sqlstore

import (
	"math"
	"testing"
)

func TestResourceLimitsSelectBoundedDefaultsAndCannotRaiseHardCeilings(t *testing.T) {
	d := (ResourceLimits{}).effective()
	if !d.valid() || d.ReplayEvents != 65536 || d.ReplayBytes != 64<<20 || d.NewWorkEvents != 32768 || d.NewWorkBytes != 32<<20 || d.PendingMessages != 256 || d.ActionableIdentities != 5000 || d.PersistentRooms != 5000 || d.ObjectRecords != 16384 || d.PersistentUsers != 5000 || d.PersistentProjects != 1000 || d.MembershipRecords != 50000 || d.MembersPerResource != 1000 {
		t.Fatal("unbounded or changed resource defaults")
	}
	for _, change := range []func(*ResourceLimits){func(l *ResourceLimits) { l.ReplayEvents = 65537 }, func(l *ResourceLimits) { l.ReplayBytes = 65 << 20 }, func(l *ResourceLimits) { l.NewWorkEvents = l.ReplayEvents }, func(l *ResourceLimits) { l.NewWorkBytes = l.ReplayBytes }, func(l *ResourceLimits) { l.PendingMessages = 257 }, func(l *ResourceLimits) { l.ActionableIdentities = 5001 }, func(l *ResourceLimits) { l.PersistentRooms = 5001 }, func(l *ResourceLimits) { l.ObjectRecords = 16385 }, func(l *ResourceLimits) { l.PersistentUsers = 5001 }, func(l *ResourceLimits) { l.PersistentProjects = 1001 }, func(l *ResourceLimits) { l.MembershipRecords = 50001 }, func(l *ResourceLimits) { l.MembersPerResource = 1001 }, func(l *ResourceLimits) { l.ReplayBytes = -1 }, func(l *ResourceLimits) { l.NewWorkEvents = math.MaxInt64 }} {
		l := d
		change(&l)
		if ValidateResourceLimits(l) == nil {
			t.Fatal("resource ceiling could be disabled or raised")
		}
	}
}
