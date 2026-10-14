package model

import (
	"testing"
	"time"
)

func TestEnterpriseRoomMetadataPreservesOnlyExplicitLogicalChanges(t *testing.T) {
	for _, change := range []EnterpriseRoomMetadataChange{{Version: 1, Action: "rename", Name: "New name"}, {Version: 1, Action: "archive"}, {Version: 1, Action: "restore"}, {Version: 1, Action: "project", ProjectID: "project-1"}, {Version: 1, Action: "project"}} {
		fact, err := NewEnterpriseRoomMetadata("room-1", 2, change, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		read, err := DecodeEnterpriseRoomMetadata(fact)
		if err != nil || read != change {
			t.Fatal("metadata did not round-trip")
		}
		for _, body := range []string{`{"version":1,"action":"archive","name":""}`, `{"version":1,"action":"archive","action":"restore"}`, `{"version":1,"action":"archive","runtime":"codex"}`, `{"version":1,"action":"archive","project_id":null}`, `{"version":2,"action":"archive"}`, `{"version":1,"action":"rename","name":" x"}`} {
			bad := fact
			bad.Data = []byte(body)
			if _, err := DecodeEnterpriseRoomMetadata(bad); err == nil {
				t.Fatalf("ambiguous metadata accepted: %s", body)
			}
		}
	}
}
