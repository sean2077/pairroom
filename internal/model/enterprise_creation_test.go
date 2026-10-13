package model_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/protocol"
)

func enterpriseCreationFixture(t *testing.T) model.Event {
	t.Helper()
	intent, err := model.PrepareEnterpriseRoomIntent("  New Room  ", model.Collaboration{})
	if err != nil {
		t.Fatal(err)
	}
	event, err := model.NewEnterpriseRoomCreation("room-new", "deployment-1", "org-1", "user-1", intent, time.Date(2026, 10, 13, 1, 2, 3, 456789, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestEnterpriseRoomCreationRoundTripAndProtocol(t *testing.T) {
	event := enterpriseCreationFixture(t)
	creation, err := model.DecodeEnterpriseRoomCreation(event)
	if err != nil {
		t.Fatal(err)
	}
	if event.Seq != 1 || event.Actor != model.ActorSystem || event.Kind != "room.created" || event.ID == "" || creation.ID != "room-new" || creation.Name != "New Room" || creation.Repo != "" || !creation.CreatedAt.Equal(event.CreatedAt) {
		t.Fatalf("creation identity/metadata: event=%+v creation=%+v", event, creation)
	}
	if creation.Enterprise.Version != 1 || creation.Enterprise.DeploymentID != "deployment-1" || creation.Enterprise.OrgID != "org-1" || creation.Enterprise.InitialOwnerUserID != "user-1" || creation.Enterprise.HostMode != model.HostNative || creation.Enterprise.ProtocolVersion != protocol.NativeDynamicVersion {
		t.Fatalf("creation origin: %+v", creation.Enterprise)
	}
	if len(creation.Enterprise.InitialSlots) != 2 || creation.Enterprise.InitialSlots[0] != (model.EnterpriseInitialSlot{Slot: model.ActorSlot1, Generation: 1, State: "vacant"}) || creation.Enterprise.InitialSlots[1] != (model.EnterpriseInitialSlot{Slot: model.ActorSlot2, Generation: 1, State: "vacant"}) {
		t.Fatalf("creation slots: %+v", creation.Enterprise.InitialSlots)
	}
	if creation.Collaboration == nil || creation.Collaboration.Version != 2 || creation.Collaboration.Mode != "default" || creation.Collaboration.Instructions != model.DefaultCollaborationInstructions {
		t.Fatalf("frozen collaboration: %+v", creation.Collaboration)
	}
	for _, forbidden := range []string{`"runtime"`, `"binding"`, `"session_id"`} {
		if bytes.Contains(event.Data, []byte(forbidden)) {
			t.Fatalf("creation fabricated %s", forbidden)
		}
	}
	// Every creation keeps fresh system-generated identity while retaining its
	// caller-controlled stable intent exactly.
	next := enterpriseCreationFixture(t)
	if next.ID == event.ID {
		t.Fatal("two creations reused an event ID")
	}
}

func TestEnterpriseRoomIntentFrozenValidation(t *testing.T) {
	intent, err := model.PrepareEnterpriseRoomIntent("  Custom  ", model.Collaboration{Mode: " custom ", Instructions: "  Preserve this rule.  "})
	if err != nil {
		t.Fatal(err)
	}
	if intent.Name != "Custom" || intent.Collaboration.Mode != "custom" || intent.Collaboration.Version != 2 || intent.Collaboration.Instructions != "Preserve this rule." {
		t.Fatalf("prepared intent: %+v", intent)
	}
	for name, mutate := range map[string]func(*model.EnterpriseRoomIntent){
		"empty name":             func(i *model.EnterpriseRoomIntent) { i.Name = "" },
		"untrimmed name":         func(i *model.EnterpriseRoomIntent) { i.Name = " Custom" },
		"control name":           func(i *model.EnterpriseRoomIntent) { i.Name = "two\nlines" },
		"invalid UTF8":           func(i *model.EnterpriseRoomIntent) { i.Name = string([]byte{0xff}) },
		"byte limit":             func(i *model.EnterpriseRoomIntent) { i.Name = strings.Repeat("界", 54) },
		"missing version":        func(i *model.EnterpriseRoomIntent) { i.Collaboration.Version = 0 },
		"future version":         func(i *model.EnterpriseRoomIntent) { i.Collaboration.Version = 3 },
		"missing mode":           func(i *model.EnterpriseRoomIntent) { i.Collaboration.Mode = "" },
		"untrimmed instructions": func(i *model.EnterpriseRoomIntent) { i.Collaboration.Instructions += " " },
		"empty instructions":     func(i *model.EnterpriseRoomIntent) { i.Collaboration.Instructions = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := intent
			mutate(&bad)
			if err := model.ValidateEnterpriseRoomIntent(bad); err == nil {
				t.Fatal("non-frozen intent accepted")
			}
		})
	}
	limit := intent
	limit.Name = strings.Repeat("a", 160)
	if err := model.ValidateEnterpriseRoomIntent(limit); err != nil {
		t.Fatalf("160-byte name refused: %v", err)
	}
	legacy, err := model.PrepareEnterpriseRoomIntent("Explicit supported version", model.Collaboration{Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Collaboration.Version != 1 {
		t.Fatal("supported explicit collaboration version was rewritten")
	}
}

func TestEnterpriseRoomCreationRejectsMalformedOrUnsupportedOrigin(t *testing.T) {
	for name, mutate := range map[string]func(*model.EnterpriseRoomCreation){
		"metadata ID":            func(c *model.EnterpriseRoomCreation) { c.ID = "other" },
		"local Repo":             func(c *model.EnterpriseRoomCreation) { c.Repo = "C:/work" },
		"time mismatch":          func(c *model.EnterpriseRoomCreation) { c.CreatedAt = c.CreatedAt.Add(time.Nanosecond) },
		"missing collaboration":  func(c *model.EnterpriseRoomCreation) { c.Collaboration = nil },
		"unfrozen collaboration": func(c *model.EnterpriseRoomCreation) { c.Collaboration.Version = 0 },
		"missing origin":         func(c *model.EnterpriseRoomCreation) { c.Enterprise = model.EnterpriseRoomOrigin{} },
		"future version":         func(c *model.EnterpriseRoomCreation) { c.Enterprise.Version = 2 },
		"embedded host":          func(c *model.EnterpriseRoomCreation) { c.Enterprise.HostMode = model.HostEmbedded },
		"legacy protocol":        func(c *model.EnterpriseRoomCreation) { c.Enterprise.ProtocolVersion = protocol.NativeVersion },
		"missing deployment":     func(c *model.EnterpriseRoomCreation) { c.Enterprise.DeploymentID = "" },
		"missing org":            func(c *model.EnterpriseRoomCreation) { c.Enterprise.OrgID = "" },
		"missing owner":          func(c *model.EnterpriseRoomCreation) { c.Enterprise.InitialOwnerUserID = "" },
		"one slot":               func(c *model.EnterpriseRoomCreation) { c.Enterprise.InitialSlots = c.Enterprise.InitialSlots[:1] },
		"duplicate slot":         func(c *model.EnterpriseRoomCreation) { c.Enterprise.InitialSlots[1].Slot = model.ActorSlot1 },
		"noninitial generation":  func(c *model.EnterpriseRoomCreation) { c.Enterprise.InitialSlots[1].Generation = 2 },
		"bound slot":             func(c *model.EnterpriseRoomCreation) { c.Enterprise.InitialSlots[1].State = "bound" },
	} {
		t.Run(name, func(t *testing.T) {
			event := enterpriseCreationFixture(t)
			creation, err := model.DecodeEnterpriseRoomCreation(event)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&creation)
			event.Data, err = json.Marshal(creation)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := model.DecodeEnterpriseRoomCreation(event); err == nil {
				t.Fatal("corrupt creation accepted")
			}
		})
	}
	for name, mutate := range map[string]func(*model.Event){
		"wrong sequence":   func(e *model.Event) { e.Seq = 2 },
		"wrong actor":      func(e *model.Event) { e.Actor = model.ActorUser },
		"wrong kind":       func(e *model.Event) { e.Kind = "native.message" },
		"missing event ID": func(e *model.Event) { e.ID = "" },
		"missing time":     func(e *model.Event) { e.CreatedAt = time.Time{} },
		"trailing JSON":    func(e *model.Event) { e.Data = append(e.Data, []byte(` {}`)...) },
		"unknown Runtime": func(e *model.Event) {
			e.Data = bytes.Replace(e.Data, []byte(`"state":"vacant"`), []byte(`"state":"vacant","runtime":"codex"`), 1)
		},
		"duplicate key": func(e *model.Event) {
			e.Data = bytes.Replace(e.Data, []byte(`"version":1`), []byte(`"version":2,"version":1`), 1)
		},
		"case folded duplicate key": func(e *model.Event) {
			e.Data = bytes.Replace(e.Data, []byte(`"name":"New Room"`), []byte(`"name":"Other Room","Name":"New Room"`), 1)
		},
		"case folded key alias": func(e *model.Event) {
			e.Data = bytes.Replace(e.Data, []byte(`"name":"New Room"`), []byte(`"Name":"New Room"`), 1)
		},
		"invalid raw UTF8": func(e *model.Event) { e.Data = bytes.Replace(e.Data, []byte("New Room"), []byte{'x', 0xff}, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			event := enterpriseCreationFixture(t)
			mutate(&event)
			if _, err := model.DecodeEnterpriseRoomCreation(event); err == nil {
				t.Fatal("invalid creation event accepted")
			}
		})
	}
}
