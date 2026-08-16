package websession

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	CSRFHeaderName     = "X-PairRoom-CSRF"
	defaultSessionTTL  = 12 * time.Hour
	maxBrowserSessions = 64
)

type Session struct {
	id        string
	lastSeen  time.Time
	CSRFToken string
	CreatedAt time.Time
	ExpiresAt time.Time
}

func (s Session) ValidCSRF(candidate string) bool {
	return s.CSRFToken != "" && constantTimeEqual(strings.TrimSpace(candidate), s.CSRFToken)
}

type Store struct {
	mu         sync.Mutex
	sessions   map[string]Session
	cookieName string
	ttl        time.Duration
	now        func() time.Time
}

func New(cookieName string) (*Store, error) {
	cookieName = strings.TrimSpace(cookieName)
	if cookieName == "" {
		return nil, errors.New("browser session cookie name is required")
	}
	if err := (&http.Cookie{Name: cookieName, Value: "session"}).Valid(); err != nil {
		return nil, fmt.Errorf("invalid browser session cookie name %q: %w", cookieName, err)
	}
	return &Store{
		sessions: make(map[string]Session), cookieName: cookieName,
		ttl: defaultSessionTTL, now: time.Now,
	}, nil
}

func (s *Store) Create(w http.ResponseWriter, r *http.Request) (Session, error) {
	id, err := randomHex(32)
	if err != nil {
		return Session{}, err
	}
	csrf, err := randomHex(32)
	if err != nil {
		return Session{}, err
	}
	now := s.now().UTC()
	value := Session{id: id, CSRFToken: csrf, CreatedAt: now, lastSeen: now, ExpiresAt: now.Add(s.ttl)}
	s.mu.Lock()
	s.pruneLocked(now)
	if len(s.sessions) >= maxBrowserSessions {
		var oldestID string
		var oldest time.Time
		for key, existing := range s.sessions {
			if oldestID == "" || existing.lastSeen.Before(oldest) {
				oldestID, oldest = key, existing.lastSeen
			}
		}
		delete(s.sessions, oldestID)
	}
	s.sessions[id] = value
	s.mu.Unlock()
	s.setCookie(w, r, value)
	return value, nil
}

func (s *Store) Get(w http.ResponseWriter, r *http.Request) (Session, bool) {
	cookie, err := r.Cookie(s.cookieName)
	if err != nil {
		return Session{}, false
	}
	id := strings.TrimSpace(cookie.Value)
	if id == "" {
		return Session{}, false
	}
	now := s.now().UTC()
	s.mu.Lock()
	s.pruneLocked(now)
	value, ok := s.sessions[id]
	if ok && constantTimeEqual(value.id, id) {
		value.lastSeen = now
		value.ExpiresAt = now.Add(s.ttl)
		s.sessions[id] = value
	} else {
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		return Session{}, false
	}
	s.setCookie(w, r, value)
	return value, true
}

func (s *Store) Delete(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(s.cookieName); err == nil {
		s.mu.Lock()
		delete(s.sessions, strings.TrimSpace(cookie.Value))
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName, Value: "", Path: "/api/v1/", MaxAge: -1,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Store) pruneLocked(now time.Time) {
	for key, value := range s.sessions {
		if !now.Before(value.ExpiresAt) {
			delete(s.sessions, key)
		}
	}
}

func (s *Store) setCookie(w http.ResponseWriter, r *http.Request, value Session) {
	maxAge := int(value.ExpiresAt.Sub(s.now().UTC()).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName, Value: value.id, Path: "/api/v1/", MaxAge: maxAge,
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
}

func randomHex(bytes int) (string, error) {
	if bytes <= 0 {
		return "", errors.New("random byte count must be positive")
	}
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func constantTimeEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
