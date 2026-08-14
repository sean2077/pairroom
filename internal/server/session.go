package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	browserSessionCookie = "pairroom_session"
	csrfHeaderName       = "X-PairRoom-CSRF"
	defaultSessionTTL    = 12 * time.Hour
	maxBrowserSessions   = 64
)

type authMode uint8

const (
	authNone authMode = iota
	authBearer
	authBrowserSession
)

type authContextKey struct{}

type requestAuth struct {
	Mode    authMode
	Session browserSession
}

type browserSession struct {
	ID        string    `json:"-"`
	CSRF      string    `json:"csrf_token"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	LastSeen  time.Time `json:"-"`
}

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]browserSession
	ttl      time.Duration
	now      func() time.Time
}

func newSessionStore() *sessionStore {
	return &sessionStore{sessions: make(map[string]browserSession), ttl: defaultSessionTTL, now: time.Now}
}

func (s *sessionStore) create() (browserSession, error) {
	id, err := randomHex(32)
	if err != nil {
		return browserSession{}, err
	}
	csrf, err := randomHex(32)
	if err != nil {
		return browserSession{}, err
	}
	now := s.now().UTC()
	value := browserSession{ID: id, CSRF: csrf, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(s.ttl)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.sessions) >= maxBrowserSessions {
		var oldestID string
		var oldest time.Time
		for key, existing := range s.sessions {
			if oldestID == "" || existing.LastSeen.Before(oldest) {
				oldestID, oldest = key, existing.LastSeen
			}
		}
		delete(s.sessions, oldestID)
	}
	s.sessions[id] = value
	return value, nil
}

func (s *sessionStore) get(id string) (browserSession, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return browserSession{}, false
	}
	now := s.now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	value, ok := s.sessions[id]
	if !ok || !constantTimeEqual(value.ID, id) {
		return browserSession{}, false
	}
	value.LastSeen = now
	value.ExpiresAt = now.Add(s.ttl)
	s.sessions[id] = value
	return value, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.sessions, strings.TrimSpace(id))
	s.mu.Unlock()
}

func (s *sessionStore) pruneLocked(now time.Time) {
	for key, value := range s.sessions {
		if !now.Before(value.ExpiresAt) {
			delete(s.sessions, key)
		}
	}
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

func authFromContext(ctx context.Context) requestAuth {
	value, _ := ctx.Value(authContextKey{}).(requestAuth)
	return value
}

func withAuth(r *http.Request, value requestAuth) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), authContextKey{}, value))
}

func setBrowserSessionCookie(w http.ResponseWriter, r *http.Request, name string, value browserSession) {
	maxAge := int(time.Until(value.ExpiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value.ID,
		Path:     "/api/v1/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearBrowserSessionCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/api/v1/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteStrictMode,
	})
}
