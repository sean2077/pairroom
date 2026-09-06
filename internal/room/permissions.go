package room

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/model"
)

const EventPermissionsUpdated = "participant.permissions.updated"

type permissionsUpdate struct {
	Actor   model.ActorID           `json:"actor"`
	Profile model.PermissionProfile `json:"profile"`
}

func (e *Engine) SnapshotMeta() model.RoomMeta {
	e.mu.RLock()
	defer e.mu.RUnlock()
	meta := e.snapshot.Meta
	meta.Collaboration = model.CloneCollaboration(meta.Collaboration)
	return meta
}

func nativePermissionRole(p model.ParticipantSnapshot) model.ParticipantRole {
	if p.PermissionProfile == "" {
		return p.Role
	} // schema-9 legacy permission boundary
	if p.PermissionProfile == model.PermissionReadOnly {
		return model.RoleReviewer
	}
	return model.RolePeer
}

func (e *Engine) configureParticipant(cfg agent.Config, p model.ParticipantSnapshot) agent.Config {
	cfg.Collaboration = e.SnapshotMeta().Collaboration
	if cfg.Collaboration == nil {
		cfg.LegacyRole = p.Role
		return cfg
	}
	cfg.LegacyRole = ""
	return agent.PermissionConfig(cfg, p.PermissionProfile)
}

// SetPermissions changes only the native policy at an idle Room boundary. It
// cannot rewrite mode, responsibility, or any creation-time instructions.
func (e *Engine) SetPermissions(ctx context.Context, actor model.ActorID, profile model.PermissionProfile) error {
	if !actor.ValidParticipant() || !profile.Valid() {
		return errors.New("invalid participant or permission profile")
	}
	if e.SnapshotMeta().Collaboration == nil {
		return errors.New("legacy Room policy is preserved; create a new Room to use independent permissions")
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	unlock, err := e.lockAllDeliveries(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	e.routingMu.Lock()
	defer e.routingMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	e.turnMu.Lock()
	busy := e.turnOwner != "" || e.turnSubmitting > 0 || len(e.turnQueue) > 0
	e.turnMu.Unlock()
	if busy {
		return errors.New("stop or finish the active Turn and queued work before changing permissions")
	}
	e.mu.RLock()
	if e.closed {
		e.mu.RUnlock()
		return errors.New("room is closed")
	}
	current := e.snapshot.Participants[actor]
	for _, p := range e.snapshot.Participants {
		if err := roleChangeSafe(p); err != nil {
			e.mu.RUnlock()
			return errors.New("both participants must be idle before changing permissions")
		}
	}
	for _, approval := range e.snapshot.Approvals {
		if approval.Status == "pending" {
			e.mu.RUnlock()
			return errors.New("resolve pending approvals before changing permissions")
		}
	}
	e.mu.RUnlock()
	if current.PermissionProfile == profile {
		return nil
	}
	old, err := e.adapter(actor)
	if err != nil {
		return err
	}
	wasRunning := old.State() != model.StateStopped
	// Record human intent before process effects, but leave the effective policy
	// unchanged when the old process cannot be stopped.
	update := permissionsUpdate{Actor: actor, Profile: profile}
	if _, err := e.record("participant.permissions.requested", model.ActorUser, update); err != nil {
		return err
	}
	if err := old.Stop(ctx); err != nil {
		return fmt.Errorf("stop participant before permission change: %w", err)
	}
	e.expireApprovals(actor, "permissions_changed")
	e.mu.RLock()
	current = e.snapshot.Participants[actor]
	e.mu.RUnlock()
	cfg := slotAgentConfig(e.cfg, actor)
	cfg.Actor = actor
	cfg.Runtime = cfg.Runtime.CanonicalForSlot(actor)
	cfg.PeerRuntime = slotAgentConfig(e.cfg, model.OtherParticipant(actor)).Runtime.CanonicalForSlot(model.OtherParticipant(actor))
	meta := e.SnapshotMeta()
	cfg.Repo, cfg.DataDir, cfg.RoomName = current.Workspace.Path, e.cfg.Store.Dir(), meta.Name
	if cfg.Repo == "" {
		cfg.Repo = meta.Repo
	}
	cfg.SessionID = current.SessionID
	if cfg.SessionID != "" {
		cfg.RequireExactSession = true
	}
	current.PermissionProfile = profile
	cfg = e.configureParticipant(cfg, current)
	factory := e.cfg.ClaudeFactory
	if actor == model.ActorCodex {
		factory = e.cfg.CodexFactory
	}
	next := factory(cfg, e.HandleRuntimeEvent)
	if err := next.SetRole(ctx, nativePermissionRole(current)); err != nil {
		return err
	}
	// No native process is running at this commit boundary. Failure to persist
	// leaves the old, stopped adapter; failure to restart retains the new policy
	// and reports an error, never falls back to broader permissions.
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := e.record(EventPermissionsUpdated, model.ActorUser, update); err != nil {
		return err
	}
	e.mu.Lock()
	e.adapters[actor] = next
	e.mu.Unlock()
	e.updateParticipant(actor, func(p *model.ParticipantSnapshot) {
		p.State = model.StateStopped
		p.CurrentTurn = ""
		p.LastError = ""
		p.LastActivity = time.Now().UTC()
		applyRoleRuntimeProjection(p, actor, p.Role, e.cfg)
	})
	if wasRunning {
		return e.startAgentLocked(ctx, actor)
	}
	return nil
}
