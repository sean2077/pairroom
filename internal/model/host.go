package model

// HostMode is immutable for a Room. Empty is accepted only at creation (the
// embedded default) or when reading the versioned provisioning-3 contract.
type HostMode string

const (
	HostEmbedded HostMode = "embedded"
	HostNative   HostMode = "native"
)

func (m HostMode) Valid() bool { return m == HostEmbedded || m == HostNative }
func (m HostMode) ForCreation() HostMode {
	if m == "" {
		return HostEmbedded
	}
	return m
}
