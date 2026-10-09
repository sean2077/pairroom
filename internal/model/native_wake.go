package model

// NativeWakeTransport is the allowlist of external wake effects PairRoom can
// submit to an already-bound native session. Its zero value permits no effect.
type NativeWakeTransport uint8

const (
	NativeWakeUnavailable NativeWakeTransport = iota
	NativeWakeClaudeInbox
	NativeWakeCodexQueue
)

// NativeWakeConsumption identifies the relay observation used to infer that
// an outstanding nudge was consumed. Neither observation proves vendor/model
// acceptance; absence of the observation is bounded by the wake renewal delay.
type NativeWakeConsumption uint8

const (
	NativeWakeUnobserved NativeWakeConsumption = iota
	NativeWakeAfterRelayCall
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
		return NativeWakePolicy{Transport: NativeWakeClaudeInbox, Consumption: NativeWakeAfterRelayCall}
	case RuntimeCodex:
		return NativeWakePolicy{Transport: NativeWakeCodexQueue, Consumption: NativeWakeAfterTurnEnd}
	default:
		return NativeWakePolicy{}
	}
}
