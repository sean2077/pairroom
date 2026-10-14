package sqlstore

import (
	"context"
	"errors"
	"time"
)

type approvalCutoffContextKey struct{}

func needsApprovalCutoff(plan opPlan) bool {
	if plan.control != nil {
		return plan.control.kind == controlUserStatus && !plan.control.enabled || plan.control.kind == ControlDeviceRevoke
	}
	if plan.roomMembership != nil {
		return membershipRevokes(*plan.roomMembership)
	}
	if plan.catalog != nil {
		return plan.catalog.kind == "room.archive" || plan.catalog.kind == "agent.retire"
	}
	return plan.governance != nil && plan.governance.kind == governanceTransfer
}
func captureApprovalCutoff(ctx context.Context, q rowQuerier, plan opPlan) (context.Context, error) {
	if !needsApprovalCutoff(plan) {
		return ctx, nil
	}
	var at time.Time
	if err := q.QueryRow(ctx, `SELECT pg_catalog.clock_timestamp()`).Scan(&at); err != nil {
		return nil, err
	}
	return context.WithValue(ctx, approvalCutoffContextKey{}, at.UTC()), nil
}
func approvalCutoff(ctx context.Context) (time.Time, error) {
	at, ok := ctx.Value(approvalCutoffContextKey{}).(time.Time)
	if !ok || at.IsZero() {
		return time.Time{}, errors.New("approval invalidation requires its captured database cutoff")
	}
	return at, nil
}
