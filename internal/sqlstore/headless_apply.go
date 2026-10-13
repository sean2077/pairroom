package sqlstore

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"math"
	"time"
)

func nullableConfirmation(c *DeviceEnrollmentCommand) any {
	if c.TransportMode == DeviceTransportHeadless {
		return c.ConfirmationHash[:]
	}
	return nil
}
func headlessFirstPoll(c *DeviceEnrollmentCommand, now time.Time) any {
	if c.TransportMode == DeviceTransportHeadless {
		return now.Add(5 * time.Second)
	}
	return nil
}
func headlessObservation(x deviceDependencies, status, disposition string) *HeadlessEnrollmentResult {
	if status == "polling_exhausted" || status == "verification_exhausted" {
		status = "invalidated"
	}
	if status == "confirmation_rejected" {
		status = "pending"
	}
	if status != "expired" && (x.status == "invalidated" || x.status == "denied") {
		status = x.status
	}
	return &HeadlessEnrollmentResult{RequestID: x.requestID, Status: status, Disposition: disposition, Sequence: x.pollSequence, ExpiresAt: x.expiresAt, NextPollAt: x.nextPollAt}
}
func (s *Store) applyHeadlessDecision(ctx context.Context, tx pgx.Tx, p opPlan, d *controlDependencies) (Result, error) {
	x, c := d.device, p.control.device.headlessDecision
	old, found, e := s.lookupControl(ctx, tx, p)
	if e != nil {
		var k *KernelError
		if errors.As(e, &k) {
			return old, e
		}
		return controlStatementFailure(s, ctx, e)
	}
	if found {
		if !validHeadlessDecisionReceipt(p, d, old.Control) {
			return refused(ClassStoredFactUnreadable, nil)
		}
		old.headless = headlessObservation(x, old.Control.Status, headlessDecisionDisposition(old.Control.Status))
		return old, nil
	}
	if x.status != "pending" && !(x.status == "approved" && !c.Approve) {
		return refused(ClassInvalidControlState, nil)
	}
	if d.revision >= math.MaxInt64 {
		return refused(ClassRangeExceeded, nil)
	}
	status, receiptStatus := "denied", "denied"
	count := x.confirmationFailures
	approval := x.approvalCount
	var owner, code any
	if x.ownerID != "" {
		owner = x.ownerID
	}
	if x.codeHash != ([32]byte{}) {
		code = x.codeHash[:]
	}
	if c.Approve {
		if count >= 5 {
			return refused(ClassInvalidControlState, nil)
		}
		if c.ConfirmationHash != x.confirmationHash {
			count++
			status = "pending"
			receiptStatus = "confirmation_rejected"
			if count == 5 {
				status = "invalidated"
				receiptStatus = "verification_exhausted"
			}
		} else {
			status = "approved"
			receiptStatus = "approved"
			approval = 1
			owner = p.auth.UserID
			code = x.confirmationHash[:]
		}
	}
	if _, e = tx.Exec(ctx, `UPDATE `+s.table("device_enrollments")+` SET status=$2,confirmation_failures=$3,approved_user_id=$4,code_hash=$5,approval_count=$6 WHERE request_id=$1`, x.requestID, status, count, owner, code, approval); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	r := ControlReceipt{Version: 1, Kind: ControlHeadlessDecision, Seq: d.revision + 1, UserID: p.auth.UserID, RequestID: x.requestID, DeviceID: x.deviceID, Status: receiptStatus, ExpiresAt: x.expiresAt}
	if e = s.appendControlResult(ctx, tx, p, r, p.auth.UserID); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	return Result{Outcome: OutcomeCommitted, Control: &r, headless: headlessObservation(x, status, headlessDecisionDisposition(receiptStatus))}, nil
}
func headlessDecisionDisposition(status string) string {
	if status == "confirmation_rejected" || status == "verification_exhausted" {
		return status
	}
	return ""
}
func validHeadlessDecisionReceipt(p opPlan, d *controlDependencies, r *ControlReceipt) bool {
	if r == nil || r.Kind != ControlHeadlessDecision || r.Version != 1 || r.Seq == 0 {
		return false
	}
	c := p.control.device.headlessDecision
	status := r.Status
	if c.Approve {
		if status != "approved" && status != "confirmation_rejected" && status != "verification_exhausted" {
			return false
		}
		if status == "approved" && c.ConfirmationHash != d.device.confirmationHash {
			return false
		}
	} else if status != "denied" {
		return false
	}
	expected := ControlReceipt{Version: 1, Kind: ControlHeadlessDecision, Seq: r.Seq, UserID: p.auth.UserID, RequestID: d.device.requestID, DeviceID: d.device.deviceID, Status: status, ExpiresAt: r.ExpiresAt}
	return expected == *r && r.ExpiresAt.Equal(d.device.expiresAt)
}
func (s *Store) resolveHeadlessPoll(p opPlan, d *controlDependencies) (Result, error) {
	x, c := d.device, p.control.device.poll
	if c.Sequence < x.pollSequence {
		return Result{Outcome: OutcomeUnknown, Class: "poll_observation_unavailable"}, nil
	}
	if c.Sequence > x.pollSequence {
		return Result{Outcome: OutcomeNotCommitted}, nil
	}
	if x.lastPollIntentHash != p.hash {
		return controlConflict()
	}
	if (x.status == "invalidated" && x.lastPollStatus != "polling_exhausted") || x.status == "denied" || x.expired {
		status := x.status
		if x.expired && (status != "invalidated" && status != "denied" || x.lastPollStatus == "polling_exhausted") {
			status = "expired"
		}
		return Result{Outcome: OutcomeReplayed, headless: headlessObservation(x, status, "")}, nil
	}
	return Result{Outcome: OutcomeReplayed, headless: &HeadlessEnrollmentResult{RequestID: x.requestID, Status: publicPollStatus(x.lastPollStatus), Disposition: pollDisposition(x.lastPollStatus), Sequence: x.pollSequence, ExpiresAt: x.expiresAt, NextPollAt: x.lastPollNextAt}}, nil
}
func pollDisposition(status string) string {
	switch status {
	case "polling_exhausted":
		return "polling_exhausted"
	}
	return ""
}
func (s *Store) applyHeadlessPoll(ctx context.Context, tx pgx.Tx, p opPlan, d *controlDependencies) (Result, error) {
	x, c := d.device, p.control.device.poll
	if c.Sequence <= x.pollSequence {
		return s.resolveHeadlessPoll(p, d)
	}
	if c.Sequence != x.pollSequence+1 {
		return refused(ClassInvalidRequest, nil)
	}
	var now time.Time
	if e := tx.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&now); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	now = now.UTC()
	status := x.status
	violation := x.pollViolations
	next := x.nextPollAt
	terminal := false
	// Live revocation/restore checks happened before TTL and rate observation.
	if status == "invalidated" || status == "denied" || status == "completed" {
	} else if !now.Before(x.expiresAt) {
		status = "expired"
	} else if c.Sequence > 120 || c.Sequence == 120 && x.status == "pending" {
		status = "polling_exhausted"
		terminal = true
	} else if now.Before(next) {
		violation++
		status = "slow_down"
		if violation >= 5 {
			status = "polling_exhausted"
			terminal = true
		}
	} else {
		next = now.Add(5 * time.Second)
		if next.After(x.expiresAt) {
			next = x.expiresAt
		}
	}
	state := x.status
	if terminal {
		state = "invalidated"
	}
	if _, e := tx.Exec(ctx, `UPDATE `+s.table("device_enrollments")+` SET status=$2,poll_sequence=$3,poll_violations=$4,next_poll_at=$5,last_poll_intent_hash=$6,last_poll_status=$7,last_poll_next_at=$5 WHERE request_id=$1`, x.requestID, state, c.Sequence, violation, next, p.hash[:], status); e != nil {
		return controlStatementFailure(s, ctx, e)
	}
	if violation > x.pollViolations || terminal {
		if e := s.writeMutationAudit(ctx, tx, p, "", "", "client", controlHeadlessPoll, "accepted", "", x.requestID, nil, now); e != nil {
			return controlStatementFailure(s, ctx, e)
		}
	}
	return Result{Outcome: OutcomeCommitted, headless: &HeadlessEnrollmentResult{RequestID: x.requestID, Status: publicPollStatus(status), Disposition: pollDisposition(status), Sequence: c.Sequence, ExpiresAt: x.expiresAt, NextPollAt: next}}, nil
}

func publicPollStatus(status string) string {
	if status == "polling_exhausted" {
		return "invalidated"
	}
	return status
}
