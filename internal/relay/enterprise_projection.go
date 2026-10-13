package relay

// ProjectEnterpriseSnapshot builds the public Room projection from a SQL read
// that the caller has already fenced and authorized. It validates and replays
// through the same implementation as OpenEnterprise, without a Store, port,
// clock, callbacks, reap or mutable runtime. The returned Snapshot is data,
// not an authorization grant; callers must not accept request-supplied facts
// or use this function instead of the SQL credential and ownership checks.
func ProjectEnterpriseSnapshot(snapshot EnterpriseRoomSnapshot) (Snapshot, error) {
	if err := validateEnterpriseSnapshot(snapshot.RoomID, snapshot.DeploymentID, snapshot); err != nil {
		return Snapshot{}, err
	}
	projection := emptyEngine(Config{RoomID: snapshot.RoomID})
	projection.enterprise = true
	if err := projection.replaySnapshotLocked(snapshot); err != nil {
		return Snapshot{}, err
	}
	return projection.Snapshot(), nil
}

// ProjectEnterpriseSnapshotTail projects only the shared bounded browser
// window after strict read-only replay. It never builds a full Snapshot first.
func ProjectEnterpriseSnapshotTail(snapshot EnterpriseRoomSnapshot) (TailSnapshot, error) {
	projection, err := enterpriseOperationView(snapshot)
	if err != nil {
		return TailSnapshot{}, err
	}
	return projection.SnapshotTail(), nil
}
