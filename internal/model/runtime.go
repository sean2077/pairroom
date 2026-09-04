package model

import "strings"

// RuntimeKind selects the native coding-agent CLI bound to a Room slot.
// ActorID remains the durable slot identity (claude = Agent 1, codex = Agent 2)
// so existing Event Logs and Bindings stay valid when a slot switches runtime.
type RuntimeKind string

const (
	RuntimeClaude RuntimeKind = "claude"
	RuntimeCodex  RuntimeKind = "codex"
	RuntimeGrok   RuntimeKind = "grok"
)

func ParseRuntimeKind(value string) RuntimeKind {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "claude", "claude-code", "claude_code", "claudecode":
		return RuntimeClaude
	case "codex":
		return RuntimeCodex
	case "grok", "grok-build", "grok_build", "grokbuild":
		return RuntimeGrok
	case "":
		return ""
	default:
		return RuntimeKind(strings.ToLower(strings.TrimSpace(value)))
	}
}

func (k RuntimeKind) Valid() bool {
	switch k.Canonical() {
	case RuntimeClaude, RuntimeCodex, RuntimeGrok:
		return true
	default:
		return false
	}
}

func (k RuntimeKind) Canonical() RuntimeKind {
	return ParseRuntimeKind(string(k))
}

func (k RuntimeKind) CanonicalForSlot(actor ActorID) RuntimeKind {
	if strings.TrimSpace(string(k)) == "" {
		switch actor {
		case ActorCodex:
			return RuntimeCodex
		default:
			return RuntimeClaude
		}
	}
	return k.Canonical()
}

func (k RuntimeKind) DefaultCommand() string {
	switch k.Canonical() {
	case RuntimeCodex:
		return "codex"
	case RuntimeGrok:
		return "grok"
	default:
		return "claude"
	}
}

func (k RuntimeKind) DisplayName() string {
	switch k.Canonical() {
	case RuntimeCodex:
		return "Codex"
	case RuntimeGrok:
		return "Grok Build"
	default:
		return "Claude Code"
	}
}

func (k RuntimeKind) ProviderAgentType() string {
	switch k.Canonical() {
	case RuntimeCodex:
		return "codex"
	case RuntimeGrok:
		return "grok"
	default:
		return "claudecode"
	}
}

func SlotLabel(actor ActorID) string {
	switch actor {
	case ActorClaude:
		return "Agent 1"
	case ActorCodex:
		return "Agent 2"
	default:
		return actor.DisplayName()
	}
}

func SlotActors() []ActorID {
	return []ActorID{ActorClaude, ActorCodex}
}

// ParticipantDisplayName keeps the historical vendor names for the default
// Claude/Codex pairing and otherwise labels the durable slot plus runtime.
func ParticipantDisplayName(actor ActorID, runtime RuntimeKind) string {
	runtime = runtime.CanonicalForSlot(actor)
	if (actor == ActorClaude && runtime == RuntimeClaude) || (actor == ActorCodex && runtime == RuntimeCodex) {
		return runtime.DisplayName()
	}
	return SlotLabel(actor) + " · " + runtime.DisplayName()
}
