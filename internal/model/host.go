package model

// HostMode is immutable for a Room. Empty is accepted only at creation and
// selects Native. Persisted Rooms always require an explicit host mode.
type HostMode string

const (
	HostEmbedded HostMode = "embedded"
	HostNative   HostMode = "native"
)

func (m HostMode) Valid() bool { return m == HostEmbedded || m == HostNative }
func (m HostMode) ForCreation() HostMode {
	if m == "" {
		return HostNative
	}
	return m
}
