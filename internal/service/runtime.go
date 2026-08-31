package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrRuntimeManagerClosed  = errors.New("room runtime manager is closed")
	ErrRuntimeBusy           = errors.New("room runtime has active work")
	ErrRuntimeDrainAborted   = errors.New("room runtime drain ended before shutdown")
	ErrRuntimeCloseUncertain = errors.New("room runtime close state is uncertain")
	ErrRuntimeRoomDeleting   = errors.New("room is being permanently removed")
)

type RuntimePhase string

const (
	RuntimeSuspended RuntimePhase = "suspended"
	RuntimeQueued    RuntimePhase = "queued"
	RuntimeStarting  RuntimePhase = "starting"
	RuntimeActive    RuntimePhase = "active"
	RuntimeStopping  RuntimePhase = "stopping"
	RuntimeFailed    RuntimePhase = "failed"
)

type RoomRuntime interface {
	URL() string
	Busy() bool
	Close(context.Context) error
}

// RuntimeActivity is an optional capability. Runtimes that expose it let the
// manager treat actual Room HTTP activity as use, rather than only counting
// activation/open requests made through the Management Shell.
type RuntimeActivity interface {
	LastActivity() time.Time
}

// RuntimeDrainControl is an optional admission-control capability. The manager
// uses it while archiving or shutting down so no new Room mutation can race the
// idle boundary. Implementations must still admit approval, cancel, and
// explicit interrupt controls that can settle work already in progress.
type RuntimeDrainControl interface {
	SetDraining(bool)
}

// RuntimeInterruptControl is an optional capability. The manager uses it when
// an explicit operator action (Room archive) must stop active work instead of
// waiting for the Turn to end naturally. Capacity eviction, idle suspend,
// safe suspend, binding completion, rename, and service shutdown never use it.
type RuntimeInterruptControl interface {
	InterruptActive(ctx context.Context) error
}

// RuntimeFactory returns a running Room runtime. On failure it normally returns
// nil. A non-nil runtime together with an error means cleanup could not be
// proven complete; the manager retains that runtime and its capacity slot.
type RuntimeFactory func(context.Context, Room) (RoomRuntime, error)

type RuntimeManagerConfig struct {
	Limit        int
	IdleTimeout  time.Duration
	PollInterval time.Duration
	CloseTimeout time.Duration
	Now          func() time.Time
}

type RuntimePolicy struct {
	Limit                    int   `json:"limit"`
	IdleTimeoutSeconds       int64 `json:"idle_timeout_seconds"`
	PollIntervalMilliseconds int64 `json:"poll_interval_milliseconds"`
	CloseTimeoutSeconds      int64 `json:"close_timeout_seconds"`
}

type RuntimeStatus struct {
	RoomID           string       `json:"room_id"`
	Phase            RuntimePhase `json:"phase"`
	QueuePosition    int          `json:"queue_position,omitempty"`
	Busy             bool         `json:"busy"`
	OccupiesCapacity bool         `json:"occupies_capacity"`
	URL              string       `json:"url,omitempty"`
	LastUsedAt       time.Time    `json:"last_used_at,omitempty"`
	QueuedAt         time.Time    `json:"queued_at,omitempty"`
	LastError        string       `json:"last_error,omitempty"`
}

type runtimeEntry struct {
	phase          RuntimePhase
	runtime        RoomRuntime
	lastUsed       time.Time
	queuedAt       time.Time
	lastError      string
	requested      bool
	drainRequested bool
	generation     uint64
}

type RuntimeManager struct {
	mu sync.Mutex

	registry    *Registry
	factory     RuntimeFactory
	cfg         RuntimeManagerConfig
	entries     map[string]*runtimeEntry
	deleting    map[string]struct{}
	queue       []string
	changed     chan struct{}
	closed      bool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	closeErrors []error
}

func NewRuntimeManager(registry *Registry, factory RuntimeFactory, cfg RuntimeManagerConfig) (*RuntimeManager, error) {
	if registry == nil {
		return nil, errors.New("service registry is required")
	}
	if factory == nil {
		return nil, errors.New("runtime factory is required")
	}
	if cfg.Limit < 1 {
		cfg.Limit = 2
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 15 * time.Minute
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 500 * time.Millisecond
	}
	if cfg.CloseTimeout <= 0 {
		cfg.CloseTimeout = 10 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &RuntimeManager{
		registry: registry, factory: factory, cfg: cfg,
		entries: make(map[string]*runtimeEntry), deleting: make(map[string]struct{}), changed: make(chan struct{}),
		ctx: ctx, cancel: cancel,
	}
	manager.wg.Add(1)
	go manager.loop()
	return manager, nil
}

func (m *RuntimeManager) RequestActivation(roomID string) (RuntimeStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return RuntimeStatus{}, ErrRuntimeManagerClosed
	}
	if _, deleting := m.deleting[roomID]; deleting {
		return RuntimeStatus{}, ErrRuntimeRoomDeleting
	}
	// Keep the manager lock through the Registry read. dispatchLocked follows
	// the same lock order, and permanent deletion uses this boundary to ensure
	// an activation can be either cancelled before removal or rejected after it.
	if err := m.registry.Healthy(); err != nil {
		return RuntimeStatus{}, err
	}
	room, ok := m.registry.Room(roomID)
	if !ok {
		return RuntimeStatus{}, ErrRoomNotFound
	}
	if room.Archived() {
		return RuntimeStatus{}, errors.New("archived room must be restored before activation")
	}
	if room.HasBlockingPendingBindings() {
		return RuntimeStatus{}, fmt.Errorf("%w: complete the legacy Room's Claude/Codex bindings before activation", ErrRoomBindingPending)
	}

	entry := m.entries[roomID]
	if entry == nil {
		entry = &runtimeEntry{phase: RuntimeSuspended}
		m.entries[roomID] = entry
	}
	switch entry.phase {
	case RuntimeSuspended:
		entry.drainRequested = false
		entry.phase = RuntimeQueued
		entry.queuedAt = m.cfg.Now()
		entry.lastError = ""
		entry.requested = true
		m.queue = appendUnique(m.queue, roomID)
	case RuntimeFailed:
		if entry.runtime != nil {
			return RuntimeStatus{}, fmt.Errorf("%w for Room %s: %s", ErrRuntimeCloseUncertain, roomID, entry.lastError)
		}
		entry.drainRequested = false
		entry.phase = RuntimeQueued
		entry.queuedAt = m.cfg.Now()
		entry.lastError = ""
		entry.requested = true
		m.queue = appendUnique(m.queue, roomID)
	case RuntimeStopping:
		// The stop cannot be cancelled safely once draining has begun. Remember
		// the demand and reactivate the Room after finishStop.
		entry.requested = true
	case RuntimeActive:
		entry.lastUsed = m.cfg.Now()
	}
	m.dispatchLocked()
	m.signalLocked()
	return m.statusLocked(roomID, entry), nil
}

func (m *RuntimeManager) Activate(ctx context.Context, roomID string) (RoomRuntime, RuntimeStatus, error) {
	if _, err := m.RequestActivation(roomID); err != nil {
		return nil, RuntimeStatus{}, err
	}
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, RuntimeStatus{}, ErrRuntimeManagerClosed
		}
		entry := m.entries[roomID]
		if entry == nil {
			m.mu.Unlock()
			return nil, RuntimeStatus{}, ErrRoomNotFound
		}
		if entry.phase == RuntimeActive && entry.runtime != nil {
			entry.lastUsed = m.cfg.Now()
			runtime := entry.runtime
			status := m.statusLocked(roomID, entry)
			m.mu.Unlock()
			return runtime, status, nil
		}
		if entry.phase == RuntimeFailed {
			status := m.statusLocked(roomID, entry)
			err := errors.New(entry.lastError)
			m.mu.Unlock()
			return nil, status, err
		}
		changed := m.changed
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			// Activation is a durable service demand, not ownership of the HTTP
			// request. A disconnected browser does not silently remove the Room
			// from the visible FIFO queue.
			return nil, m.Status(roomID), ctx.Err()
		case <-changed:
		}
	}
}

// Policy returns the effective runtime policy for observability. The values
// are immutable after manager construction; changing service flags still
// requires a controlled service restart so active Room turns are never
// reconfigured underneath a running vendor session.
func (m *RuntimeManager) Policy() RuntimePolicy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return RuntimePolicy{
		Limit:                    m.cfg.Limit,
		IdleTimeoutSeconds:       int64(m.cfg.IdleTimeout / time.Second),
		PollIntervalMilliseconds: int64(m.cfg.PollInterval / time.Millisecond),
		CloseTimeoutSeconds:      int64(m.cfg.CloseTimeout / time.Second),
	}
}

func (m *RuntimeManager) Touch(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry := m.entries[roomID]; entry != nil && entry.phase == RuntimeActive {
		entry.lastUsed = m.cfg.Now()
		m.signalLocked()
	}
}

func (m *RuntimeManager) Status(roomID string) RuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[roomID]
	if entry == nil {
		return RuntimeStatus{RoomID: roomID, Phase: RuntimeSuspended}
	}
	m.refreshUsageLocked(entry)
	return m.statusLocked(roomID, entry)
}

func (m *RuntimeManager) Statuses() []RuntimeStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	statuses := make([]RuntimeStatus, 0, len(ids))
	for _, id := range ids {
		m.refreshUsageLocked(m.entries[id])
		statuses = append(statuses, m.statusLocked(id, m.entries[id]))
	}
	return statuses
}

// Suspend closes an idle runtime. It never interrupts active work.
func (m *RuntimeManager) Suspend(ctx context.Context, roomID string) error {
	m.mu.Lock()
	entry := m.entries[roomID]
	if entry != nil {
		entry.requested = false
	}
	if entry == nil || entry.phase == RuntimeSuspended {
		m.mu.Unlock()
		return nil
	}
	if entry.phase == RuntimeQueued {
		m.removeQueuedLocked(roomID)
		entry.phase = RuntimeSuspended
		m.signalLocked()
		m.mu.Unlock()
		return nil
	}
	if entry.phase == RuntimeFailed {
		if entry.runtime != nil {
			err := fmt.Errorf("%w for Room %s: %s", ErrRuntimeCloseUncertain, roomID, entry.lastError)
			m.mu.Unlock()
			return err
		}
		entry.phase = RuntimeSuspended
		m.signalLocked()
		m.mu.Unlock()
		return nil
	}
	if entry.phase != RuntimeActive || entry.runtime == nil {
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
			return m.Suspend(ctx, roomID)
		}
	}
	if entry.runtime.Busy() {
		m.mu.Unlock()
		return ErrRuntimeBusy
	}
	runtime := entry.runtime
	entry.phase = RuntimeStopping
	entry.generation++
	generation := entry.generation
	m.signalLocked()
	m.mu.Unlock()

	err := runtime.Close(ctx)
	m.finishStop(roomID, generation, err)
	return err
}

// WaitAndSuspend is used by control-plane mutations (rename, binding
// completion). It closes the Room mutation gate first, waits for active turns
// to settle, and then closes the runtime; it never calls Interrupt. A
// canceled operation reopens the gate unless service shutdown has taken
// ownership of the drain.
func (m *RuntimeManager) WaitAndSuspend(ctx context.Context, roomID string) error {
	return m.drainAndSuspend(ctx, roomID, false)
}

// InterruptAndSuspend is used by Room archive. It closes the Room mutation
// gate, asks the runtime to interrupt active work so the operator does not
// have to stop the Agent from inside the Room first, waits for the runtime to
// settle, and then closes it. Runtimes without interrupt support, or work
// that survives an interrupt, still settle through the same wait boundary as
// WaitAndSuspend. A canceled operation reopens the gate unless service
// shutdown has taken ownership of the drain.
func (m *RuntimeManager) InterruptAndSuspend(ctx context.Context, roomID string) error {
	return m.drainAndSuspend(ctx, roomID, true)
}

// drainAndSuspend closes the Room mutation gate, polls Suspend until the
// runtime settles, and then closes it. When interruptActive is set it asks the
// runtime to stop in-flight work on each attempt instead of waiting for the
// Turn to end naturally; capturing the wait signal before the interrupt lets
// the loop re-check Suspend as soon as the runtime reports one.
func (m *RuntimeManager) drainAndSuspend(ctx context.Context, roomID string, interruptActive bool) error {
	m.setDrainRequested(roomID, true)
	defer m.releaseDrainRequested(roomID)

	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	for {
		err := m.Suspend(ctx, roomID)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrRuntimeBusy) && !errors.Is(err, ErrRuntimeDrainAborted) {
			return err
		}
		changed := m.changedChannel()
		if interruptActive {
			m.interruptActiveWork(ctx, roomID)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-changed:
		}
	}
}

// interruptActiveWork asks the active runtime to stop in-flight work. It is
// best-effort: a failed or unsupported interrupt leaves the caller inside the
// ordinary wait boundary, where work can still end naturally. Signalling the
// wait loop lets it re-check Suspend as soon as the runtime reports an
// interrupt, rather than waiting for the next poll tick.
func (m *RuntimeManager) interruptActiveWork(ctx context.Context, roomID string) {
	if ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	entry := m.entries[roomID]
	var runtime RoomRuntime
	if entry != nil && entry.phase == RuntimeActive {
		runtime = entry.runtime
	}
	m.mu.Unlock()
	control, ok := runtime.(RuntimeInterruptControl)
	if !ok {
		return
	}
	_ = control.InterruptActive(ctx)
	m.mu.Lock()
	m.signalLocked()
	m.mu.Unlock()
}

// PrepareRoomDeletion creates a Runtime admission barrier, cancels queued
// activation, and closes an idle Runtime. It never interrupts active work: a
// busy Runtime returns ErrRuntimeBusy and the barrier is rolled back.
func (m *RuntimeManager) PrepareRoomDeletion(ctx context.Context, roomID string) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrRuntimeManagerClosed
	}
	if _, exists := m.deleting[roomID]; exists {
		m.mu.Unlock()
		return ErrRuntimeRoomDeleting
	}
	m.deleting[roomID] = struct{}{}
	if entry := m.entries[roomID]; entry != nil {
		entry.requested = false
		entry.drainRequested = true
		if control, ok := entry.runtime.(RuntimeDrainControl); ok {
			control.SetDraining(true)
		}
	}
	m.signalLocked()
	m.mu.Unlock()

	if err := m.Suspend(ctx, roomID); err != nil {
		m.AbortRoomDeletion(roomID)
		return err
	}
	m.mu.Lock()
	if _, exists := m.deleting[roomID]; !exists {
		m.mu.Unlock()
		return ErrRuntimeRoomDeleting
	}
	entry := m.entries[roomID]
	if entry != nil && (entry.phase != RuntimeSuspended || entry.runtime != nil) {
		// Defensive consistency failure: never leave a permanent admission gate
		// behind when the Runtime cannot be proven removable.
		delete(m.deleting, roomID)
		entry.drainRequested = false
		if control, ok := entry.runtime.(RuntimeDrainControl); ok {
			control.SetDraining(false)
		}
		m.dispatchLocked()
		m.signalLocked()
		m.mu.Unlock()
		return fmt.Errorf("%w: Room %s runtime did not reach a removable state", ErrRuntimeCloseUncertain, roomID)
	}
	m.mu.Unlock()
	return nil
}

// AbortRoomDeletion reopens Runtime admission after Registry removal fails.
func (m *RuntimeManager) AbortRoomDeletion(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.deleting[roomID]; !exists {
		return
	}
	delete(m.deleting, roomID)
	if entry := m.entries[roomID]; entry != nil {
		entry.drainRequested = false
		if control, ok := entry.runtime.(RuntimeDrainControl); ok {
			control.SetDraining(false)
		}
	}
	m.dispatchLocked()
	m.signalLocked()
}

// CommitRoomDeletion discards the now-suspended Runtime bookkeeping only after
// the Registry has durably forgotten the Room.
func (m *RuntimeManager) CommitRoomDeletion(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeQueuedLocked(roomID)
	delete(m.entries, roomID)
	delete(m.deleting, roomID)
	m.dispatchLocked()
	m.signalLocked()
}

func (m *RuntimeManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.cancel() // cancel only in-progress factories; active runtimes own independent contexts
		m.queue = nil
		for _, entry := range m.entries {
			entry.requested = false
			entry.drainRequested = true
			if control, ok := entry.runtime.(RuntimeDrainControl); ok {
				control.SetDraining(true)
			}
			if entry.phase == RuntimeQueued {
				entry.phase = RuntimeSuspended
			}
		}
		m.signalLocked()
	}
	m.mu.Unlock()

	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	var result error
	for {
		m.mu.Lock()
		pending := false
		for roomID, entry := range m.entries {
			switch entry.phase {
			case RuntimeStarting, RuntimeStopping:
				pending = true
			case RuntimeActive:
				if entry.runtime == nil {
					entry.phase = RuntimeSuspended
					continue
				}
				if entry.runtime.Busy() {
					pending = true
					continue
				}
				runtime := entry.runtime
				entry.phase = RuntimeStopping
				entry.generation++
				generation := entry.generation
				pending = true
				m.wg.Add(1)
				go func(id string, rt RoomRuntime, gen uint64) {
					defer m.wg.Done()
					closeCtx, cancel := context.WithTimeout(context.Background(), m.cfg.CloseTimeout)
					err := rt.Close(closeCtx)
					cancel()
					m.finishStop(id, gen, err)
				}(roomID, runtime, generation)
			}
		}
		if !pending {
			m.mu.Unlock()
			m.wg.Wait()
			m.mu.Lock()
			for _, closeErr := range m.closeErrors {
				result = errors.Join(result, closeErr)
			}
			m.mu.Unlock()
			return result
		}
		changed := m.changed
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return errors.Join(result, ctx.Err())
		case <-ticker.C:
		case <-changed:
		}
	}
}

func (m *RuntimeManager) loop() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.reconcile()
		}
	}
}

func (m *RuntimeManager) reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		m.signalLocked()
		return
	}
	now := m.cfg.Now()
	for roomID, entry := range m.entries {
		m.refreshUsageLocked(entry)
		if entry.phase != RuntimeActive || entry.runtime == nil || entry.runtime.Busy() {
			continue
		}
		if now.Sub(entry.lastUsed) < m.cfg.IdleTimeout {
			continue
		}
		runtime := entry.runtime
		entry.phase = RuntimeStopping
		entry.requested = false
		entry.generation++
		generation := entry.generation
		m.wg.Add(1)
		go func(id string, rt RoomRuntime, gen uint64) {
			defer m.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), m.cfg.CloseTimeout)
			err := rt.Close(ctx)
			cancel()
			m.finishStop(id, gen, err)
		}(roomID, runtime, generation)
	}
	m.dispatchLocked()
	m.signalLocked()
}

func (m *RuntimeManager) dispatchLocked() {
	if m.closed {
		return
	}
	for len(m.queue) > 0 {
		roomID := m.queue[0]
		entry := m.entries[roomID]
		if entry == nil || entry.phase != RuntimeQueued {
			m.queue = m.queue[1:]
			continue
		}
		if _, deleting := m.deleting[roomID]; deleting {
			m.queue = m.queue[1:]
			entry.phase = RuntimeSuspended
			entry.requested = false
			continue
		}
		if m.capacityLocked() < m.cfg.Limit {
			m.queue = m.queue[1:]
			room, ok := m.registry.Room(roomID)
			if !ok || room.Archived() {
				entry.phase = RuntimeFailed
				entry.lastError = "room no longer exists or is archived"
				m.signalLocked()
				continue
			}
			entry.phase = RuntimeStarting
			entry.requested = false
			entry.generation++
			generation := entry.generation
			m.wg.Add(1)
			go func(id string, value Room, gen uint64) {
				defer m.wg.Done()
				runtime, err := m.factory(m.ctx, value)
				m.finishStart(id, gen, runtime, err)
			}(roomID, room, generation)
			continue
		}

		victimID, victim := m.idleLRULocked()
		if victim == nil {
			return // all capacity is starting, stopping, or busy; FIFO remains visible
		}
		runtime := victim.runtime
		victim.phase = RuntimeStopping
		victim.requested = false
		victim.generation++
		generation := victim.generation
		m.wg.Add(1)
		go func(id string, rt RoomRuntime, gen uint64) {
			defer m.wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), m.cfg.CloseTimeout)
			err := rt.Close(ctx)
			cancel()
			m.finishStop(id, gen, err)
		}(victimID, runtime, generation)
		return
	}
}

func (m *RuntimeManager) finishStart(roomID string, generation uint64, runtime RoomRuntime, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[roomID]
	if entry == nil || entry.generation != generation || entry.phase != RuntimeStarting {
		if runtime != nil {
			go runtime.Close(context.Background())
		}
		return
	}
	entry.requested = false
	if err != nil || runtime == nil {
		entry.phase = RuntimeFailed
		if err == nil {
			err = errors.New("runtime factory returned nil runtime")
		}
		entry.lastError = err.Error()
		entry.runtime = runtime
		if runtime != nil {
			m.closeErrors = append(m.closeErrors, fmt.Errorf("start Room %s left runtime cleanup uncertain: %w", roomID, err))
		}
	} else {
		entry.phase = RuntimeActive
		entry.runtime = runtime
		entry.lastUsed = m.cfg.Now()
		entry.lastError = ""
		if entry.drainRequested {
			if control, ok := runtime.(RuntimeDrainControl); ok {
				control.SetDraining(true)
			}
		}
	}
	m.dispatchLocked()
	m.signalLocked()
}

func (m *RuntimeManager) finishStop(roomID string, generation uint64, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[roomID]
	if entry == nil || entry.generation != generation || entry.phase != RuntimeStopping {
		return
	}
	if err != nil {
		if errors.Is(err, ErrRuntimeDrainAborted) {
			// The runtime guarantees that it did not cross its irreversible close
			// boundary. Restore it as active and let the caller retry after the
			// racing mutation settles; this is not cleanup uncertainty.
			entry.phase = RuntimeActive
			entry.lastUsed = m.cfg.Now()
			entry.lastError = ""
			m.dispatchLocked()
			m.signalLocked()
			return
		}
		// Any other failed Close does not prove that vendor processes or their
		// bindings have stopped. Retain the runtime and its capacity slot so a
		// second runtime can never be started for the same durable bindings.
		entry.phase = RuntimeFailed
		entry.requested = false
		entry.lastError = err.Error()
		m.closeErrors = append(m.closeErrors, fmt.Errorf("close Room %s runtime: %w", roomID, err))
		m.dispatchLocked()
		m.signalLocked()
		return
	}
	entry.runtime = nil
	entry.drainRequested = false
	entry.lastError = ""
	if entry.requested && !m.closed {
		entry.phase = RuntimeQueued
		entry.queuedAt = m.cfg.Now()
		entry.requested = true
		m.queue = appendUnique(m.queue, roomID)
	} else {
		entry.phase = RuntimeSuspended
		entry.requested = false
	}
	m.dispatchLocked()
	m.signalLocked()
}

func (m *RuntimeManager) capacityLocked() int {
	count := 0
	for _, entry := range m.entries {
		switch entry.phase {
		case RuntimeStarting, RuntimeActive, RuntimeStopping:
			count++
		case RuntimeFailed:
			if entry.runtime != nil {
				count++
			}
		}
	}
	return count
}

func (m *RuntimeManager) idleLRULocked() (string, *runtimeEntry) {
	var id string
	var selected *runtimeEntry
	for roomID, entry := range m.entries {
		m.refreshUsageLocked(entry)
		if entry.phase != RuntimeActive || entry.runtime == nil || entry.runtime.Busy() {
			continue
		}
		if selected == nil || entry.lastUsed.Before(selected.lastUsed) || (entry.lastUsed.Equal(selected.lastUsed) && roomID < id) {
			id, selected = roomID, entry
		}
	}
	return id, selected
}

func (m *RuntimeManager) refreshUsageLocked(entry *runtimeEntry) {
	if entry == nil || entry.runtime == nil {
		return
	}
	// A running turn is itself recent use.  LastActivity is primarily driven by
	// HTTP/UI traffic, so without this refresh a long background turn could
	// finish with an old timestamp and be suspended immediately instead of
	// receiving a full idle-timeout window.
	if entry.runtime.Busy() {
		if now := m.cfg.Now(); now.After(entry.lastUsed) {
			entry.lastUsed = now
		}
	}
	activity, ok := entry.runtime.(RuntimeActivity)
	if !ok {
		return
	}
	if used := activity.LastActivity(); used.After(entry.lastUsed) {
		entry.lastUsed = used
	}
}

func (m *RuntimeManager) statusLocked(roomID string, entry *runtimeEntry) RuntimeStatus {
	status := RuntimeStatus{
		RoomID: roomID, Phase: entry.phase, LastUsedAt: entry.lastUsed,
		QueuedAt: entry.queuedAt, LastError: entry.lastError,
		OccupiesCapacity: entry.phase == RuntimeStarting || entry.phase == RuntimeActive ||
			entry.phase == RuntimeStopping || (entry.phase == RuntimeFailed && entry.runtime != nil),
	}
	if entry.runtime != nil && entry.phase != RuntimeFailed {
		status.Busy = entry.runtime.Busy()
		status.URL = entry.runtime.URL()
	}
	if entry.phase == RuntimeQueued {
		for index, id := range m.queue {
			if id == roomID {
				status.QueuePosition = index + 1
				break
			}
		}
	}
	return status
}

func (m *RuntimeManager) setDrainRequested(roomID string, value bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[roomID]
	if entry == nil {
		return
	}
	entry.drainRequested = value
	if control, ok := entry.runtime.(RuntimeDrainControl); ok {
		control.SetDraining(value)
	}
	m.signalLocked()
}

func (m *RuntimeManager) releaseDrainRequested(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	entry := m.entries[roomID]
	if entry == nil {
		return
	}
	entry.drainRequested = false
	if control, ok := entry.runtime.(RuntimeDrainControl); ok {
		control.SetDraining(false)
	}
	m.signalLocked()
}

func (m *RuntimeManager) removeQueuedLocked(roomID string) {
	for index, id := range m.queue {
		if id == roomID {
			m.queue = append(m.queue[:index], m.queue[index+1:]...)
			return
		}
	}
}

func (m *RuntimeManager) signalLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func (m *RuntimeManager) changedChannel() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.changed
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func (s RuntimeStatus) String() string {
	if s.QueuePosition > 0 {
		return fmt.Sprintf("%s (queue %d)", s.Phase, s.QueuePosition)
	}
	return string(s.Phase)
}
