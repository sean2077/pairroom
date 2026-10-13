package sqlstore

import "time"

const (
	CredentialBrowser          = "browser"
	CredentialDevice           = "device"
	CredentialBinding          = "binding"
	ControlDeviceEnrollment    = "device.enrollment.begin"
	ControlDeviceDecision      = "device.enrollment.decide"
	ControlDeviceComplete      = "device.enrollment.complete"
	ControlHeadlessDecision    = "device.enrollment.headless.decide"
	ControlHeadlessComplete    = "device.enrollment.headless.complete"
	controlHeadlessPoll        = "device.enrollment.headless.poll"
	DeviceTransportLoopback    = "loopback"
	DeviceTransportHeadless    = "headless"
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
	TransportMode                                                string
	ConfirmationHash                                             [32]byte `json:"-"`
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
	TransportMode, CandidateFingerprint                                  string
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

type HeadlessEnrollmentDecision struct {
	Auth                AuthContext
	ClientID, RequestID string
	Approve             bool
	ConfirmationHash    [32]byte `json:"-"`
}
type HeadlessEnrollmentPoll struct {
	RequestID, Origin string
	ProofHash         [32]byte `json:"-"`
	Sequence          uint64
}
type HeadlessEnrollmentCompletion struct {
	RequestID, ClientID, Origin string
	ProofHash, ConfirmationHash [32]byte `json:"-"`
}
type HeadlessEnrollmentResult struct {
	RequestID, Status, Disposition string
	Sequence                       uint64
	ExpiresAt, NextPollAt          time.Time
}

func (r Result) HeadlessEnrollmentResult() (HeadlessEnrollmentResult, bool) {
	if r.headless == nil {
		return HeadlessEnrollmentResult{}, false
	}
	return *r.headless, true
}
func (DeviceEnrollmentCommand) String() string        { return "<private-device-input>" }
func (DeviceEnrollmentCommand) GoString() string      { return "<private-device-input>" }
func (HeadlessEnrollmentDecision) String() string     { return "<private-device-input>" }
func (HeadlessEnrollmentDecision) GoString() string   { return "<private-device-input>" }
func (HeadlessEnrollmentPoll) String() string         { return "<private-device-input>" }
func (HeadlessEnrollmentPoll) GoString() string       { return "<private-device-input>" }
func (HeadlessEnrollmentCompletion) String() string   { return "<private-device-input>" }
func (HeadlessEnrollmentCompletion) GoString() string { return "<private-device-input>" }
