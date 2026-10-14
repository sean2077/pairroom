package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEnterpriseMembershipCodecRoundTripsClosedActions(t *testing.T) {
	for _, intent := range []EnterpriseMembershipIntent{
		{TargetUserID: "user-1", Action: EnterpriseMembershipAdmit, Role: EnterpriseRoomMember},
		{TargetUserID: "user-1", Action: EnterpriseMembershipChangeRole, Role: EnterpriseRoomViewer},
		{TargetUserID: "user-1", Action: EnterpriseMembershipRemove},
	} {
		fact, err := NewEnterpriseMembershipChange("room-1", 7, intent, time.Date(2026, 10, 13, 2, 3, 4, 123456789, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeEnterpriseMembershipChange(fact)
		if err != nil || decoded.Version != 1 || decoded.EnterpriseMembershipIntent != intent || fact.Actor != ActorSystem || fact.Seq != 7 || fact.CreatedAt.Nanosecond() != 123456789 {
			t.Fatalf("closed codec: %+v %+v %v", fact, decoded, err)
		}
	}
}

func TestEnterpriseMembershipCodecRejectsInvalidFacts(t *testing.T) {
	valid, err := NewEnterpriseMembershipChange("room-1", 2, EnterpriseMembershipIntent{TargetUserID: "u-target", Action: EnterpriseMembershipAdmit, Role: EnterpriseRoomMember}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"version":2,"target_user_id":"u-target","action":"admit","role":"member"}`,
		`{"version":1,"target_user_id":"u-target","action":"admit","role":"owner"}`,
		`{"version":1,"target_user_id":"u-target","action":"transfer","role":"member"}`,
		`{"version":1,"target_user_id":"u-target","action":"remove","role":"viewer"}`,
		`{"version":1,"target_user_id":" u-target ","action":"admit","role":"member"}`,
		`{"version":1,"target_user_id":"u/target","action":"admit","role":"member"}`,
		`{"version":1,"target_user_id":"u-target","action":"admit","role":"member","admin":true}`,
		`{"version":1,"Version":1,"target_user_id":"u-target","action":"admit","role":"member"}`,
		`{"version":1,"target_user_id":"u-target","action":"admit","action":"remove","role":"member"}`,
		`{"version":1,"target_user_id":"u-target","action":"remove"} {}`,
		`null`,
	} {
		fact := valid
		fact.Data = json.RawMessage(body)
		if _, err := DecodeEnterpriseMembershipChange(fact); err == nil {
			t.Fatalf("accepted invalid body %s", body)
		}
	}
	for _, change := range []func(*Event){
		func(f *Event) { f.Actor = ActorUser },
		func(f *Event) { f.Seq = 1 },
		func(f *Event) { f.Kind = "room.membership.other" },
		func(f *Event) { f.CreatedAt = time.Time{} },
		func(f *Event) { f.RoomID = "" },
		func(f *Event) {
			f.Data = []byte(strings.Replace(string(valid.Data), "u-target", string([]byte{0xff}), 1))
		},
	} {
		fact := valid
		change(&fact)
		if _, err := DecodeEnterpriseMembershipChange(fact); err == nil {
			t.Fatalf("accepted invalid envelope %+v", fact)
		}
	}
}
