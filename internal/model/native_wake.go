package model

// NativeWakeTransport is the allowlist of external wake effects PairRoom can
// submit to an already-bound native session. Its zero value permits no effect.
type NativeWakeTransport uint8

const (
	NativeWakeUnavailable NativeWakeTransport = iota
	NativeWakeClaudeInbox
	NativeWakeCodexQueue
)

// Supported is the dispatch allowlist. A future nonzero transport does not
// gain an external effect until its handler is deliberately supported here.
func (t NativeWakeTransport) Supported() bool {
	return t == NativeWakeClaudeInbox || t == NativeWakeCodexQueue
}

// NativeWakeConsumption identifies the relay observation that releases an
// outstanding wake. A handoff supersedes the need for the nudge; a Turn boundary
// infers consumption. Neither proves vendor/model acceptance. Without either
// observation, the wake renewal delay bounds suppression.
type NativeWakeConsumption uint8

const (
	NativeWakeUnobserved NativeWakeConsumption = iota
	NativeWakeAfterHandoff
	NativeWakeAfterTurnEnd
)

type NativeWakePolicy struct {
	Transport   NativeWakeTransport
	Consumption NativeWakeConsumption
}

// NativeWakePolicy keeps transport selection and consumption inference tied to
// the same runtime contract. Missing/unknown runtimes do not inherit a slot's
// default runtime or an external wake capability.
func (k RuntimeKind) NativeWakePolicy() NativeWakePolicy {
	switch k.Canonical() {
	case RuntimeClaude:
		return NativeWakePolicy{Transport: NativeWakeClaudeInbox, Consumption: NativeWakeAfterHandoff}
	case RuntimeCodex:
		return NativeWakePolicy{Transport: NativeWakeCodexQueue, Consumption: NativeWakeAfterTurnEnd}
	default:
		return NativeWakePolicy{}
	}
}
