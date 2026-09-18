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

var (
	ErrServiceAlreadyRunning   = errors.New("another PairRoom Service owns this data root")
	ErrServiceLockOwnerRunning = errors.New("service lock owner is still running")
)

type serviceLockMetadata struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Nonce     string    `json:"nonce"`
}

func decodeServiceLockMetadata(data []byte) (serviceLockMetadata, error) {
	var metadata serviceLockMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return serviceLockMetadata{}, err
	}
	if metadata.PID <= 0 {
		return serviceLockMetadata{}, errors.New("service lock owner PID is missing or invalid")
	}
	if metadata.StartedAt.IsZero() {
		return serviceLockMetadata{}, errors.New("service lock start time is missing or invalid")
	}
	if strings.TrimSpace(metadata.Nonce) == "" {
		return serviceLockMetadata{}, errors.New("service lock nonce is missing")
	}
	return metadata, nil
}

// ServiceLockInfo is the safe-to-display portion of a service.lock owner.
// The nonce remains private so diagnostics cannot be used to impersonate the
// current owner or to remove a replacement lock.
type ServiceLockInfo struct {
	PID       int
	StartedAt time.Time
}

// serviceLockReuseTolerance absorbs clock granularity and the small gap
// between process start and the lock's StartedAt write when deciding whether
// a live PID is really the recorded owner or a newer process reusing the PID.
const serviceLockReuseTolerance = 5 * time.Minute

// ServiceLockOwnerRunning checks whether the process recorded in a lock is
// still present. A true result is conservative: recovery must never proceed
// while the owner may still be alive. When the platform can report the
// process creation time, a PID now held by a process created well after the
// recorded owner started is recognized as reuse: the owner is gone.
func ServiceLockOwnerRunning(info ServiceLockInfo) (bool, error) {
	alive, err := serviceLockProcessAlive(info.PID)
	if err != nil || !alive {
		return alive, err
	}
	started, ok, err := serviceLockProcessStartedAt(info.PID)
	if err != nil {
		return true, err
	}
	if !ok {
		return true, nil
	}
	// Both timestamps come from the same wall-clock domain: the kernel's
	// process creation time and this Service's own StartedAt write, which
	// happens seconds after the process starts. A genuine owner therefore
	// always satisfies creation <= StartedAt; only a large clock step landing
	// inside that brief create-to-lock window could invert the comparison, and
	// the tolerance widens the safe side further.
	if !info.StartedAt.IsZero() && started.After(info.StartedAt.Add(serviceLockReuseTolerance)) {
		return false, nil
	}
	return true, nil
}

// ServiceLock is a cooperative process lock for one service data root. The
// on-disk nonce prevents a delayed Close from deleting a replacement owner's
// lock. Crash-stale locks are recovered only after the recorded PID is
// confirmed gone; PairRoom never guesses that another process is dead.
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
		if err := RecoverServiceLock(root); err != nil {
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
				if owner, decodeErr := decodeServiceLockMetadata(existing); decodeErr == nil {
					detail = fmt.Sprintf(" (pid %d, started %s", owner.PID, owner.StartedAt.Format(time.RFC3339))
					if running, probeErr := serviceLockProcessAlive(owner.PID); probeErr == nil {
						if running {
							detail += "; process is running"
						} else {
							detail += "; process is not running"
						}
					}
					detail += ")"
				} else {
					detail = fmt.Sprintf(" (lock metadata is unreadable: %v; if no PairRoom Service is starting this is crash debris, and --recover-stale-lock removes an unreadable lock older than %s)", decodeErr, serviceLockDebrisAge)
				}
			} else {
				detail = fmt.Sprintf(" (lock metadata could not be read: %v)", readErr)
			}
			return nil, fmt.Errorf("%w%s; use --recover-stale-lock only after the recorded owner is confirmed gone", ErrServiceAlreadyRunning, detail)
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

// InspectServiceLock reports the owner metadata for a selected data root. It
// never changes the lock and returns found=false when the lock is absent.
func InspectServiceLock(input string) (info ServiceLockInfo, found bool, err error) {
	root, err := ResolveRoot(input)
	if err != nil {
		return ServiceLockInfo{}, false, err
	}
	metadata, found, err := readServiceLockMetadata(filepath.Join(root, "service.lock"))
	if err != nil {
		return ServiceLockInfo{}, found, err
	}
	if !found {
		return ServiceLockInfo{}, false, nil
	}
	return ServiceLockInfo{PID: metadata.PID, StartedAt: metadata.StartedAt}, true, nil
}

func readServiceLockMetadata(path string) (metadata serviceLockMetadata, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return serviceLockMetadata{}, false, nil
	}
	if err != nil {
		return serviceLockMetadata{}, true, fmt.Errorf("read service lock: %w", err)
	}
	metadata, err = decodeServiceLockMetadata(data)
	if err != nil {
		return serviceLockMetadata{}, true, fmt.Errorf("decode service lock: %w", err)
	}
	return metadata, true, nil
}

// serviceLockRecoveryHook runs after a stale lock has been moved aside and
// before that moved file is deleted. Tests use it to insert a replacement
// owner at the live path.
var serviceLockRecoveryHook func(movedPath, livePath string)

func sameServiceLockMetadata(left, right serviceLockMetadata) bool {
	return left.PID == right.PID && left.StartedAt.Equal(right.StartedAt) && left.Nonce == right.Nonce
}

// RecoverServiceLock removes one crash-stale lock after the recorded PID is
// confirmed gone. The owner metadata is validated and its PID is probed before
// the file is moved aside; the live path is never deleted in place, so a
// replacement owner that appears after the move is left untouched. Desktop and
// daemon start invoke this after that liveness check; a live owner still fails
// closed.
func RecoverServiceLock(input string) error {
	root, err := ResolveRoot(input)
	if err != nil {
		return err
	}
	path := filepath.Join(root, "service.lock")
	metadata, found, inspectErr := readServiceLockMetadata(path)
	if inspectErr != nil {
		if handled, debrisErr := recoverUnreadableServiceLock(path, root); handled {
			return debrisErr
		}
		return fmt.Errorf("cannot verify service lock owner before recovery: %w; if no PairRoom Service is starting, remove %s manually after confirming no owner process, then retry", inspectErr, path)
	}
	if !found {
		return nil
	}
	running, probeErr := ServiceLockOwnerRunning(ServiceLockInfo{PID: metadata.PID, StartedAt: metadata.StartedAt})
	if probeErr != nil {
		return fmt.Errorf("verify service lock owner pid %d before recovery: %w", metadata.PID, probeErr)
	}
	if running {
		return fmt.Errorf("%w (pid %d, started %s)", ErrServiceLockOwnerRunning, metadata.PID, metadata.StartedAt.Format(time.RFC3339))
	}

	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Errorf("generate service lock recovery name: %w", err)
	}
	moved := path + ".recovering-" + hex.EncodeToString(raw[:])
	if err := os.Rename(path, moved); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("move crash-stale service lock aside: %w", err)
	}
	if hook := serviceLockRecoveryHook; hook != nil {
		hook(moved, path)
	}

	movedMetadata, movedFound, movedErr := readServiceLockMetadata(moved)
	if movedErr != nil || !movedFound || !sameServiceLockMetadata(movedMetadata, metadata) {
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			if restoreErr := os.Rename(moved, path); restoreErr != nil && !errors.Is(restoreErr, os.ErrNotExist) {
				return fmt.Errorf("service lock changed during recovery and could not be restored: %w", restoreErr)
			}
		}
		if movedErr != nil {
			return fmt.Errorf("cannot verify moved service lock before recovery: %w", movedErr)
		}
		return errors.New("service lock changed during recovery; leaving the current owner in place")
	}
	if err := os.Remove(moved); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove recovered service lock: %w", err)
	}
	if err := syncDir(root); err != nil {
		return err
	}
	return nil
}

// serviceLockDebrisAge is the conservative window after which an unreadable
// service.lock is treated as crash debris from the O_EXCL-create-to-write gap
// rather than a possible in-flight acquisition.
const serviceLockDebrisAge = 2 * time.Minute

// recoverUnreadableServiceLock removes crash debris left between the O_EXCL
// create and the metadata write: a lock file that does not parse as JSON at
// all. Debris is only eligible after serviceLockDebrisAge, so a concurrent
// live acquisition is never stolen. Parseable-but-incomplete metadata is not
// debris: it may name a real PID that cannot be fully verified, so the caller
// keeps refusing it. The first return reports whether this path owned the
// outcome.
func recoverUnreadableServiceLock(path, root string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, nil
	}
	var probe serviceLockMetadata
	if json.Unmarshal(data, &probe) == nil {
		return false, nil
	}
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) < serviceLockDebrisAge {
		return false, nil
	}
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return true, fmt.Errorf("generate service lock recovery name: %w", err)
	}
	moved := path + ".recovering-" + hex.EncodeToString(raw[:])
	if err := os.Rename(path, moved); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return true, fmt.Errorf("move unreadable service lock aside: %w", err)
	}
	if hook := serviceLockRecoveryHook; hook != nil {
		hook(moved, path)
	}
	// A replacement owner that materialized during the move wins: restore or
	// keep its live lock and drop only the debris that was moved aside.
	if _, movedFound, movedErr := readServiceLockMetadata(moved); movedErr == nil && movedFound {
		if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
			if restoreErr := os.Rename(moved, path); restoreErr == nil {
				return true, errors.New("service lock gained readable owner metadata during recovery; leaving it in place")
			}
		}
	}
	if err := os.Remove(moved); err != nil && !errors.Is(err, os.ErrNotExist) {
		return true, fmt.Errorf("remove recovered unreadable service lock: %w", err)
	}
	if err := syncDir(root); err != nil {
		return true, err
	}
	return true, nil
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
