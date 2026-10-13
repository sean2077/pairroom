package sqlstore

import (
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

const (
	bindingRequest     = "request"
	bindingDecide      = "decide"
	bindingComplete    = "complete"
	bindingRead        = "read"
	bindingList        = "list"
	bindingContextRead = "context"
	bindingRenew       = "renew"
	bindingRequestTTL  = 10 * time.Minute
	bindingGrantTTL    = 24 * time.Hour
)

// BindingGrantCandidate is minted and retained privately by the endpoint
// before its request. Approval/receipts never return a token or its verifier.
type BindingGrantCandidate struct {
	GrantID   string
	TokenHash [32]byte `json:"-"`
}

// The Device chooses its own Agent, exact slot and observed generation. This
// consent is immutable; a browser approval cannot substitute another endpoint.
type BindingRequestCommand struct {
	Auth                                            AuthContext
	ClientID, RequestID, RoomID, AgentID, BindingID string
	Slot                                            model.ActorID
	ObservedGeneration                              uint64
	Replace, ParkEnabled                            bool
	ProofHash                                       [32]byte `json:"-"`
	Grant                                           BindingGrantCandidate
}

type BindingDecisionCommand struct {
	Auth                        AuthContext
	ClientID, RoomID, RequestID string
	Approve                     bool
}

type BindingCompletionCommand struct {
	Auth                        AuthContext
	ClientID, RoomID, RequestID string
	ProofHash                   [32]byte `json:"-"`
}

// Renewal changes only the narrow credential for an already admitted Binding.
// The new candidate and operation ID are saved by the endpoint before sending.
type BindingRenewalCommand struct {
	Auth                        AuthContext
	ClientID, RoomID, BindingID string
	Generation                  uint64
	Grant                       BindingGrantCandidate
}

// BindingRequestInfo is safe approval metadata. It contains neither vendor
// session/path information nor application proof/token hashes.
type BindingRequestInfo struct {
	RequestID          string            `json:"request_id"`
	RoomID             string            `json:"room_id"`
	Slot               model.ActorID     `json:"slot"`
	ObservedGeneration uint64            `json:"observed_generation"`
	Replace            bool              `json:"replace"`
	AgentID            string            `json:"agent_id"`
	Runtime            model.RuntimeKind `json:"runtime"`
	BindingID          string            `json:"binding_id"`
	UserID             string            `json:"user_id"`
	DeviceID           string            `json:"device_id"`
	ParkEnabled        bool              `json:"park_enabled"`
	Status             string            `json:"status"`
	RequestedAt        time.Time         `json:"requested_at"`
	ExpiresAt          time.Time         `json:"expires_at"`
	ApprovedBy         string            `json:"approved_by,omitempty"`
}

type BindingAuthorityReceipt struct {
	Version   int                      `json:"version"`
	Kind      string                   `json:"kind"`
	Request   *BindingRequestInfo      `json:"request,omitempty"`
	Binding   *model.EnterpriseBinding `json:"binding,omitempty"`
	GrantID   string                   `json:"grant_id,omitempty"`
	ExpiresAt time.Time                `json:"expires_at,omitzero"`
	Seq       uint64                   `json:"seq,omitempty"`
	Revision  uint64                   `json:"revision,omitempty"`
}

type BindingContext struct {
	RoomID           string                               `json:"room_id"`
	UserID           string                               `json:"user_id"`
	Role             string                               `json:"role"`
	OwnerUserID      string                               `json:"owner_user_id"`
	OwnershipPending bool                                 `json:"ownership_pending"`
	Slots            map[model.ActorID]BindingSlotContext `json:"slots"`
}

type BindingSlotContext struct {
	Slot           model.ActorID     `json:"slot"`
	HeadGeneration uint64            `json:"head_generation"`
	Vacant         bool              `json:"vacant"`
	BindingID      string            `json:"binding_id,omitempty"`
	AgentID        string            `json:"agent_id,omitempty"`
	Runtime        model.RuntimeKind `json:"runtime,omitempty"`
}
