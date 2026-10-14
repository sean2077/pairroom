package relay

// ProjectEnterpriseHistory replays one already authorized, fenced snapshot
// through the existing Relay apply and bounded history implementation. It has
// no Store, clock, lease expiry, notification or native effect.
func ProjectEnterpriseHistory(snapshot EnterpriseRoomSnapshot, query HistoryQuery) (HistoryPage, error) {
	view, err := enterpriseOperationView(snapshot)
	if err != nil {
		return HistoryPage{}, err
	}
	return view.historyLocked(query)
}
