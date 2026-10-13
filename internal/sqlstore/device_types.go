package sqlstore

import "time"

const (
	CredentialBrowser          = "browser"
	CredentialDevice           = "device"
	CredentialBinding          = "binding"
	ControlDeviceEnrollment    = "device.enrollment.begin"
	ControlDeviceDecision      = "device.enrollment.decide"
	ControlDeviceComplete      = "device.enrollment.complete"
	ControlDeviceRotate        = "device.rotate"
	ControlDeviceRecover       = "device.recover"
	ControlDeviceRevoke        = "device.revoke"
	ControlAgentRegister       = "agent.register"
	controlDeviceAuthenticate  = "device.authenticate"
	controlBindingAuthenticate = "binding.authenticate"
	controlEnrollmentRead      = "device.enrollment.read"
)

// Candidates are generated and durably saved by the native client before any
// request. Only hashes cross this API; accepted receipts never contain secrets.
type DeviceCredentialCandidate struct {
	GrantID     string
	AccessHash  [32]byte `json:"-"`
	RefreshHash [32]byte `json:"-"`
}

type DeviceEnrollmentCommand struct {
	RequestID, DeviceID, Label, Origin, CallbackURI, ClientState string
	ProofHash                                                    [32]byte `json:"-"`
	Credential                                                   DeviceCredentialCandidate
}

type DeviceEnrollmentDecision struct {
	Auth                AuthContext
	ClientID, RequestID string
	Approve             bool
	CodeHash            [32]byte `json:"-"`
}

type DeviceEnrollmentCompletion struct {
	RequestID, ClientID, Origin string
	ProofHash, CodeHash         [32]byte `json:"-"`
}

type DeviceEnrollmentInfo struct {
	RequestID, DeviceID, Label, Origin, CallbackURI, ClientState, Status string
	ExpiresAt                                                            time.Time
}

type DeviceRotationCommand struct {
	DeviceID, CurrentGrantID, RotationID, Origin string
	ClientProofHash, CurrentRefreshHash          [32]byte `json:"-"`
	Next                                         DeviceCredentialCandidate
}

type DeviceCredential struct {
	Auth                        AuthContext
	DeviceID, GrantID           string
	ExpiresAt, RefreshExpiresAt time.Time
}

type BindingCredential struct {
	Auth                                                AuthContext
	DeviceID, AgentID, RoomID, BindingID, GrantID, Slot string
	Generation                                          uint64
	ExpiresAt                                           time.Time
}
