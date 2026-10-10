package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/lanclient"
	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/privatefile"
)

// The optional Service observes the same per-user client records as direct
// CLI callers. It owns no guest certificate, membership or receipt journal.
type lanGuestManager struct {
	mu     sync.Mutex
	server *ManagementServer
	store  *lanclient.Store
	// storeErr is set when the per-user catalog could not be opened. It keeps
	// the Service running: host Rooms, hooks and this root's LAN host identity
	// do not depend on the machine-wide joined-Room catalog.
	storeErr error
	guests   map[string]*lanGuest
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closed   bool
}

// errLANStoreUnavailable reports a per-user joined-Room catalog this build
// cannot use. Every guest surface reports it; nothing regenerates or discards
// the catalog automatically.
var errLANStoreUnavailable = errors.New("the local LAN client store is unusable; joined-Room views and the optional wake observer are unavailable until it is repaired or removed by hand")

type lanGuest struct {
	mu       sync.Mutex
	client   *lanclient.Client
	waker    *nativeWaker
	polling  bool
	nextPoll time.Time
	// pollDelay is the current backoff after a failed observation pass; it grows
	// to the larger of the healthy period and observerMaxBackoff, and clears
	// on the next successful pass.
	pollDelay time.Duration
}

type lanGuestSummary = lanclient.Snapshot

// Host transport identity directories retain their existing private boundary.
func lanGuestPrivateDir(root string, parts ...string) (string, error) {
	path := root
	for _, part := range parts {
		if !lanshare.ValidID(part) {
			return "", errors.New("invalid LAN private directory")
		}
		path = filepath.Join(path, part)
		if err := privatefile.Mkdir(path); err != nil {
			return "", err
		}
	}
	return path, nil
}

func initLANGuests(s *ManagementServer) error {
	// An unusable per-user catalog must not stop the Service: this root's Rooms,
	// hooks and LAN host identity are independent of it, and the catalog's owner
	// validates it on use. The failure is reported in the Service snapshot and on
	// every guest surface.
	store, storeErr := lanclient.Open()
	if storeErr != nil {
		store = nil
	}
	ctx, cancel := context.WithCancel(s.streams)
	g := &lanGuestManager{server: s, store: store, storeErr: storeErr, guests: make(map[string]*lanGuest), ctx: ctx, cancel: cancel}
	s.lanGuests = g
	g.wg.Add(1)
	go g.maintain()
	return nil
}

// storeFailure reports why the per-user joined-Room catalog cannot be used.
func (g *lanGuestManager) storeFailure() error {
	if g == nil {
		return errLANStoreUnavailable
	}
	if g.store == nil {
		if g.storeErr != nil {
			return fmt.Errorf("%w: %v", errLANStoreUnavailable, g.storeErr)
		}
		return errLANStoreUnavailable
	}
	return nil
}

// withStore returns the per-user store, or the reason it is unusable.
func (g *lanGuestManager) withStore() (*lanclient.Store, error) {
	if err := g.storeFailure(); err != nil {
		return nil, err
	}
	return g.store, nil
}

// list returns the joined-Room snapshots or the catalog failure.
func (g *lanGuestManager) list(ctx context.Context) ([]lanclient.Snapshot, error) {
	store, err := g.withStore()
	if err != nil {
		return nil, err
	}
	return store.List(ctx)
}

// room returns one joined client for a local browser action.
func (g *lanGuestManager) room(ctx context.Context, id string) (*lanclient.Client, error) {
	store, err := g.withStore()
	if err != nil {
		return nil, err
	}
	return store.Get(ctx, id)
}

func (g *lanGuestManager) close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.closed = true
	g.cancel()
	g.mu.Unlock()
	g.wg.Wait()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, guest := range g.guests {
		if guest.waker != nil {
			guest.waker.Close()
		}
	}
	if g.store != nil {
		g.store.Close()
	}
}

func (g *lanGuestManager) get(id string) *lanGuest {
	if g == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
	defer cancel()
	client, err := g.room(ctx, id)
	if err != nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return nil
	}
	if guest := g.guests[id]; guest != nil {
		return guest
	}
	guest := &lanGuest{client: client}
	g.guests[id] = guest
	return guest
}

// summaries reads the optional catalog once within the caller's lifetime and
// the snapshot budget. A blocked client lock cannot stall hosted Room views.
func (g *lanGuestManager) summaries(ctx context.Context) ([]lanGuestSummary, string) {
	if g == nil {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	result, err := g.list(ctx)
	if err != nil {
		return nil, fmt.Sprintf("%s: %v", errLANStoreUnavailable, err)
	}
	return result, ""
}

func (s *ManagementServer) mountLANGuests(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/lan/joined", s.listLANJoinedRooms)
	mux.HandleFunc("POST /api/v1/lan/joined/{room}/{action}", s.lanGuestOwnerAction)
	mux.HandleFunc("GET /api/v1/lan/joined/{room}/attachments/{attachment}", s.lanGuestAttachment)
}

func (s *ManagementServer) listLANJoinedRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.lanGuests.list(r.Context())
	if err != nil {
		writeLANBridgeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}
