package relay

import (
	"context"
	"sync"

	"github.com/sean2077/pairroom/internal/model"
)

// LAN transfers hold their own effect lock, never the Room lock, while doing
// network I/O. A binding change first cancels and joins its old transfers, then
// commits the revocation fact. No old transfer can release bytes after that
// durable boundary, and an unrelated Room operation can proceed during I/O.
type lanTransferSet struct {
	active   map[*LANTransfer]struct{}
	stopping map[model.ActorID]int
}

type LANTransfer struct {
	mu        sync.Mutex
	engine    *Engine
	auth      Auth
	ctx       context.Context
	cancel    context.CancelFunc
	interrupt func()
	stopOnce  sync.Once
	closeOnce sync.Once
	stopWatch func() bool
	done      chan struct{}
}

// BeginLANTransfer registers one admitted attachment request. interrupt must
// promptly interrupt both request reads and response writes (for HTTP, set
// connection deadlines before closing the body). The caller must defer Close.
func (e *Engine) BeginLANTransfer(ctx context.Context, a Auth, interrupt func()) (*LANTransfer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, err := e.auth(a, false); err != nil {
		return nil, err
	}
	if a.MemberKey == "" || e.lanTransfers.stopping[a.Slot] != 0 {
		return nil, ErrAuth
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.lanTransfers.active == nil {
		e.lanTransfers.active = make(map[*LANTransfer]struct{})
	}
	derived, cancel := context.WithCancel(ctx)
	t := &LANTransfer{engine: e, auth: a, ctx: derived, cancel: cancel, interrupt: interrupt, done: make(chan struct{})}
	e.lanTransfers.active[t] = struct{}{}
	t.stopWatch = context.AfterFunc(ctx, t.stop)
	return t, nil
}

func (t *LANTransfer) Context() context.Context { return t.ctx }

// Effect revalidates the original certificate, binding and generation before
// each network write. The independent transfer barrier joins an already
// admitted write before allowing its membership to be revoked.
func (t *LANTransfer) Effect(effect func() error) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ctx.Err(); err != nil {
		return err
	}
	t.engine.mu.Lock()
	_, err := t.engine.auth(t.auth, false)
	if err == nil && t.engine.lanTransfers.stopping[t.auth.Slot] != 0 {
		err = ErrAuth
	}
	t.engine.mu.Unlock()
	if err != nil {
		return err
	}
	if err := t.ctx.Err(); err != nil {
		return err
	}
	return effect()
}

func (t *LANTransfer) stop() {
	t.stopOnce.Do(func() {
		t.cancel()
		if t.interrupt != nil {
			t.interrupt()
		}
	})
}

func (t *LANTransfer) Close() {
	t.closeOnce.Do(func() {
		t.stopWatch()
		t.mu.Lock()
		defer t.mu.Unlock()
		// Completion has no remaining network effect to interrupt. Setting an
		// expired deadline here can permanently cancel net/http's background
		// read and poison the keepalive connection for the following claim.
		// Claim the shared once without interrupting, or join a cancellation
		// callback that already started, before the handler resets deadlines.
		t.stopOnce.Do(t.cancel)
		t.engine.mu.Lock()
		delete(t.engine.lanTransfers.active, t)
		close(t.done)
		t.engine.mu.Unlock()
	})
}

// Only authentication identity participates in this comparison. Ordinary
// transient activity is allowed while a transfer is being interrupted.
type lanBindingIdentity struct {
	active                                       bool
	bindID, remoteKey, sessionID, credentialHash string
	generation                                   uint64
}

func lanIdentity(b bindingFact) lanBindingIdentity {
	return lanBindingIdentity{b.Active, b.BindID, b.RemoteKey, b.SessionID, b.CredentialHash, b.Generation}
}

// Called with e.mu held, before Registry.commitNativeBinding takes its locks.
// The caller's earlier authentication remains valid only if both Room binding
// identities still match after the unlocked wait (including the local owner).
func (e *Engine) quiesceLANTransfersLocked(next bindingFact) error {
	old := e.bindings[next.Slot]
	if !old.Active || old.RemoteKey == "" || lanIdentity(old) == lanIdentity(next) {
		return nil
	}
	var active []*LANTransfer
	for transfer := range e.lanTransfers.active {
		if transfer.auth.Slot == next.Slot && transfer.auth.BindID == old.BindID && transfer.auth.Generation == old.Generation && transfer.auth.MemberKey == old.RemoteKey {
			active = append(active, transfer)
		}
	}
	if len(active) == 0 {
		return nil
	}
	ownerBefore := lanIdentity(e.bindings[model.OtherParticipant(next.Slot)])
	memberBefore := lanIdentity(old)
	if e.lanTransfers.stopping == nil {
		e.lanTransfers.stopping = make(map[model.ActorID]int)
	}
	e.lanTransfers.stopping[next.Slot]++
	e.mu.Unlock()
	for _, transfer := range active {
		transfer.stop()
	}
	for _, transfer := range active {
		<-transfer.done
	}
	e.mu.Lock()
	e.lanTransfers.stopping[next.Slot]--
	if err := e.healthy(); err != nil {
		return err
	}
	if lanIdentity(e.bindings[next.Slot]) != memberBefore || lanIdentity(e.bindings[model.OtherParticipant(next.Slot)]) != ownerBefore {
		return ErrAuth
	}
	return nil
}
