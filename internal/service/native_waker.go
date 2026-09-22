package service

import (
	"context"
	"errors"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sean2077/pairroom/internal/claudewake"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

const (
	nativeWakeNudge           = "PairRoom inbox has messages for you. Run: pairroom relay wait"
	nativeWakeGrace           = 2 * time.Second
	nativeWakeMinimumInterval = time.Minute
	nativeWakeHourlyLimit     = 10
	nativeWakeCommandTimeout  = 10 * time.Second
)

var errNativeWakeAudit = errors.New("native wake audit is unavailable")

// nativeWakeRelay is deliberately narrower than Engine: the Service waker can
// inspect only a bounded candidate and append only fixed audit facts. Its
// in-memory SessionID is used solely as the vendor command argument; it never
// reaches the Event Log, diagnostics, or relay body.
type nativeWakeRelay interface {
	WakeCandidate(messageID string) (relay.WakeCandidate, bool)
	ReserveWake(messageID string, target model.ActorID) error
	RecordWake(outcome, reason string, target model.ActorID) error
	WakeReservations() []relay.WakeReservation
}

type nativeWakePrepare func(relay.WakeCandidate) (claudewake.Send, error)

type nativeWakeRun func(context.Context, string, ...string) error
type nativeWakeWait func(context.Context, time.Duration) error

type nativeWakerConfig struct {
	CodexCommand string
	Mock         bool
	Claude       nativeWakePrepare
	Relay        nativeWakeRelay
	Now          func() time.Time
	Grace        time.Duration
	MinInterval  time.Duration
	HourlyLimit  int
	Timeout      time.Duration
	Run          nativeWakeRun
	Wait         nativeWakeWait
}

// nativeWaker belongs to one active native Room runtime. Its durable
// reservations are rehydrated from the Room Event Log; its pending map keeps
// one grace task per target so a burst produces one wake decision.
type nativeWaker struct {
	codexCommand string
	mock         bool
	claude       nativeWakePrepare
	mu           sync.Mutex
	relay        nativeWakeRelay
	now          func() time.Time
	grace        time.Duration
	minInterval  time.Duration
	hourlyLimit  int
	timeout      time.Duration
	run          nativeWakeRun
	wait         nativeWakeWait
	attempts     []relay.WakeReservation
	pending      map[model.ActorID]struct{}
	deferred     map[model.ActorID]nativeWakeDeferred
	lastRecorded map[model.ActorID]string
	workers      sync.WaitGroup
	closing      bool
}

func newNativeWaker(cfg nativeWakerConfig) *nativeWaker {
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Grace <= 0 {
		cfg.Grace = nativeWakeGrace
	}
	if cfg.MinInterval <= 0 {
		cfg.MinInterval = nativeWakeMinimumInterval
	}
	if cfg.HourlyLimit <= 0 {
		cfg.HourlyLimit = nativeWakeHourlyLimit
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = nativeWakeCommandTimeout
	}
	if cfg.Run == nil {
		cfg.Run = runNativeWakeCommand
	}
	if cfg.Wait == nil {
		cfg.Wait = waitNativeWake
	}
	w := &nativeWaker{
		codexCommand: cfg.CodexCommand,
		mock:         cfg.Mock,
		claude:       cfg.Claude,
		relay:        cfg.Relay,
		now:          cfg.Now,
		grace:        cfg.Grace,
		minInterval:  cfg.MinInterval,
		hourlyLimit:  cfg.HourlyLimit,
		timeout:      cfg.Timeout,
		run:          cfg.Run,
		wait:         cfg.Wait,
		pending:      map[model.ActorID]struct{}{},
		deferred:     map[model.ActorID]nativeWakeDeferred{},
		lastRecorded: map[model.ActorID]string{},
	}
	if cfg.Relay != nil {
		w.attempts = append(w.attempts, cfg.Relay.WakeReservations()...)
	}
	return w
}

// Schedule is intentionally fire-and-forget at the Service HTTP boundary.
// The native FIFO is already durable before it is called; a failed wake leaves
// that message queued for foreground collection rather than affecting send.
// Schedule owns every worker until Close; cancellation must finish before the
// Room writer closes. The runtime's existing maintenance tick rechecks heads.
func (w *nativeWaker) Schedule(ctx context.Context, messageID string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return
	}
	w.workers.Add(1)
	w.mu.Unlock()
	go func() { defer w.workers.Done(); _ = w.Wake(ctx, messageID) }()
}

func (w *nativeWaker) Close() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.closing = true
	w.mu.Unlock()
	w.workers.Wait()
}

type nativeWakeDeferred struct {
	MessageID string
	At        time.Time
	Reason    string
}

type nativeWakeHeadSource interface{ WakeHeads() []relay.WakeCandidate }

// Reconcile is transport maintenance, not a model poll. It examines at most two
// heads, including after restart, queue-head changes and a collector's exit.
// A reserved head is NEVER retried. A rate-suppressed (unattempted) head becomes
// eligible when its deadline passes even if no further messages arrive.
func (w *nativeWaker) Reconcile(ctx context.Context) {
	if w == nil || ctx.Err() != nil {
		return
	}
	source, ok := w.relay.(nativeWakeHeadSource)
	if !ok {
		return
	}
	heads := source.WakeHeads()
	now := w.now()
	live := map[model.ActorID]bool{}
	for _, c := range heads {
		live[c.Target] = true
		w.mu.Lock()
		next := w.deferred[c.Target]
		if next.MessageID != c.MessageID || c.Reserved || !c.Enabled {
			delete(w.deferred, c.Target)
			next = nativeWakeDeferred{}
		}
		_, pending := w.pending[c.Target]
		closed := w.closing
		w.mu.Unlock()
		if closed || pending || c.Reserved || nativeWakeSuppression(c) != "" || (!next.At.IsZero() && now.Before(next.At)) {
			continue
		}
		w.Schedule(ctx, c.MessageID)
	}
	w.mu.Lock()
	for slot := range w.deferred {
		if !live[slot] {
			delete(w.deferred, slot)
		}
	}
	w.mu.Unlock()
}

// A deferred rate-limited head holds the Native runtime lease until it can be
// reconsidered (the Room hourly budget may exceed the ordinary idle timeout).
// Unsupported/unbound/capability-missing sessions do not pin idle runtimes.
func (w *nativeWaker) InUse() bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		return true
	}
	for _, d := range w.deferred {
		if d.Reason == "minimum_interval" || d.Reason == "hourly_limit" {
			return true
		}
	}
	return false
}

func (w *nativeWaker) deferCandidate(c relay.WakeCandidate, at time.Time, reason string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deferred[c.Target] = nativeWakeDeferred{MessageID: c.MessageID, At: at, Reason: reason}
}

// Wake evaluates one post-enqueue message. A durable reservation is appended
// before invoking the vendor wake, so neither a crash nor a duplicate HTTP retry
// can automatically repeat a possibly accepted vendor effect.
func (w *nativeWaker) Wake(ctx context.Context, messageID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if w == nil || w.relay == nil || strings.TrimSpace(messageID) == "" {
		return errNativeWakeAudit
	}
	candidate, ok := w.relay.WakeCandidate(messageID)
	if !ok {
		return nil
	}
	if !candidate.QueueStart {
		// The oldest queued message for this target owns the burst decision.
		// Do not add one suppressed audit fact per later message.
		return nil
	}
	if reason := nativeWakeSuppression(candidate); reason != "" {
		return w.record("suppressed", reason, candidate.Target)
	}
	if !w.begin(candidate.Target) {
		return nil
	}
	defer w.end(candidate.Target)
	target := candidate.Target
	if err := w.wait(ctx, w.grace); err != nil {
		// Runtime shutdown/cancellation must not manufacture a vendor attempt.
		return nil
	}
	candidate, ok = w.relay.WakeCandidate(messageID)
	if !ok {
		return w.record("suppressed", "collected", target)
	}
	if !candidate.QueueStart {
		return w.record("suppressed", "burst", candidate.Target)
	}
	if reason := nativeWakeSuppression(candidate); reason != "" {
		return w.record("suppressed", reason, candidate.Target)
	}
	var claudeSend claudewake.Send
	if candidate.Runtime.Canonical() == model.RuntimeClaude {
		if w.claude == nil {
			w.deferCandidate(candidate, w.now().Add(30*time.Second), "capability_unavailable")
			return w.record("suppressed", "capability_unavailable", candidate.Target)
		}
		var err error
		claudeSend, err = w.claude(candidate)
		if err != nil || claudeSend == nil {
			w.deferCandidate(candidate, w.now().Add(30*time.Second), "capability_unavailable")
			return w.record("suppressed", "capability_unavailable", candidate.Target)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	now := w.now().UTC()
	reservation := relay.WakeReservation{MessageID: candidate.MessageID, Target: candidate.Target, At: now}
	if reason := w.reserveRate(reservation); reason != "" {
		_, at := w.RateWindow(candidate.Target)
		w.deferCandidate(candidate, at, reason)
		return w.record("suppressed", reason, candidate.Target)
	}
	if err := w.relay.ReserveWake(candidate.MessageID, candidate.Target); err != nil {
		w.removeAttempt(candidate.MessageID)
		if errors.Is(err, relay.ErrWakeReserved) {
			return w.record("suppressed", "duplicate", candidate.Target)
		}
		if errors.Is(err, relay.ErrWakeIneligible) {
			return w.record("suppressed", "invalid_message", candidate.Target)
		}
		return errNativeWakeAudit
	}

	commandCtx, cancel := context.WithTimeout(ctx, w.timeout)
	if claudeSend != nil {
		err := claudeSend(commandCtx, nativeWakeNudge)
		cancel()
		if err == nil {
			return w.record("submitted", "", candidate.Target)
		}
		reason := "socket_failed"
		if errors.Is(err, context.DeadlineExceeded) {
			reason = "socket_timeout"
		} else if errors.Is(err, context.Canceled) {
			reason = "socket_cancelled"
		}
		return w.record("failed", reason, candidate.Target)
	}
	err := w.run(commandCtx, "codex", "queue", "--thread", candidate.SessionID, "--message", nativeWakeNudge)
	commandContextErr := commandCtx.Err()
	cancel()
	if err == nil {
		return w.record("accepted", "", candidate.Target)
	}
	return w.record("failed", classifyNativeWakeFailure(err, commandContextErr), candidate.Target)
}

func nativeWakeSuppression(candidate relay.WakeCandidate) string {
	switch {
	case !candidate.Enabled:
		return "disabled"
	case candidate.WaiterActive || candidate.Delivering:
		return "waiter_active"
	case strings.TrimSpace(candidate.SessionID) == "":
		return "unbound"
	case candidate.Runtime.Canonical() != model.RuntimeCodex && candidate.Runtime.Canonical() != model.RuntimeClaude:
		return "unsupported_runtime"
	default:
		return ""
	}
}

func (w *nativeWaker) begin(target model.ActorID) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, exists := w.pending[target]; exists {
		return false
	}
	if w.closing {
		return false
	}
	w.pending[target] = struct{}{}
	return true
}

func (w *nativeWaker) end(target model.ActorID) {
	w.mu.Lock()
	delete(w.pending, target)
	w.mu.Unlock()
}

// RateWindow reports an advisory deadline, never permission to call a vendor.
func (w *nativeWaker) RateWindow(target model.ActorID) (string, time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.rateWindowLocked(w.now().UTC(), target)
}

func (w *nativeWaker) rateWindowLocked(now time.Time, target model.ActorID) (string, time.Time) {
	cutoff := now.Add(-time.Hour)
	kept := w.attempts[:0]
	var last time.Time
	times := make([]time.Time, 0, len(w.attempts))
	for _, a := range w.attempts {
		if !a.At.After(cutoff) {
			continue
		}
		kept = append(kept, a)
		times = append(times, a.At)
		if a.Target == target && (last.IsZero() || a.At.After(last)) {
			last = a.At
		}
	}
	w.attempts = kept
	reason := ""
	var due time.Time
	if !last.IsZero() && now.Before(last.Add(w.minInterval)) {
		reason = "minimum_interval"
		due = last.Add(w.minInterval)
	}
	if len(times) >= w.hourlyLimit {
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		hourly := times[len(times)-w.hourlyLimit].Add(time.Hour)
		if due.IsZero() || hourly.After(due) {
			reason = "hourly_limit"
			due = hourly
		}
	}
	return reason, due
}

func (w *nativeWaker) reserveRate(reservation relay.WakeReservation) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if reason, _ := w.rateWindowLocked(reservation.At, reservation.Target); reason != "" {
		return reason
	}
	w.attempts = append(w.attempts, reservation)
	delete(w.deferred, reservation.Target)
	return ""
}

func (w *nativeWaker) removeAttempt(messageID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for index := len(w.attempts) - 1; index >= 0; index-- {
		if w.attempts[index].MessageID == messageID {
			w.attempts = append(w.attempts[:index], w.attempts[index+1:]...)
			return
		}
	}
}

func (w *nativeWaker) record(outcome, reason string, target model.ActorID) error {
	// Maintenance must not grow the audit once a second while a known condition
	// persists. Changes of reason/outcome are still recorded; never suppress an
	// accepted/submitted/failed effect record.
	w.mu.Lock()
	defer w.mu.Unlock()
	key := outcome + "/" + reason
	if outcome == "suppressed" && w.lastRecorded[target] == key {
		return nil
	}
	if w.relay.RecordWake(outcome, reason, target) != nil {
		return errNativeWakeAudit
	}
	w.lastRecorded[target] = key
	return nil
}

func classifyNativeWakeFailure(err, commandContextErr error) string {
	switch {
	case errors.Is(commandContextErr, context.DeadlineExceeded):
		return "command_timeout"
	case errors.Is(commandContextErr, context.Canceled):
		return "command_cancelled"
	case errors.Is(err, exec.ErrNotFound):
		return "command_unavailable"
	default:
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			return "command_unavailable"
		}
		return "command_failed"
	}
}

func waitNativeWake(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func runNativeWakeCommand(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}

// fixedNativeWakeCommand pins the wake runner to the operator-configured Codex
// executable so a Service whose PATH does not include codex (daemon/Desktop
// launch environments snapshot a different PATH than the user's shell) can
// still wake through the configured command template.
func fixedNativeWakeCommand(executable string) nativeWakeRun {
	return func(ctx context.Context, _ string, args ...string) error {
		return exec.CommandContext(ctx, executable, args...).Run()
	}
}

// unavailableNativeWakeCommand keeps Mock rooms fail-closed: the wake audit
// records command_unavailable instead of spawning a real vendor CLI.
func unavailableNativeWakeCommand() nativeWakeRun {
	return func(context.Context, string, ...string) error { return exec.ErrNotFound }
}

// Only the canonical bound workspace can supply a capability. No socket path,
// token, vendor registry lookup, or arbitrary wake command comes from a message.
func prepareNativeClaudeWake(workspace, room string) nativeWakePrepare {
	return func(candidate relay.WakeCandidate) (claudewake.Send, error) {
		dir, err := claudewake.SlotDir(workspace, room, string(candidate.Target))
		if err != nil {
			return nil, err
		}
		return claudewake.Prepare(dir, claudewake.Identity{BindID: candidate.BindID, Generation: candidate.Generation, SessionID: candidate.SessionID})
	}
}
