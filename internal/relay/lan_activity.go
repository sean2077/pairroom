package relay

// noteLANActivityLocked records a live, authenticated native operation. The
// same LAN certificate also serves owner views and the optional wake observer;
// their authentication alone is not evidence of native progress. In particular,
// read-only polling and recovery of an old ACK cannot consume a Codex nudge.
//
// Like local relay activity and Turn-end observations, this is transient. No
// event is appended for a heartbeat; replay retains spent wake reservations and
// conservatively requires a fresh Turn-end/progress observation when necessary.
func (e *Engine) noteLANActivityLocked(a Auth) {
	b := e.bindings[a.Slot]
	if b.RemoteKey == "" || !b.Active {
		return
	}
	if now := e.cfg.Now(); now.After(b.LastActivity) {
		b.LastActivity = now
		e.bindings[a.Slot] = b
	}
}

// ConfirmLAN confirms an already authenticated remote native caller without
// receiving its private native session ID or transcript path. The local client
// validates those details before this request. Inspect remains observational.
func (e *Engine) ConfirmLAN(a Auth) (Binding, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b, err := e.auth(a, false)
	if err != nil {
		return Binding{}, err
	}
	if b.RemoteKey == "" {
		return Binding{}, ErrAuth
	}
	e.noteLANActivityLocked(a)
	return e.bindings[a.Slot].Binding, nil
}
