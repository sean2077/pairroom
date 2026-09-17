package service

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"

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

type nativeWakeRun func(context.Context, string, ...string) error
type nativeWakeWait func(context.Context, time.Duration) error

type nativeWakerConfig struct {
	Relay       nativeWakeRelay
	Now         func() time.Time
	Grace       time.Duration
	MinInterval time.Duration
	HourlyLimit int
	Timeout     time.Duration
	Run         nativeWakeRun
	Wait        nativeWakeWait
}

// nativeWaker belongs to one active native Room runtime. Its durable
// reservations are rehydrated from the Room Event Log; its pending map keeps
// one grace task per target so a burst produces one wake decision.
type nativeWaker struct {
	mu          sync.Mutex
	relay       nativeWakeRelay
	now         func() time.Time
	grace       time.Duration
	minInterval time.Duration
	hourlyLimit int
	timeout     time.Duration
	run         nativeWakeRun
	wait        nativeWakeWait
	attempts    []relay.WakeReservation
	pending     map[model.ActorID]struct{}
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
		relay:       cfg.Relay,
		now:         cfg.Now,
		grace:       cfg.Grace,
		minInterval: cfg.MinInterval,
		hourlyLimit: cfg.HourlyLimit,
		timeout:     cfg.Timeout,
		run:         cfg.Run,
		wait:        cfg.Wait,
		pending:     map[model.ActorID]struct{}{},
	}
	if cfg.Relay != nil {
		w.attempts = append(w.attempts, cfg.Relay.WakeReservations()...)
	}
	return w
}

// Schedule is intentionally fire-and-forget at the Service HTTP boundary.
// The native FIFO is already durable before it is called; a failed wake leaves
// that message queued for foreground collection rather than affecting send.
func (w *nativeWaker) Schedule(ctx context.Context, messageID string) {
	go func() { _ = w.Wake(ctx, messageID) }()
}

// Wake evaluates one post-enqueue message. A durable reservation is appended
// before invoking codex queue, so neither a crash nor a duplicate HTTP retry
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
	now := w.now().UTC()
	reservation := relay.WakeReservation{MessageID: candidate.MessageID, Target: candidate.Target, At: now}
	if reason := w.reserveRate(reservation); reason != "" {
		return w.record("suppressed", reason, candidate.Target)
	}
	if err := w.relay.ReserveWake(candidate.MessageID, candidate.Target); err != nil {
		w.removeAttempt(candidate.MessageID)
		if errors.Is(err, relay.ErrWakeReserved) {
			return w.record("suppressed", "duplicate", candidate.Target)
		}
		return errNativeWakeAudit
	}

	commandCtx, cancel := context.WithTimeout(ctx, w.timeout)
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
	case candidate.Runtime.Canonical() != model.RuntimeCodex:
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
	w.pending[target] = struct{}{}
	return true
}

func (w *nativeWaker) end(target model.ActorID) {
	w.mu.Lock()
	delete(w.pending, target)
	w.mu.Unlock()
}

func (w *nativeWaker) reserveRate(reservation relay.WakeReservation) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := reservation.At
	cutoff := now.Add(-time.Hour)
	kept := w.attempts[:0]
	var last time.Time
	for _, attempt := range w.attempts {
		if !attempt.At.After(cutoff) {
			continue
		}
		kept = append(kept, attempt)
		if last.IsZero() || attempt.At.After(last) {
			last = attempt.At
		}
	}
	w.attempts = kept
	if !last.IsZero() && now.Before(last.Add(w.minInterval)) {
		return "minimum_interval"
	}
	if len(w.attempts) >= w.hourlyLimit {
		return "hourly_limit"
	}
	w.attempts = append(w.attempts, reservation)
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
	if w.relay.RecordWake(outcome, reason, target) != nil {
		return errNativeWakeAudit
	}
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
