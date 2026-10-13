package sqlstore

import (
	"context"
	"errors"

	"github.com/sean2077/pairroom/internal/relay"
)

// RoomPort adapts the closed kernel to one Room engine's enterprise port. It
// borrows the Store: the deployment owner opens and closes the Store, and this
// adapter never closes it, the lease or the work connection.
type RoomPort struct {
	store *Store
}

// NewRoomPort builds the adapter over a borrowed Store.
func NewRoomPort(store *Store) *RoomPort {
	return &RoomPort{store: store}
}

var _ relay.EnterprisePort = (*RoomPort)(nil)

// CommitHumanSend maps the engine command onto the kernel's typed human_send
// operation; the kernel recomputes the intent hash itself.
func (p *RoomPort) CommitHumanSend(ctx context.Context, cmd relay.EnterpriseSendCommand) (relay.EnterpriseSendResult, error) {
	result, err := p.store.CommitHumanSend(ctx, HumanSendCommand{
		Auth:             AuthContext{Deployment: cmd.Auth.Deployment, UserID: cmd.Auth.UserID},
		RoomID:           cmd.RoomID,
		ClientID:         cmd.ClientID,
		Intent:           HumanSendIntent{To: cmd.Intent.To, Text: cmd.Intent.Text},
		ExpectedRevision: cmd.ExpectedRevision,
		Fact:             cmd.Fact,
	})
	return adaptSendResult(result, err)
}

// ResolveHumanSend maps the read-only verification.
func (p *RoomPort) ResolveHumanSend(ctx context.Context, auth relay.EnterpriseAuth, roomID, clientID string, intent relay.EnterpriseSendIntent) (relay.EnterpriseSendResult, error) {
	result, err := p.store.ResolveHumanSend(ctx, AuthContext{Deployment: auth.Deployment, UserID: auth.UserID}, roomID, clientID, HumanSendIntent{To: intent.To, Text: intent.Text})
	return adaptSendResult(result, err)
}

// ReadonlySnapshot maps the authenticated read-only snapshot.
func (p *RoomPort) ReadonlySnapshot(ctx context.Context, auth relay.EnterpriseAuth, roomID string) (relay.EnterpriseRoomSnapshot, error) {
	snapshot, err := p.store.ReadonlySnapshot(ctx, AuthContext{Deployment: auth.Deployment, UserID: auth.UserID}, roomID)
	if err != nil {
		return relay.EnterpriseRoomSnapshot{}, adaptPortError(err)
	}
	return relay.EnterpriseRoomSnapshot{
		RoomID:         snapshot.RoomID,
		DeploymentID:   snapshot.DeploymentID,
		OrgID:          snapshot.OrgID,
		CreationUserID: snapshot.CreationUserID,
		HostMode:       snapshot.HostMode,
		Events:         snapshot.Events,
		Seq:            snapshot.Seq,
		Revision:       snapshot.Revision,
	}, nil
}

// adaptSendResult converts the kernel result; the outcome vocabulary is shared
// by value, so the engine can branch on it directly.
func adaptSendResult(result Result, err error) (relay.EnterpriseSendResult, error) {
	out := relay.EnterpriseSendResult{
		Outcome:  string(result.Outcome),
		Fact:     result.Fact,
		Seq:      result.Seq,
		Revision: result.Revision,
	}
	if err != nil {
		return out, adaptPortError(err)
	}
	return out, nil
}

// adaptPortError converts a kernel error into the engine-facing class carrier.
func adaptPortError(err error) error {
	var kernelErr *KernelError
	if errors.As(err, &kernelErr) {
		return &relay.EnterpriseCommandError{Outcome: string(kernelErr.Outcome), Class: kernelErr.Class, Err: err}
	}
	return err
}
