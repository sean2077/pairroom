package model_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func bindingCandidate() model.EnterpriseBinding {
	return model.EnterpriseBinding{Version: 1, RoomID: "room-1", Slot: model.ActorSlot1, BindID: "bind-1", Generation: 1, AgentID: "agent-1", Runtime: model.RuntimeClaude, Active: true, ParkEnabled: false}
}

func TestEnterpriseBindingCodecRoundTripWithoutNativeIdentity(t *testing.T) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 123456789, time.UTC)
	for _, retired := range []bool{false, true} {
		binding := bindingCandidate()
		if retired {
			binding.Active, binding.RetiredAt, binding.RetiredReason = false, &at, "explicit_unbind"
		}
		event, err := model.NewEnterpriseBindingUpdate(binding, at)
		if err != nil {
			t.Fatal(err)
		}
		if event.RoomID != binding.RoomID || event.Actor != binding.Slot || event.Kind != "native.enterprise.binding.updated" || event.Seq != 0 || !event.CreatedAt.Equal(at) {
			t.Fatalf("event: %+v", event)
		}
		decoded, err := model.DecodeEnterpriseBindingUpdate(event)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.BindID != binding.BindID || decoded.AgentID != binding.AgentID || decoded.Runtime != model.RuntimeClaude || decoded.Active != binding.Active || decoded.ParkEnabled != binding.ParkEnabled || decoded.Generation != 1 || decoded.RetiredReason != binding.RetiredReason {
			t.Fatalf("round trip: %+v", decoded)
		}
		if retired && (decoded.RetiredAt == nil || !decoded.RetiredAt.Equal(at)) {
			t.Fatal("retirement timestamp changed")
		}
		for _, forbidden := range []string{"session_id", "credential_hash", "remote_key", "transcript_path", "secret"} {
			if strings.Contains(string(event.Data), forbidden) {
				t.Fatalf("native credential field leaked: %s", event.Data)
			}
		}
	}
}

func TestEnterpriseBindingCodecRequiresLiteralParkPolicy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		binding := bindingCandidate()
		binding.ParkEnabled = enabled
		event, err := model.NewEnterpriseBindingUpdate(binding, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := model.DecodeEnterpriseBindingUpdate(event)
		if err != nil || decoded.ParkEnabled != enabled {
			t.Fatalf("park policy %v did not round-trip: %+v %v", enabled, decoded, err)
		}
		for name, value := range map[string]json.RawMessage{"missing": nil, "null": []byte(`null`), "string": []byte(`"false"`), "number": []byte(`0`)} {
			t.Run(name, func(t *testing.T) {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(event.Data, &fields); err != nil {
					t.Fatal(err)
				}
				if value == nil {
					delete(fields, "park_enabled")
				} else {
					fields["park_enabled"] = value
				}
				bad := event
				bad.Data, _ = json.Marshal(fields)
				if _, err := model.DecodeEnterpriseBindingUpdate(bad); err == nil {
					t.Fatal("missing or non-boolean park policy was inferred")
				}
			})
		}
	}
}

func TestEnterpriseBindingCodecRefusesInvalidIdentityAndRetirement(t *testing.T) {
	at := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	tests := map[string]func(*model.EnterpriseBinding){
		"future version":            func(b *model.EnterpriseBinding) { b.Version = 2 },
		"room missing":              func(b *model.EnterpriseBinding) { b.RoomID = "" },
		"room path":                 func(b *model.EnterpriseBinding) { b.RoomID = "room/other" },
		"human slot":                func(b *model.EnterpriseBinding) { b.Slot = model.ActorUser },
		"binding missing":           func(b *model.EnterpriseBinding) { b.BindID = "" },
		"agent missing":             func(b *model.EnterpriseBinding) { b.AgentID = "" },
		"agent path":                func(b *model.EnterpriseBinding) { b.AgentID = `agent\other` },
		"agent control":             func(b *model.EnterpriseBinding) { b.AgentID = "agent\nother" },
		"invalid utf8":              func(b *model.EnterpriseBinding) { b.AgentID = string([]byte{255}) },
		"generation zero":           func(b *model.EnterpriseBinding) { b.Generation = 0 },
		"generation overflow":       func(b *model.EnterpriseBinding) { b.Generation = math.MaxUint64 },
		"vacant runtime":            func(b *model.EnterpriseBinding) { b.Runtime = model.RuntimeAwaitingBinding },
		"invalid runtime":           func(b *model.EnterpriseBinding) { b.Runtime = "other" },
		"active retirement time":    func(b *model.EnterpriseBinding) { b.RetiredAt = &at },
		"active retirement reason":  func(b *model.EnterpriseBinding) { b.RetiredReason = "retired" },
		"retired missing metadata":  func(b *model.EnterpriseBinding) { b.Active = false },
		"retired missing reason":    func(b *model.EnterpriseBinding) { b.Active = false; b.RetiredAt = &at },
		"retired whitespace reason": func(b *model.EnterpriseBinding) { b.Active = false; b.RetiredAt = &at; b.RetiredReason = "\u2003" },
		"retired wrong time": func(b *model.EnterpriseBinding) {
			b.Active = false
			other := at.Add(time.Second)
			b.RetiredAt = &other
			b.RetiredReason = "retired"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			binding := bindingCandidate()
			mutate(&binding)
			if _, err := model.NewEnterpriseBindingUpdate(binding, at); err == nil {
				t.Fatal("invalid binding admitted")
			}
		})
	}
	if _, err := model.NewEnterpriseBindingUpdate(bindingCandidate(), time.Time{}); err == nil {
		t.Fatal("zero timestamp admitted")
	}
}

func TestEnterpriseBindingCodecAcceptsWorstCaseEscapingWithinFieldLimits(t *testing.T) {
	at := time.Now().UTC()
	binding := bindingCandidate()
	binding.RoomID, binding.BindID, binding.AgentID = strings.Repeat("<", 256), strings.Repeat(">", 256), strings.Repeat("&", 256)
	binding.Active, binding.RetiredAt, binding.RetiredReason = false, &at, strings.Repeat("<", 256)
	event, err := model.NewEnterpriseBindingUpdate(binding, at)
	if err != nil {
		t.Fatal(err)
	}
	if len(event.Data) <= 4<<10 {
		t.Fatal("fixture did not reach the JSON expansion boundary")
	}
	if _, err := model.DecodeEnterpriseBindingUpdate(event); err != nil {
		t.Fatalf("legal constructor output could not round-trip: %v", err)
	}
}

func TestEnterpriseBindingDecoderRejectsSmuggledFieldsAndEnvelopeMismatch(t *testing.T) {
	event, err := model.NewEnterpriseBindingUpdate(bindingCandidate(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"session_id", "credential_hash", "remote_key", "transcript_path", "token", "Version"} {
		t.Run(field, func(t *testing.T) {
			bad := event
			bad.Data = append(append([]byte{}, event.Data[:len(event.Data)-1]...), []byte(`,"`+field+`":"private"}`)...)
			if _, err := model.DecodeEnterpriseBindingUpdate(bad); err == nil {
				t.Fatal("unknown/aliased field admitted")
			}
		})
	}
	for name, mutate := range map[string]func(*model.Event){
		"wrong room":       func(e *model.Event) { e.RoomID = "room-2" },
		"wrong actor":      func(e *model.Event) { e.Actor = model.ActorSlot2 },
		"wrong kind":       func(e *model.Event) { e.Kind = "native.binding.updated" },
		"missing event id": func(e *model.Event) { e.ID = "" },
		"zero timestamp":   func(e *model.Event) { e.CreatedAt = time.Time{} },
		"duplicate field": func(e *model.Event) {
			e.Data = append(append([]byte{}, e.Data[:len(e.Data)-1]...), []byte(`,"version":1}`)...)
		},
		"trailing JSON": func(e *model.Event) { e.Data = append(append([]byte{}, e.Data...), []byte(` {}`)...) },
		"active null": func(e *model.Event) {
			e.Data = []byte(strings.Replace(string(e.Data), `"active":true`, `"active":null`, 1))
		},
		"active omitted": func(e *model.Event) {
			var fields map[string]any
			_ = json.Unmarshal(e.Data, &fields)
			delete(fields, "active")
			e.Data, _ = json.Marshal(fields)
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := event
			mutate(&bad)
			if _, err := model.DecodeEnterpriseBindingUpdate(bad); err == nil {
				t.Fatal("invalid binding encoding admitted")
			}
		})
	}
}
