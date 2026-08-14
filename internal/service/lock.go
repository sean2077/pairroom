package service

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrServiceAlreadyRunning = errors.New("another PairRoom Service owns this data root")

type serviceLockMetadata struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Nonce     string    `json:"nonce"`
}

// ServiceLock is a cooperative process lock for one service data root. The
// on-disk nonce prevents a delayed Close from deleting a replacement owner's
// lock. Crash-stale locks are recovered only through an explicit CLI flag;
// PairRoom never guesses that another process is dead.
type ServiceLock struct {
	root  string
	path  string
	nonce string
	once  sync.Once
	err   error
}

func ResolveRoot(input string) (string, error) {
	root := strings.TrimSpace(input)
	if root == "" {
		var err error
		root, err = DefaultRoot()
		if err != nil {
			return "", err
		}
	} else if !filepath.IsAbs(root) {
		return "", errors.New("service data root must be absolute")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve service data root: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func AcquireServiceLock(input string, recoverStale bool) (*ServiceLock, error) {
	root, err := ResolveRoot(input)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create service data root: %w", err)
	}
	path := filepath.Join(root, "service.lock")
	if recoverStale {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove explicitly recovered service lock: %w", err)
		}
		if err := syncDir(root); err != nil {
			return nil, err
		}
	}

	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("generate service lock nonce: %w", err)
	}
	metadata := serviceLockMetadata{
		PID: os.Getpid(), StartedAt: time.Now().UTC(), Nonce: hex.EncodeToString(raw[:]),
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode service lock: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			detail := ""
			if existing, readErr := os.ReadFile(path); readErr == nil {
				var owner serviceLockMetadata
				if json.Unmarshal(existing, &owner) == nil && owner.PID > 0 {
					detail = fmt.Sprintf(" (pid %d, started %s)", owner.PID, owner.StartedAt.Format(time.RFC3339))
				}
			}
			return nil, fmt.Errorf("%w%s; verify that process is gone before using --recover-stale-lock", ErrServiceAlreadyRunning, detail)
		}
		return nil, fmt.Errorf("create service lock: %w", err)
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("write service lock: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync service lock: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close service lock: %w", err)
	}
	if err := syncDir(root); err != nil {
		return nil, err
	}
	cleanup = false
	return &ServiceLock{root: root, path: path, nonce: metadata.Nonce}, nil
}

func (l *ServiceLock) Root() string {
	if l == nil {
		return ""
	}
	return l.root
}

func (l *ServiceLock) Close() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		data, err := os.ReadFile(l.path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return
			}
			l.err = fmt.Errorf("read service lock during release: %w", err)
			return
		}
		var metadata serviceLockMetadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			l.err = fmt.Errorf("decode service lock during release: %w", err)
			return
		}
		if metadata.Nonce != l.nonce {
			l.err = errors.New("service lock ownership changed; refusing to remove another process's lock")
			return
		}
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			l.err = fmt.Errorf("remove service lock: %w", err)
			return
		}
		l.err = syncDir(l.root)
	})
	return l.err
}
