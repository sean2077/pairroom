package sqlstore

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

// humanSendCandidate builds a producer-contract-conformant candidate fact for
// admission tests; overrides mutate the JSON payload before decoding.
func humanSendCandidate(userID, clientID, to, text string, overrides map[string]any) model.Event {
	now := time.Now().UTC().Truncate(time.Microsecond)
	payload := map[string]any{
		"id":                "relay-" + clientID,
		"from":              "user",
		"to":                to,
		"text":              text,
		"state":             "queued",
		"target_generation": 1,
		"created_at":        now,
		"updated_at":        now,
		"source":            "send",
		"client_key":        humanSendClientKey(userID, clientID),
	}
	for key, value := range overrides {
		payload[key] = value
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return model.Event{ID: "evt-" + clientID, RoomID: "room-1", Kind: humanSendKind, Actor: model.ActorUser, CreatedAt: now, Data: data}
}

func humanSendCommand(userID, clientID, to, text string) HumanSendCommand {
	return HumanSendCommand{
		Auth:     AuthContext{Deployment: "dep-test", UserID: userID},
		RoomID:   "room-1",
		ClientID: clientID,
		Intent:   HumanSendIntent{To: to, Text: text},
		Fact:     humanSendCandidate(userID, clientID, to, text, nil),
	}
}

func humanSendFactClass(t *testing.T, cmd HumanSendCommand) string {
	t.Helper()
	key, class := humanSendKey(cmd)
	if class != "" {
		return class
	}
	_ = key
	return validateHumanSendFact(cmd)
}

func TestHumanSendAdmissionRefusals(t *testing.T) {
	if class := humanSendFactClass(t, humanSendCommand("u-owner", "hs-1", "slot2", "hello")); class != "" {
		t.Fatalf("valid candidate refused: %s", class)
	}
	payloadMutations := []struct {
		name      string
		overrides map[string]any
	}{
		{"from runtime", map[string]any{"from_runtime": "claude"}},
		{"from generation", map[string]any{"from_generation": 1}},
		{"author", map[string]any{"author": "host_owner"}},
		{"review", map[string]any{"review": map[string]any{}}},
		{"quote", map[string]any{"quote": map[string]any{}}},
		{"attachments", map[string]any{"attachments": []any{map[string]any{"id": "a"}}}},
		{"empty message id", map[string]any{"id": ""}},
		{"wrong from", map[string]any{"from": "slot1"}},
		{"wrong to", map[string]any{"to": "slot1"}},
		{"changed text", map[string]any{"text": "changed"}},
		{"wrong state", map[string]any{"state": "delivering"}},
		{"wrong source", map[string]any{"source": "stop"}},
		{"claimed at", map[string]any{"claimed_at": "2026-10-13T00:00:00Z"}},
		{"retry of", map[string]any{"retry_of": "relay-x"}},
		{"receipt", map[string]any{"receipt": "r-1"}},
		{"zero created", map[string]any{"created_at": "0001-01-01T00:00:00Z"}},
		{"updated differs", map[string]any{"updated_at": "2030-01-01T00:00:00Z"}},
		{"zero target generation", map[string]any{"target_generation": 0}},
		{"wrong client key", map[string]any{"client_key": "user/other/id"}},
		{"unknown field", map[string]any{"mystery": true}},
	}
	for _, test := range payloadMutations {
		cmd := humanSendCommand("u-owner", "hs-1", "slot2", "hello")
		cmd.Fact = humanSendCandidate("u-owner", "hs-1", "slot2", "hello", test.overrides)
		if class := humanSendFactClass(t, cmd); class != ClassInvalidFact {
			t.Fatalf("%s: class = %q, want %q", test.name, class, ClassInvalidFact)
		}
	}

	envelopeMutations := []struct {
		name   string
		mutate func(*HumanSendCommand)
		class  string
	}{
		{"wrong kind", func(c *HumanSendCommand) { c.Fact.Kind = "message" }, ClassInvalidFact},
		{"wrong actor", func(c *HumanSendCommand) { c.Fact.Actor = model.ActorSlot1 }, ClassInvalidFact},
		{"wrong room", func(c *HumanSendCommand) { c.Fact.RoomID = "room-2" }, ClassInvalidFact},
		{"slash event id", func(c *HumanSendCommand) { c.Fact.ID = "evt/1" }, ClassInvalidFact},
		{"bad target", func(c *HumanSendCommand) { c.Intent.To = "user" }, ClassInvalidRequest},
		{"blank text", func(c *HumanSendCommand) { c.Intent.Text = "   " }, ClassInvalidRequest},
		{"invalid utf8", func(c *HumanSendCommand) { c.Intent.Text = string([]byte{0xff, 0xfe}) }, ClassInvalidRequest},
		{"oversized text", func(c *HumanSendCommand) { c.Intent.Text = strings.Repeat("x", humanSendTextBytes+1) }, ClassInvalidRequest},
		{"slash principal", func(c *HumanSendCommand) { c.Auth.UserID = "u/1" }, ClassInvalidRequest},
		{"slash client id", func(c *HumanSendCommand) { c.ClientID = "c/1" }, ClassInvalidRequest},
		{"revision out of range", func(c *HumanSendCommand) { c.ExpectedRevision = math.MaxUint64 }, ClassInvalidRequest},
	}
	for _, test := range envelopeMutations {
		cmd := humanSendCommand("u-owner", "hs-1", "slot2", "hello")
		test.mutate(&cmd)
		if class := humanSendFactClass(t, cmd); class != test.class {
			t.Fatalf("%s: class = %q, want %q", test.name, class, test.class)
		}
	}
}

func TestHumanSendHashExcludesGeneratedArtifacts(t *testing.T) {
	cmd := humanSendCommand("u-owner", "hs-1", "slot2", "hello")
	key, class := humanSendKey(cmd)
	if class != "" {
		t.Fatal(class)
	}
	base := humanSendHash(key, cmd.Intent)
	regenerated := humanSendCandidate("u-owner", "hs-1", "slot2", "hello", map[string]any{
		"id":         "relay-regenerated",
		"created_at": "2026-10-13T01:02:03.123456789Z",
		"updated_at": "2026-10-13T01:02:03.123456789Z",
	})
	regenerated.ID = "evt-regenerated"
	if humanSendFactClass(t, HumanSendCommand{Auth: cmd.Auth, RoomID: cmd.RoomID, ClientID: cmd.ClientID, Intent: cmd.Intent, Fact: regenerated}) != "" {
		t.Fatal("regenerated candidate refused")
	}
	if humanSendHash(key, cmd.Intent) != base {
		t.Fatal("intent hash changed")
	}
	if humanSendHash(key, HumanSendIntent{To: "slot2", Text: "changed"}) == base {
		t.Fatal("changed text must change the hash")
	}
}
