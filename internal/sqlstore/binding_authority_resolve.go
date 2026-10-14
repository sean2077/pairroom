package sqlstore

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/sean2077/pairroom/internal/model"
)

func (s *Store) resolveBindingPlan(ctx context.Context, tx pgx.Tx, plan opPlan, d *bindingDependencies) (Result, error) {
	if !bindingReadAllowed(plan, d) {
		return refused(ClassAuthorization, nil)
	}
	if plan.binding.kind == bindingContextRead {
		view := BindingContext{RoomID: plan.key.RoomID, UserID: plan.userID, Role: d.actorRole, OwnerUserID: d.owner, OwnershipPending: d.ownerMissing || d.ownerDisabled || d.ownerMemberMissing || d.ownerMemberDisabled || d.ownerRole != "owner", Slots: make(map[model.ActorID]BindingSlotContext, 2)}
		for _, slot := range model.SlotActors() {
			entry := BindingSlotContext{Slot: slot}
			if err := tx.QueryRow(ctx, `SELECT current_generation FROM `+s.table("room_slots")+` WHERE room_id=$1 AND slot=$2`, plan.key.RoomID, string(slot)).Scan(&entry.HeadGeneration); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return refused(ClassStoredFactUnreadable, nil)
				}
				return resolveIncomplete(ctx, err)
			}
			var generation uint64
			err := tx.QueryRow(ctx, `SELECT b.binding_id,b.agent_id,a.runtime_kind,b.generation FROM `+s.table("bindings")+` b JOIN `+s.table("agents")+` a USING(agent_id) WHERE b.room_id=$1 AND b.slot=$2 AND b.retired_at IS NULL`, plan.key.RoomID, string(slot)).Scan(&entry.BindingID, &entry.AgentID, &entry.Runtime, &generation)
			if errors.Is(err, pgx.ErrNoRows) {
				entry.Vacant = true
			} else if err != nil {
				return resolveIncomplete(ctx, err)
			} else if !entry.Runtime.Valid() || entry.Runtime != entry.Runtime.Canonical() || generation != entry.HeadGeneration {
				return refused(ClassStoredFactUnreadable, nil)
			}
			view.Slots[slot] = entry
		}
		return Result{Outcome: OutcomeReplayed, bindingContext: &view}, nil
	}
	if plan.binding.kind == bindingRead {
		if d.request.missing || d.request.info.RoomID != plan.key.RoomID {
			return refused(ClassAuthorization, nil)
		}
		info := d.request.info
		return Result{Outcome: OutcomeReplayed, BindingAuthority: &BindingAuthorityReceipt{Version: 1, Kind: bindingRead, Request: &info}}, nil
	}
	if plan.binding.kind == bindingList {
		family := ""
		if plan.auth.DeviceGrantID != "" {
			family = d.credential.familyID
		}
		// Native callers see only their own Device family. A browser caller
		// reached this point solely through the current live Room Owner guard.
		rows, err := tx.Query(ctx, `SELECT request_id FROM `+s.table("binding_requests")+` WHERE room_id=$1 AND ($2='' OR (user_id=$3 AND family_id=$2)) AND status IN ('pending','approved') AND expires_at>pg_catalog.clock_timestamp() ORDER BY requested_at,request_id LIMIT 32`, plan.key.RoomID, family, plan.userID)
		if err != nil {
			return resolveIncomplete(ctx, err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return resolveIncomplete(ctx, err)
			}
			ids = append(ids, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return resolveIncomplete(ctx, err)
		}
		requests := make([]BindingRequestInfo, 0, len(ids))
		for _, id := range ids {
			request, err := s.readBindingRequestState(ctx, tx, id)
			if err != nil {
				return bindingOperationFailure(s, ctx, Result{}, err, true)
			}
			if request.missing {
				return refused(ClassStateInvalid, nil)
			}
			if err := tx.QueryRow(ctx, `SELECT COALESCE(runtime_kind,'') FROM `+s.table("agents")+` WHERE agent_id=$1`, request.info.AgentID).Scan(&request.info.Runtime); err != nil {
				return resolveIncomplete(ctx, err)
			}
			requests = append(requests, request.info)
		}
		return Result{Outcome: OutcomeReplayed, bindingRequests: requests}, nil
	}
	plan.hash = bindingIntentHash(plan, d)
	result, found, err := s.lookupBindingOperation(ctx, tx, plan, d)
	if err != nil {
		return bindingOperationFailure(s, ctx, result, err, true)
	}
	if !found {
		return Result{Outcome: OutcomeNotCommitted}, nil
	}
	return result, nil
}
