package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type sessionDependency struct {
	required, provided, missing, revoked, expired, epochMismatch, userMismatch, orgDisabled bool
	userID, sessionID                                                                       string
	csrfHash                                                                                [32]byte
	expiresAt                                                                               time.Time
}

func (d sessionDependency) allowed() bool {
	return !d.required || d.provided && !d.missing && !d.revoked && !d.expired && !d.epochMismatch && !d.userMismatch && !d.orgDisabled
}

// readCredential is called inside every public command/receipt/snapshot
// transaction, after the User guard and before membership or Room guards.
// Authentication middleware alone cannot close a concurrent-revocation race.
func (s *Store) readCredential(ctx context.Context, q rowQuerier, auth AuthContext, lock string) (sessionDependency, error) {
	d := sessionDependency{required: s.cfg.RequireSessionAuth || auth.SessionID != "", provided: auth.SessionID != "", sessionID: auth.SessionID}
	if !d.required {
		return d, nil
	}
	if auth.SessionID == "" {
		d.missing = true
		return d, nil
	}
	var csrf []byte
	var epoch string
	var currentEpoch *string
	err := q.QueryRow(ctx, `SELECT user_id, csrf_hash, expires_at, revoked_at IS NOT NULL, expires_at <= pg_catalog.clock_timestamp(), restore_epoch, (SELECT restore_epoch FROM `+s.table("identity_policy")+` WHERE id), (SELECT o.disabled FROM `+s.table("organizations")+` o JOIN `+s.table("configured_org")+` c USING (org_id) WHERE c.id) FROM `+s.table("browser_sessions")+` WHERE session_id = $1`+lock, auth.SessionID).
		Scan(&d.userID, &csrf, &d.expiresAt, &d.revoked, &d.expired, &epoch, &currentEpoch, &d.orgDisabled)
	if errors.Is(err, pgx.ErrNoRows) {
		d.missing = true
		return d, nil
	}
	if err != nil {
		return sessionDependency{}, err
	}
	if len(csrf) != len(d.csrfHash) {
		return sessionDependency{}, &storedFactCorruption{err: fmt.Errorf("stored browser session verifier has invalid length")}
	}
	copy(d.csrfHash[:], csrf)
	d.expiresAt = d.expiresAt.UTC()
	d.epochMismatch = currentEpoch == nil || epoch != *currentEpoch
	d.userMismatch = auth.UserID != d.userID
	return d, nil
}

// AuthenticateSession uses the same bounded fenced read runner as Resolve.
// Its returned context is a selector for a credential, not an authorization
// cache: every later transaction rechecks that credential and its live User.
func (s *Store) AuthenticateSession(ctx context.Context, tokenHash [32]byte) (BrowserSession, error) {
	if tokenHash == ([32]byte{}) {
		return BrowserSession{}, refusedError(ClassAuthorization, nil)
	}
	plan := opPlan{control: &controlMutation{kind: controlAuthenticate, tokenHash: tokenHash}}
	result, err := s.resolveWithFallback(ctx, plan)
	if err != nil {
		return BrowserSession{}, err
	}
	if result.session == nil {
		return BrowserSession{}, refusedError(ClassStateInvalid, nil)
	}
	return *result.session, nil
}
