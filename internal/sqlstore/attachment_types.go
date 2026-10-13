package sqlstore

import (
	"github.com/sean2077/pairroom/internal/enterpriseobject"
	"github.com/sean2077/pairroom/internal/model"
	"time"
)

type AttachmentSpec = enterpriseobject.Spec

// AttachmentLimits are operator configuration, never request authority. All
// zero disables new reservations; existing authorized objects remain readable.
type AttachmentLimits struct {
	DeploymentBytes   int64
	RoomBytes         int64
	DeploymentPending int64
	RoomPending       int64
}

func (l AttachmentLimits) valid() bool {
	return l == (AttachmentLimits{}) || l.DeploymentBytes > 0 && l.RoomBytes > 0 && l.RoomBytes <= l.DeploymentBytes && l.DeploymentPending > 0 && l.RoomPending > 0 && l.RoomPending <= l.DeploymentPending
}

const AttachmentReservationTTL = 30 * time.Minute
const AttachmentReclaimGrace = 7 * 24 * time.Hour

type AttachmentReserveCommand struct {
	Auth             AuthContext
	RoomID, ClientID string
	Spec             AttachmentSpec
}
type AttachmentVerifyCommand struct {
	Auth                       AuthContext
	RoomID, ClientID, ObjectID string
	Verified                   enterpriseobject.Verified
}
type AttachmentObjectCommand struct {
	Auth                       AuthContext
	RoomID, ClientID, ObjectID string
}

type AttachmentReceipt struct {
	Version   int              `json:"version"`
	Kind      string           `json:"kind"`
	RoomID    string           `json:"room_id,omitempty"`
	ObjectID  string           `json:"object_id,omitempty"`
	State     string           `json:"state,omitempty"`
	Metadata  model.Attachment `json:"metadata"`
	ExpiresAt time.Time        `json:"expires_at,omitempty"`
	BarrierID string           `json:"barrier_id,omitempty"`
}
type AttachmentReservation struct {
	ObjectID, RoomID string
	Spec             AttachmentSpec
	ExpiresAt        time.Time
}
type AttachmentCollectionReport struct{ Examined, Abandoned, Deleted int }

type attachmentMutation struct {
	kind, objectID, barrierID string
	spec                      AttachmentSpec
	verified                  enterpriseobject.Verified
	operator                  bool
	limit                     int
}
type attachmentState struct {
	missing                                     bool
	roomID, ownerID, bindingID, state           string
	meta                                        model.Attachment
	expiresAt, abandonedAt, markedAt, deletedAt time.Time
	expired, collectible, epochMatches          bool
}
type attachmentDependencies struct {
	authority                                                  dependencyState
	object                                                     attachmentState
	deploymentBytes, roomBytes, deploymentPending, roomPending int64
	barrier                                                    bool
}

// HumanMessageCommand is the public closed producer, sharing the old
// human_send key, table and empty-attachment intent hash exactly.
type HumanMessageCommand struct {
	Auth             AuthContext
	RoomID, ClientID string
	ExpectedRevision uint64
	Intent           HumanSendIntent
}
