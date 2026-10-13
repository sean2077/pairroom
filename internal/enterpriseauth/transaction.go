// Package enterpriseauth verifies configured OpenID Connect identities. It does
// not admit users, issue application sessions, or grant resource permissions.
package enterpriseauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrConfiguration          = errors.New("enterprise authentication configuration invalid")
	ErrProviderUnavailable    = errors.New("enterprise identity provider unavailable")
	ErrIdentityInvalid        = errors.New("enterprise identity verification failed")
	ErrTransactionInvalid     = errors.New("enterprise login transaction invalid or expired")
	ErrTransactionUnavailable = errors.New("enterprise login transaction unavailable")
	ErrTransactionCapacity    = errors.New("enterprise login transaction capacity reached")
)

type Purpose string

const (
	PurposeLogin          Purpose = "login"
	PurposeReauthenticate Purpose = "reauthenticate"
)

// LoginIntent is application-supplied intent, never callback query parameters.
// CorrelationID may reference a pending application operation; it grants nothing.
type LoginIntent struct {
	Purpose       Purpose
	ReturnPath    string
	CorrelationID string
}

// LoginTransaction is private server state. Nonce and PKCEVerifier must never be
// included in browser responses, logs, diagnostics, or application sessions.
type LoginTransaction struct {
	StateHash           [32]byte
	BrowserHash         [32]byte
	ProviderFingerprint [32]byte
	ConnectionID        string
	Nonce               string
	PKCEVerifier        string
	Intent              LoginIntent
	CreatedAt           time.Time
	ExpiresAt           time.Time
}

// TransactionStore must atomically consume a live browser-bound transaction.
// A wrong browser must not consume it. Success permanently removes it before any
// provider call, including when a subsequent token exchange fails or is unknown.
// Implementations must bound retained transactions and return no expired state.
// No database transaction or lock may be retained across these method returns.
type TransactionStore interface {
	Put(context.Context, LoginTransaction) error
	Consume(context.Context, [32]byte, [32]byte, time.Time) (LoginTransaction, error)
}

// MemoryStore is a bounded process-local implementation. A restart discards all
// pending logins; production persistence is supplied through TransactionStore.
type MemoryStore struct {
	mu           sync.Mutex
	capacity     int
	transactions map[[32]byte]LoginTransaction
}

func NewMemoryStore(capacity int) (*MemoryStore, error) {
	if capacity < 1 || capacity > 100000 {
		return nil, ErrConfiguration
	}
	return &MemoryStore{capacity: capacity, transactions: make(map[[32]byte]LoginTransaction)}, nil
}

func (s *MemoryStore) Put(ctx context.Context, tx LoginTransaction) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validTransaction(tx) {
		return ErrTransactionInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, pending := range s.transactions {
		if !pending.ExpiresAt.After(tx.CreatedAt) {
			delete(s.transactions, key)
		}
	}
	if _, exists := s.transactions[tx.StateHash]; exists {
		return ErrTransactionInvalid
	}
	if len(s.transactions) >= s.capacity {
		return ErrTransactionCapacity
	}
	s.transactions[tx.StateHash] = tx
	return nil
}

func (s *MemoryStore) Consume(ctx context.Context, stateHash, browserHash [32]byte, now time.Time) (LoginTransaction, error) {
	if err := ctx.Err(); err != nil {
		return LoginTransaction{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, ok := s.transactions[stateHash]
	if !ok {
		return LoginTransaction{}, ErrTransactionInvalid
	}
	if !tx.ExpiresAt.After(now) || tx.CreatedAt.After(now) {
		delete(s.transactions, stateHash)
		return LoginTransaction{}, ErrTransactionInvalid
	}
	if subtle.ConstantTimeCompare(tx.BrowserHash[:], browserHash[:]) != 1 {
		return LoginTransaction{}, ErrTransactionInvalid
	}
	delete(s.transactions, stateHash)
	return tx, nil
}

// NewBrowserSecret supplies entropy for a private, secure, HttpOnly browser
// binding cookie. Cookie scope, SameSite and CSRF policy belong to the HTTP layer.
func NewBrowserSecret() (string, error) { return randomSecret() }

func randomSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", ErrTransactionUnavailable
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func validSecret(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == 32 && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validIntent(intent LoginIntent) bool {
	return (intent.Purpose == PurposeLogin || intent.Purpose == PurposeReauthenticate) &&
		len(intent.CorrelationID) <= 128 && !strings.ContainsAny(intent.CorrelationID, "\x00\r\n") &&
		validReturnPath(intent.ReturnPath)
}

func validReturnPath(value string) bool {
	return len(value) > 0 && len(value) <= 1024 && strings.HasPrefix(value, "/") &&
		!strings.HasPrefix(value, "//") && !strings.ContainsAny(value, "\\?#%\x00\r\n\t")
}

func validTransaction(tx LoginTransaction) bool {
	return tx.StateHash != [32]byte{} && tx.BrowserHash != [32]byte{} && tx.ProviderFingerprint != [32]byte{} &&
		tx.ConnectionID != "" && validSecret(tx.Nonce) && validSecret(tx.PKCEVerifier) && validIntent(tx.Intent) &&
		!tx.CreatedAt.IsZero() && tx.ExpiresAt.After(tx.CreatedAt) && tx.ExpiresAt.Sub(tx.CreatedAt) <= 10*time.Minute
}

func secretHash(value string) [32]byte { return sha256.Sum256([]byte(value)) }
