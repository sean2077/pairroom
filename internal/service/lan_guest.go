package service

import (
	"context"
	"errors"
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
	guests map[string]*lanGuest
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed bool
}

type lanGuest struct {
	mu      sync.Mutex
	client  *lanclient.Client
	waker   *nativeWaker
	polling bool
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
	store, err := lanclient.Open()
	if err != nil {
		return err
	}
	if _, err := store.List(s.streams); err != nil {
		store.Close()
		return err
	}
	ctx, cancel := context.WithCancel(s.streams)
	g := &lanGuestManager{server: s, store: store, guests: make(map[string]*lanGuest), ctx: ctx, cancel: cancel}
	s.lanGuests = g
	g.wg.Add(1)
	go g.maintain()
	return nil
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
	g.store.Close()
}

func (g *lanGuestManager) get(id string) *lanGuest {
	if g == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
	defer cancel()
	client, err := g.store.Get(ctx, id)
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

func (g *lanGuestManager) summaries() []lanGuestSummary {
	if g == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(g.ctx, 3*time.Second)
	defer cancel()
	result, err := g.store.List(ctx)
	if err != nil {
		return nil
	}
	return result
}

func (s *ManagementServer) mountLANGuests(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/lan/joined", s.listLANJoinedRooms)
	mux.HandleFunc("POST /api/v1/lan/joined/{room}/{action}", s.lanGuestOwnerAction)
	mux.HandleFunc("GET /api/v1/lan/joined/{room}/attachments/{attachment}", s.lanGuestAttachment)
}

func (s *ManagementServer) listLANJoinedRooms(w http.ResponseWriter, r *http.Request) {
	rooms, err := s.lanGuests.store.List(r.Context())
	if err != nil {
		writeLANBridgeError(w, err)
		return
	}
	writeManagementJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
}
