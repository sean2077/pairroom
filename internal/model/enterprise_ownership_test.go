package model

import (
	"strings"
	"testing"
	"time"
)

func TestEnterpriseOwnershipHistoryHasExplicitClosedPolicy(t *testing.T) {
	change := EnterpriseOwnershipChange{Version: 1, FromUserID: "original-owner", ToUserID: "eligible-member", Governance: true}
	fact, err := NewEnterpriseOwnershipChange("room-test", 3, change, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecodeEnterpriseOwnershipChange(fact); err != nil || got != change {
		t.Fatalf("ownership codec: %+v %v", got, err)
	}
	for name, mutate := range map[string]func(*Event){
		"unknown-field": func(f *Event) { f.Data = []byte(strings.TrimSuffix(string(f.Data), "}") + `,"grant":"invented"}`) },
		"duplicate": func(f *Event) {
			f.Data = []byte(strings.Replace(string(f.Data), `"governance":true`, `"governance":true,"governance":false`, 1))
		},
		"case-alias":     func(f *Event) { f.Data = []byte(strings.Replace(string(f.Data), `"governance"`, `"Governance"`, 1)) },
		"missing-policy": func(f *Event) { f.Data = []byte(strings.Replace(string(f.Data), `,"governance":true`, "", 1)) },
		"null-policy": func(f *Event) {
			f.Data = []byte(strings.Replace(string(f.Data), `"governance":true`, `"governance":null`, 1))
		},
		"future-version": func(f *Event) { f.Data = []byte(strings.Replace(string(f.Data), `"version":1`, `"version":9`, 1)) },
		"first-fact":     func(f *Event) { f.Seq = 1 },
		"agent-author":   func(f *Event) { f.Actor = ActorSlot1 },
		"no-change": func(f *Event) {
			f.Data = []byte(strings.Replace(string(f.Data), "eligible-member", "original-owner", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := fact
			candidate.Data = append([]byte(nil), fact.Data...)
			mutate(&candidate)
			if _, err := DecodeEnterpriseOwnershipChange(candidate); err == nil {
				t.Fatal("ambiguous ownership fact accepted")
			}
		})
	}
	change.Governance = false
	if _, err := NewEnterpriseOwnershipChange("room-test", 4, change, time.Now().UTC()); err != nil {
		t.Fatal("explicit normal Owner transfer rejected")
	}
}
