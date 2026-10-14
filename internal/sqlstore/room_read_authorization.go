package sqlstore

import "context"

// AuthorizeRoomRead rechecks authority immediately before a buffered response
// is released. It uses only live metadata under the ordinary ownership fence;
// it never allocates or replays Room events or returns a stored receipt.
func (s *Store) AuthorizeRoomRead(ctx context.Context, auth AuthContext, roomID string) error {
	if auth.Deployment != s.cfg.DeploymentID || !validToken(auth.UserID) || !validToken(roomID) || auth.DeviceGrantID != "" || auth.BindingGrantID != "" && auth.SessionID != "" {
		return refusedError(ClassAuthorization, nil)
	}
	plan := opPlan{auth: auth, userID: auth.UserID, key: OpKey{Deployment: s.cfg.DeploymentID, RoomID: roomID}, roomReadAuthorization: true}
	_, err := s.resolveWithFallback(ctx, plan)
	return err
}
