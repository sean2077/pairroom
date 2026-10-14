package sqlstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"github.com/sean2077/pairroom/internal/attachment"
	"github.com/sean2077/pairroom/internal/model"
)

func attachmentReadOnly(kind string) bool {
	return kind == "read" || kind == "upload" || kind == "candidates" || kind == "gc.read"
}
func (s *Store) attachmentPlan(auth AuthContext, roomID, clientID string, m attachmentMutation) (opPlan, string) {
	if !validRoomCreateIdentity(clientID) {
		return opPlan{}, ClassInvalidRequest
	}
	if !m.operator {
		if auth.Deployment != s.cfg.DeploymentID || !validRoomCreateIdentity(auth.UserID) || !validRoomCreateIdentity(roomID) || auth.DeviceGrantID != "" || (auth.SessionID != "" && auth.BindingGrantID != "") {
			return opPlan{}, ClassAuthorization
		}
	}
	if m.kind == "reserve" && m.spec.Validate() != nil {
		return opPlan{}, ClassInvalidRequest
	}
	if m.objectID != "" && !attachment.ValidObjectID(m.objectID) {
		return opPlan{}, ClassInvalidRequest
	}
	if (m.kind == "verify" || m.kind == "cancel" || m.kind == "read" || m.kind == "upload" || m.kind == "gc.read" || m.kind == "expire" || m.kind == "mark" || m.kind == "deleted") && !attachment.ValidObjectID(m.objectID) {
		return opPlan{}, ClassInvalidRequest
	}
	principal := auth.UserID
	if m.operator {
		principal = "operator"
	}
	key := OpKey{Deployment: s.cfg.DeploymentID, RoomID: roomID, Principal: principal, Identifier: clientID, OpKind: m.kind, Namespace: namespaceHuman}
	// Credential rotation does not change a browser draft's identity; Binding
	// uploads are tied to the original Binding identity by the locked scope.
	h := sha256.New()
	for _, v := range []string{"sqlstore.attachment.v1", key.Deployment, roomID, principal, m.kind, clientID, m.objectID, m.barrierID} {
		writePart(h, []byte(v))
	}
	b, _ := json.Marshal(m.spec)
	writePart(h, b)
	var hash [32]byte
	copy(hash[:], h.Sum(nil))
	return opPlan{auth: auth, userID: auth.UserID, key: key, hash: hash, attachment: &m}, ""
}
func (s *Store) runAttachment(ctx context.Context, auth AuthContext, roomID, clientID string, m attachmentMutation, resolve bool) (Result, error) {
	p, class := s.attachmentPlan(auth, roomID, clientID, m)
	if class != "" {
		return refused(class, nil)
	}
	if resolve {
		return s.resolveWithFallback(ctx, p)
	}
	return s.commitWithRestarts(ctx, p, model.Event{})
}
func (s *Store) ReserveAttachment(ctx context.Context, c AttachmentReserveCommand) (Result, error) {
	return s.runAttachment(ctx, c.Auth, c.RoomID, c.ClientID, attachmentMutation{kind: "reserve", spec: c.Spec}, false)
}
func (s *Store) ResolveAttachmentReservation(ctx context.Context, c AttachmentReserveCommand) (Result, error) {
	return s.runAttachment(ctx, c.Auth, c.RoomID, c.ClientID, attachmentMutation{kind: "reserve", spec: c.Spec}, true)
}
func (s *Store) VerifyAttachment(ctx context.Context, c AttachmentVerifyCommand) (Result, error) {
	return s.runAttachment(ctx, c.Auth, c.RoomID, c.ClientID, attachmentMutation{kind: "verify", objectID: c.ObjectID, verified: c.Verified}, false)
}
func (s *Store) ResolveAttachmentVerification(ctx context.Context, c AttachmentObjectCommand) (Result, error) {
	return s.runAttachment(ctx, c.Auth, c.RoomID, c.ClientID, attachmentMutation{kind: "verify", objectID: c.ObjectID}, true)
}
func (s *Store) CancelAttachment(ctx context.Context, c AttachmentObjectCommand) (Result, error) {
	return s.runAttachment(ctx, c.Auth, c.RoomID, c.ClientID, attachmentMutation{kind: "cancel", objectID: c.ObjectID}, false)
}
func (s *Store) ResolveAttachmentCancellation(ctx context.Context, c AttachmentObjectCommand) (Result, error) {
	return s.runAttachment(ctx, c.Auth, c.RoomID, c.ClientID, attachmentMutation{kind: "cancel", objectID: c.ObjectID}, true)
}
func (s *Store) ReadUploadReservation(ctx context.Context, auth AuthContext, roomID, objectID string) (AttachmentReservation, error) {
	r, err := s.runAttachment(ctx, auth, roomID, objectID, attachmentMutation{kind: "upload", objectID: objectID}, true)
	if err != nil {
		return AttachmentReservation{}, err
	}
	m := r.Attachment.Metadata
	return AttachmentReservation{ObjectID: objectID, RoomID: roomID, Spec: AttachmentSpec{Name: m.Name, Kind: m.Kind, Size: m.Size, SHA256: m.SHA256}, ExpiresAt: r.Attachment.ExpiresAt}, nil
}
func (s *Store) ReadAttachment(ctx context.Context, auth AuthContext, roomID, objectID string) (model.Attachment, error) {
	r, err := s.runAttachment(ctx, auth, roomID, objectID, attachmentMutation{kind: "read", objectID: objectID}, true)
	if err != nil {
		return model.Attachment{}, err
	}
	return r.Attachment.Metadata, nil
}
