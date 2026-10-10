package lanclient

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/sean2077/pairroom/internal/lanshare"
	"github.com/sean2077/pairroom/internal/nativeidentity"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
	"github.com/sean2077/pairroom/internal/relay"
)

const maxClients = 1024
const maxDeliveries = 4096
const maxWakeReservations = 4096

// DefaultRoot is the per-user joined-Room catalog below the operating system's
// user configuration directory.
func DefaultRoot() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "pairroom", "lan-clients"), nil
}

// Open and OpenAt are read-only constructors. Discovery of an absent client
// store creates no directories, certificate, admission or identity claim.
func Open() (*Store, error) {
	root, err := DefaultRoot()
	if err != nil {
		return nil, err
	}
	return OpenAt(root)
}

func OpenAt(root string) (*Store, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("LAN client store requires an absolute clean path")
	}
	if err := privatefile.CheckDirectory(root); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	identities, err := nativeidentity.OpenAt(filepath.Join(filepath.Dir(root), "native-identities"))
	if err != nil {
		return nil, err
	}
	return &Store{root: root, identities: identities, clients: map[string]*Client{}}, nil
}

func (s *Store) Root() string { return s.root }

// ID includes both the pinned host identity and the remote Room identity.
func ID(invite lanshare.Invite) string {
	return "lan_" + relay.Digest(invite.HostPin + "\x00" + invite.RoomID)[:32]
}

func (s *Store) client(id string) (*Client, error) {
	if !lanshare.ValidID(id) || !strings.HasPrefix(id, "lan_") {
		return nil, ErrInvalidState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.clients[id]; c != nil {
		return c, nil
	}
	c := &Client{store: s, id: id, dir: filepath.Join(s.root, id)}
	s.clients[id] = c
	return c, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Client, error) {
	c, err := s.client(id)
	if err != nil {
		return nil, err
	}
	if _, err := c.read(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) List(ctx context.Context) ([]Snapshot, error) {
	if err := privatefile.CheckDirectory(s.root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	entries, err := readClientDirectories(s.root)
	if err != nil {
		return nil, err
	}
	var result []Snapshot
	for _, e := range entries {
		c, err := s.client(e.Name())
		if err != nil {
			return nil, err
		}
		snapshot, err := c.Snapshot(ctx)
		if errors.Is(err, os.ErrNotExist) {
			continue
		} // interrupted first mkdir
		if err != nil {
			return nil, err
		}
		result = append(result, snapshot)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

// Discovery and admission count the same client directories. Unrelated OS or
// sync metadata cannot consume Room capacity; client-shaped files and symlinks
// still fail the private-record boundary. An interrupted first mkdir retains
// its capacity reservation even before client.json is committed.
func readClientDirectories(root string) ([]os.DirEntry, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var clients []os.DirEntry
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !lanshare.ValidID(name) || !strings.HasPrefix(name, "lan_") {
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, ErrInvalidState
		}
		clients = append(clients, entry)
		if len(clients) > maxClients {
			return nil, errors.New("LAN joined Room limit exceeded")
		}
	}
	return clients, nil
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		c.Close()
	}
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.http != nil {
		c.http.CloseIdleConnections()
	}
}

func validSession(session string) bool {
	return session != "" && len(session) <= 256 && strings.TrimSpace(session) == session && !strings.ContainsAny(session, "/\\") && !strings.ContainsFunc(session, unicode.IsControl)
}

func validateRecord(r record) error {
	if r.Schema != 1 || r.Invite.Validate() != nil || r.ID != ID(r.Invite) || !lanshare.ValidID(r.RequestID) || !lanshare.ValidID(r.BindID) || !lanshare.ValidFingerprint(r.CredentialHash) || !filepath.IsAbs(r.Workspace) || filepath.Clean(r.Workspace) != r.Workspace || !r.Runtime.Valid() || !validSession(r.SessionID) || len(r.TranscriptPath) > 4096 || r.TranscriptPath != "" && !filepath.IsAbs(r.TranscriptPath) {
		return ErrInvalidState
	}
	if _, err := r.Identity.Fingerprint(); err != nil {
		return ErrInvalidState
	}
	if r.PreviousBindID != "" && (!lanshare.ValidID(r.PreviousBindID) || r.PreviousBindID == r.BindID) {
		return ErrInvalidState
	}
	switch r.Status {
	case "pending", "accepted", "revoked", "expired", "left", "detached":
	default:
		return ErrInvalidState
	}
	if r.Room != nil {
		if r.Room.RoomID != r.Invite.RoomID || !r.Room.Slot.ValidParticipant() || r.Room.Generation == 0 || !lanshare.ValidID(r.Room.BindID) || len(r.Room.Runtimes) != 2 || r.Room.Runtimes[r.Room.Slot] != r.Runtime || r.Room.Collaboration == nil || r.Room.Collaboration.Validate() != nil {
			return ErrInvalidState
		}
		for slot, kind := range r.Room.Runtimes {
			if !slot.ValidParticipant() || !kind.Valid() {
				return ErrInvalidState
			}
		}
	} else if r.Status == "accepted" {
		return ErrInvalidState
	}
	if r.Receipt != "" {
		receipt, err := lanshare.ParseReceipt(r.Receipt)
		fingerprint, keyErr := r.Identity.Fingerprint()
		if err != nil || keyErr != nil || receipt.RoomID != r.Invite.RoomID || receipt.RequestID != r.RequestID || receipt.Fingerprint != fingerprint {
			return ErrInvalidState
		}
	}
	if len(r.Deliveries) > maxDeliveries || len(r.Spent) > maxWakeReservations {
		return ErrInvalidState
	}
	seen := make(map[string]bool, len(r.Deliveries))
	for _, d := range r.Deliveries {
		if !lanshare.ValidID(d.ID) || !lanshare.ValidID(d.Receipt) || r.Room == nil || d.Generation != r.Room.Generation || seen[d.ID] {
			return ErrInvalidState
		}
		switch d.State {
		case "claimed", "stdout", "acknowledged", "unknown":
		default:
			return ErrInvalidState
		}
		seen[d.ID] = true
	}
	seen = make(map[string]bool, len(r.Spent))
	for _, spent := range r.Spent {
		if r.Room == nil || !lanshare.ValidID(spent.MessageID) || spent.Target != r.Room.Slot || spent.At.IsZero() || seen[spent.MessageID] {
			return ErrInvalidState
		}
		seen[spent.MessageID] = true
	}
	return nil
}

func readRecord(dir, id string) (record, error) {
	var r record
	if err := privatefile.ReadJSON(filepath.Join(dir, "client.json"), 2<<20, &r); err != nil {
		return r, err
	}
	if r.ID != id {
		return r, ErrInvalidState
	}
	return r, validateRecord(r)
}

// withRecord holds the stable directory lock, rereads the latest disk state,
// and writes only an actual change. Callbacks must not perform network I/O.
// Lock order is client record, then nativeidentity; neither acquires the other
// in reverse. Evidence and collector locks are separate from this short lock.
func (c *Client) withRecord(ctx context.Context, update func(*record) error) (record, error) {
	var r record
	if err := privatefile.CheckDirectory(c.store.root); err != nil {
		return r, err
	}
	if err := privatefile.CheckDirectory(c.dir); err != nil {
		return r, err
	}
	unlock, err := privatelock.Lock(ctx, c.dir)
	if err != nil {
		return r, err
	}
	defer unlock()
	r, err = readRecord(c.dir, c.id)
	if err != nil || update == nil {
		return r, err
	}
	before, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	if err := update(&r); err != nil {
		return r, err
	}
	if err := validateRecord(r); err != nil {
		return r, err
	}
	after, err := json.Marshal(r)
	if err != nil {
		return r, err
	}
	if !bytes.Equal(before, after) {
		err = privatefile.WriteJSON(filepath.Join(c.dir, "client.json"), r)
	}
	return r, err
}

func (c *Client) read(ctx context.Context) (record, error) { return c.withRecord(ctx, nil) }

func binding(r record) relay.Binding {
	b := relay.Binding{Runtime: r.Runtime, BindID: r.BindID, SessionID: r.SessionID, TranscriptPath: r.TranscriptPath, ParkEnabled: r.ParkEnabled, Active: r.Status == "accepted", LastActivity: r.LastActivity}
	if r.Room != nil {
		b.Slot, b.Generation = r.Room.Slot, r.Room.Generation
	}
	return b
}

func reservation(r record) nativeidentity.Claim {
	claim := nativeidentity.Claim{Runtime: r.Runtime, SessionID: r.SessionID, Association: nativeidentity.Remote(r.Invite.HostPin, r.Invite.RoomID), BindID: r.BindID}
	if r.Room != nil {
		claim.Generation = r.Room.Generation
	}
	return claim
}

func (c *Client) Metadata(ctx context.Context) (Metadata, error) {
	r, err := c.read(ctx)
	if err != nil {
		return Metadata{}, err
	}
	b := binding(r)
	return Metadata{ID: r.ID, Invite: r.Invite, Workspace: r.Workspace, Runtime: r.Runtime, SessionID: r.SessionID, BindID: r.BindID, Generation: b.Generation, Slot: b.Slot, Status: r.Status, Binding: b, PreviousBindID: r.PreviousBindID}, nil
}

func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	r, err := c.read(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	c.mu.Lock()
	connected, lastSeen := c.connected, c.lastSeen
	c.mu.Unlock()
	ownerKey, _ := r.Identity.Fingerprint()
	s := Snapshot{ID: r.ID, RemoteRoomID: r.Invite.RoomID, Workspace: r.Workspace, Runtime: r.Runtime, Status: r.Status, Connected: connected, LastSeen: lastSeen, HostPin: r.Invite.HostPin, Endpoint: r.Invite.Endpoint, OwnerKey: ownerKey}
	if r.Room != nil {
		s.Name, s.Slot, s.Generation = r.Room.Name, r.Room.Slot, r.Room.Generation
	}
	return s, nil
}

func authenticate(r record, auth relay.Auth, leaving bool) error {
	if r.BindID != auth.BindID || r.SessionID != auth.SessionID || subtle.ConstantTimeCompare([]byte(r.CredentialHash), []byte(relay.Digest(auth.Secret))) != 1 {
		return relay.ErrAuth
	}
	if r.Room != nil {
		if r.Room.Slot != auth.Slot || r.Room.Generation != auth.Generation {
			return relay.ErrAuth
		}
	} else if !leaving || auth.Generation != 0 {
		return relay.ErrAuth
	}
	if r.Status != "accepted" && !leaving {
		return relay.ErrAuth
	}
	return nil
}

func (c *Client) authenticated(ctx context.Context, auth relay.Auth, leaving bool) (record, error) {
	return c.withRecord(ctx, func(r *record) error {
		if err := authenticate(*r, auth, leaving); err != nil {
			return err
		}
		if leaving {
			return nil
		}
		return c.store.identities.Check(ctx, reservation(*r))
	})
}

// Detach is an explicit local-only retirement. It does not claim that the host
// received a leave request. The exact local identity reservation is released
// before the record is retired, so a failure that must be reported — including
// a claim now held by a replacement binding — leaves the accepted/pending
// record intact instead of reporting failure for a half-retired binding.
func (c *Client) Detach(ctx context.Context, auth relay.Auth) error {
	return c.detach(ctx, &auth)
}

func (c *Client) detach(ctx context.Context, auth *relay.Auth) error {
	_, err := c.withRecord(ctx, func(r *record) error {
		if auth != nil {
			if err := authenticate(*r, *auth, true); err != nil {
				return err
			}
		}
		// Repair only a pending-to-admitted interruption for this exact local
		// binding before releasing it; a different owner remains protected.
		if err := c.store.identities.Reserve(ctx, reservation(*r)); err != nil {
			return err
		}
		if err := c.store.identities.Release(ctx, reservation(*r)); err != nil {
			return err
		}
		// withRecord persists the retired status after this callback succeeds.
		r.Status = "detached"
		return nil
	})
	return err
}
