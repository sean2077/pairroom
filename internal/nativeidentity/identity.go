// Package nativeidentity prevents a local native session from acquiring two
// Room associations across independent CLI and Service processes. Reservations
// are private durable local facts; remote hosts never receive session identity.
package nativeidentity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/privatefile"
	"github.com/sean2077/pairroom/internal/privatelock"
)

var ErrOwned = errors.New("native session is already reserved by a different Room association")
var ErrUnowned = errors.New("native session reservation is missing or no longer matches this binding")

type Claim struct {
	Runtime     model.RuntimeKind `json:"runtime"`
	SessionID   string            `json:"session_id"`
	Association string            `json:"association"`
	BindID      string            `json:"bind_id"`
	Generation  uint64            `json:"generation"`
}

func (c Claim) Valid() bool { return validClaim(c) }

type record struct {
	Schema   int   `json:"schema"`
	Claim    Claim `json:"claim"`
	Released bool  `json:"released,omitempty"`
}

type Store struct{ root string }

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func Hosted(root, room string, slot model.ActorID) string {
	root = filepath.Clean(root)
	if runtime.GOOS == "windows" {
		root = strings.ToLower(root)
	}
	return "host:" + digest(root+"\x00"+room+"\x00"+string(slot))
}

func Remote(hostPin, room string) string { return "lan:" + digest(hostPin+"\x00"+room) }

func Open() (*Store, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(base, "pairroom", "native-identities"))
}

func OpenAt(root string) (*Store, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, errors.New("native identity store requires an absolute clean path")
	}
	return &Store{root: root}, nil
}

func validClaim(c Claim) bool {
	prefix, suffix, ok := strings.Cut(c.Association, ":")
	_, err := hex.DecodeString(suffix)
	return c.Runtime.Valid() && c.SessionID != "" && len(c.SessionID) <= 512 && strings.TrimSpace(c.SessionID) == c.SessionID && !strings.ContainsAny(c.SessionID, "/\\") && !strings.ContainsFunc(c.SessionID, unicode.IsControl) && ok && (prefix == "host" || prefix == "lan") && len(suffix) == 64 && err == nil && c.BindID != "" && len(c.BindID) <= 128 && !strings.ContainsFunc(c.BindID, unicode.IsControl)
}

func identityKey(c Claim) string { return digest(string(c.Runtime) + "\x00" + c.SessionID) }

func (s *Store) directory(c Claim, create bool) (string, error) {
	if !validClaim(c) {
		return "", errors.New("invalid native session reservation")
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(s.root), 0o700); err != nil {
			return "", err
		}
		if err := privatefile.Mkdir(s.root); err != nil {
			return "", err
		}
	} else if err := privatefile.CheckDirectory(s.root); err != nil {
		return "", err
	}
	dir := filepath.Join(s.root, identityKey(c))
	var err error
	if create {
		err = privatefile.Mkdir(dir)
	} else {
		err = privatefile.CheckDirectory(dir)
	}
	return dir, err
}

func readClaim(dir string) (*Claim, error) {
	var r record
	err := privatefile.ReadJSON(filepath.Join(dir, "claim.json"), 4096, &r)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r.Schema != 1 || !validClaim(r.Claim) || identityKey(r.Claim) != filepath.Base(dir) {
		return nil, errors.New("invalid or unsupported native session reservation; state was not changed")
	}
	if r.Released {
		return nil, nil
	}
	return &r.Claim, nil
}

func sameOwner(a, b Claim) bool {
	return a.Runtime == b.Runtime && a.SessionID == b.SessionID && a.Association == b.Association && a.BindID == b.BindID
}

// Commit reserves next before a bounded local durable effect and releases
// previous only after that effect succeeds. A failed effect retains ownership
// because its durable outcome may be unknown. Never perform network I/O here.
func (s *Store) Commit(ctx context.Context, previous, next *Claim, effect func() error) error {
	return s.commit(ctx, previous, next, effect, false)
}

// ReplacePending is an explicit local re-admission operation. The caller must
// have retired the old client association and retain its old journal before
// replacing it with a fresh binding. An already released old claim is omitted
// so a later unrelated owner can never be touched during replacement.
func (s *Store) ReplacePending(ctx context.Context, previous *Claim, next Claim, effect func() error) error {
	if next.Generation != 0 || previous != nil && (next.BindID == previous.BindID || next.Association != previous.Association) {
		return ErrOwned
	}
	return s.commit(ctx, previous, &next, effect, true)
}

func (s *Store) commit(ctx context.Context, previous, next *Claim, effect func() error, newPending bool) error {
	if previous != nil && next != nil && *previous != *next && (previous.Association != next.Association || !newPending && next.Generation <= previous.Generation) {
		return ErrOwned
	}
	if previous == nil && next == nil {
		if effect != nil {
			return effect()
		}
		return nil
	}
	dirs := make(map[string]string)
	for _, c := range []*Claim{previous, next} {
		if c != nil {
			dir, err := s.directory(*c, true)
			if err != nil {
				return err
			}
			dirs[identityKey(*c)] = dir
		}
	}
	keys := make([]string, 0, len(dirs))
	for key := range dirs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var releases []func()
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	current := make(map[string]*Claim)
	for _, key := range keys {
		unlock, err := privatelock.Lock(ctx, dirs[key])
		if err != nil {
			return err
		}
		releases = append(releases, unlock)
		current[key], err = readClaim(dirs[key])
		if err != nil {
			return err
		}
	}
	if previous != nil {
		old := current[identityKey(*previous)]
		if old != nil && *old != *previous && (next == nil || *old != *next) {
			return ErrOwned
		}
	}
	if next != nil {
		old := current[identityKey(*next)]
		allowed := old == nil || *old == *next || sameOwner(*old, *next) && old.Generation == 0 && next.Generation > 0 || previous != nil && old != nil && *old == *previous && old.Association == next.Association
		if !allowed {
			return ErrOwned
		}
		if old == nil || *old != *next {
			if err := privatefile.WriteJSON(filepath.Join(dirs[identityKey(*next)], "claim.json"), record{Schema: 1, Claim: *next}); err != nil {
				return err
			}
		}
	}
	if effect != nil {
		if err := effect(); err != nil {
			return err
		}
	}
	if previous != nil && (next == nil || identityKey(*previous) != identityKey(*next)) {
		return privatefile.WriteJSON(filepath.Join(dirs[identityKey(*previous)], "claim.json"), record{Schema: 1, Claim: *previous, Released: true})
	}
	return nil
}

func (s *Store) Reserve(ctx context.Context, c Claim) error { return s.Commit(ctx, nil, &c, nil) }
func (s *Store) Release(ctx context.Context, c Claim) error { return s.Commit(ctx, &c, nil, nil) }

// ReleaseIfHeld reconciles an already durable retirement fact. A newer owner
// is never an error or a release target: only the exact original claim changes.
// Unlike Release it creates no directory for an absent historical claim.
func (s *Store) ReleaseIfHeld(ctx context.Context, c Claim) (bool, error) {
	dir, err := s.directory(c, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	unlock, err := privatelock.Lock(ctx, dir)
	if err != nil {
		return false, err
	}
	defer unlock()
	current, err := readClaim(dir)
	if err != nil || current == nil || *current != c {
		return false, err
	}
	if err := privatefile.WriteJSON(filepath.Join(dir, "claim.json"), record{Schema: 1, Claim: c, Released: true}); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) Check(ctx context.Context, c Claim) error {
	current, err := s.lookup(ctx, c)
	if err != nil {
		return err
	}
	if current == nil || *current != c {
		return ErrUnowned
	}
	return nil
}

// Available is an advisory preflight; Commit still checks under the same
// cross-process lock as reservation and local durable effect.
func (s *Store) Available(ctx context.Context, c Claim) error {
	current, err := s.lookup(ctx, c)
	if err != nil {
		return err
	}
	if current != nil && current.Association != c.Association {
		return ErrOwned
	}
	return nil
}

// Retained returns the exact existing claim for a known association. A
// checkpoint-only archived Room uses this to retain its original operation
// identity without inventing a relay binding from a derived checkpoint.
func (s *Store) Retained(ctx context.Context, probe Claim) (Claim, bool, error) {
	c, err := s.lookup(ctx, probe)
	if err != nil || c == nil {
		return Claim{}, false, err
	}
	if c.Association != probe.Association {
		return Claim{}, false, ErrOwned
	}
	return *c, true, nil
}

// Validate is a bounded read-only startup preflight. Invalid private ownership
// facts must be rejected before a Service performs cleanup or recovery writes.
func (s *Store) Validate(ctx context.Context) error {
	if err := privatefile.CheckDirectory(s.root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	f, err := os.Open(s.root)
	if err != nil {
		return err
	}
	defer f.Close()
	entries, err := f.ReadDir(16385)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > 16384 {
		return errors.New("native identity store exceeds its bounded startup limit")
	}
	for _, entry := range entries {
		key := entry.Name()
		if len(key) != 64 || key != strings.ToLower(key) || !entry.IsDir() {
			return errors.New("invalid native identity directory")
		}
		if _, err := hex.DecodeString(key); err != nil {
			return errors.New("invalid native identity directory")
		}
		dir := filepath.Join(s.root, key)
		if err := privatefile.CheckDirectory(dir); err != nil {
			return err
		}
		unlock, err := privatelock.Lock(ctx, dir)
		if err != nil {
			return err
		}
		_, err = readClaim(dir)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) lookup(ctx context.Context, c Claim) (*Claim, error) {
	dir, err := s.directory(c, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	unlock, err := privatelock.Lock(ctx, dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return readClaim(dir)
}

func Reserve(ctx context.Context, c Claim) error {
	s, err := Open()
	if err != nil {
		return err
	}
	return s.Reserve(ctx, c)
}
func Check(ctx context.Context, c Claim) error {
	s, err := Open()
	if err != nil {
		return err
	}
	return s.Check(ctx, c)
}
func Release(ctx context.Context, c Claim) error {
	s, err := Open()
	if err != nil {
		return err
	}
	return s.Release(ctx, c)
}
