package sqlstore

import (
	"math"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func unitKey() OpKey {
	return OpKey{
		Namespace:  namespaceHuman,
		Deployment: "dep",
		RoomID:     "room-1",
		OpKind:     opKindAppendFact,
		Identifier: "op-1",
		Principal:  "u-1",
	}
}

func unitMutation() Mutation {
	return Mutation{
		Version: mutationVersionV1,
		Fact: model.Event{
			ID:        "evt-1",
			RoomID:    "room-1",
			Kind:      "room.note",
			Actor:     model.ActorUser,
			CreatedAt: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC),
			Data:      []byte(`{"text":"hello"}`),
		},
		Audit: AuditRecord{Kind: "fact.append", Outcome: "accepted"},
	}
}

func unitCommand() Command {
	return Command{
		Auth:     AuthContext{Deployment: "dep", UserID: "u-1"},
		Key:      unitKey(),
		Mutation: unitMutation(),
	}
}

// Only system-generated artifacts are excluded from the mutation hash: a
// retried logical operation with a freshly constructed candidate event must
// hash identically, which is what lets replay return the original fact.
func TestCanonicalHashExcludesOnlySystemGeneratedArtifacts(t *testing.T) {
	base := unitMutation()
	recandidate := unitMutation()
	recandidate.Fact.ID = "evt-regenerated"
	recandidate.Fact.Seq = 44
	recandidate.Fact.CreatedAt = base.Fact.CreatedAt.Add(time.Hour + 123456789*time.Nanosecond)
	if canonicalHash(unitKey(), base) != canonicalHash(unitKey(), recandidate) {
		t.Fatal("system-generated event fields changed the mutation hash")
	}
}

// Every stable field participates, so a changed body, kind or audit detail can
// never replay under the same key.
func TestCanonicalHashCoversEveryStableField(t *testing.T) {
	base := canonicalHash(unitKey(), unitMutation())
	mutations := []struct {
		name   string
		mutate func(*Mutation)
	}{
		{"fact data", func(m *Mutation) { m.Fact.Data = []byte(`{"text":"different"}`) }},
		{"fact kind", func(m *Mutation) { m.Fact.Kind = "room.other" }},
		{"fact room", func(m *Mutation) { m.Fact.RoomID = "room-2" }},
		{"audit kind", func(m *Mutation) { m.Audit.Kind = "other" }},
		{"audit outcome", func(m *Mutation) { m.Audit.Outcome = "rejected" }},
		{"audit detail", func(m *Mutation) { m.Audit.Detail = "changed" }},
		{"mutation version", func(m *Mutation) { m.Version = 2 }},
	}
	for _, test := range mutations {
		mutation := unitMutation()
		test.mutate(&mutation)
		if canonicalHash(unitKey(), mutation) == base {
			t.Fatalf("%s did not change the mutation hash", test.name)
		}
	}
	keys := []struct {
		name   string
		mutate func(*OpKey)
	}{
		{"identifier", func(k *OpKey) { k.Identifier = "op-2" }},
		{"room", func(k *OpKey) { k.RoomID = "room-2" }},
		{"principal", func(k *OpKey) { k.Principal = "u-2" }},
	}
	for _, test := range keys {
		key := unitKey()
		test.mutate(&key)
		if canonicalHash(key, unitMutation()) == base {
			t.Fatalf("key %s did not change the mutation hash", test.name)
		}
	}
}

func TestValidateCommandRefusals(t *testing.T) {
	if class := validateCommand(unitCommand()); class != "" {
		t.Fatalf("valid command refused: %s", class)
	}
	cases := []struct {
		name   string
		mutate func(*Command)
		class  string
	}{
		{"binding namespace", func(c *Command) { c.Key.Namespace = "binding" }, ClassUnsupportedNamespace},
		{"binding fields", func(c *Command) { c.Key.BindingID = "b-1"; c.Key.Generation = 1 }, ClassUnsupportedNamespace},
		{"unknown op", func(c *Command) { c.Key.OpKind = "send" }, ClassUnsupportedOp},
		{"future mutation", func(c *Command) { c.Mutation.Version = 2 }, ClassUnsupportedMutation},
		{"deployment mismatch", func(c *Command) { c.Key.Deployment = "other" }, ClassInvalidRequest},
		{"principal mismatch", func(c *Command) { c.Key.Principal = "u-2" }, ClassInvalidRequest},
		{"empty identifier", func(c *Command) { c.Key.Identifier = "" }, ClassInvalidRequest},
		{"fact room mismatch", func(c *Command) { c.Mutation.Fact.RoomID = "room-2" }, ClassInvalidFact},
		{"empty event id", func(c *Command) { c.Mutation.Fact.ID = "" }, ClassInvalidFact},
		{"bad kind shape", func(c *Command) { c.Mutation.Fact.Kind = "BadKind" }, ClassInvalidFact},
		{"invalid json", func(c *Command) { c.Mutation.Fact.Data = []byte("{") }, ClassInvalidFact},
		{"empty audit", func(c *Command) { c.Mutation.Audit.Kind = "" }, ClassInvalidFact},
		{"free-text audit detail", func(c *Command) { c.Mutation.Audit.Detail = "body text" }, ClassInvalidFact},
		{"actor spoof", func(c *Command) { c.Mutation.Fact.Actor = model.ActorSlot1 }, ClassInvalidFact},
		{"revision out of range", func(c *Command) { c.ExpectedRevision = math.MaxUint64 }, ClassInvalidRequest},
	}
	for _, test := range cases {
		cmd := unitCommand()
		test.mutate(&cmd)
		if class := validateCommand(cmd); class != test.class {
			t.Fatalf("%s: class = %q, want %q", test.name, class, test.class)
		}
	}
}
