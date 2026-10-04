package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
)

func TestCodexSlowMetadataProbeReachesEmbeddedStartup(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_PROBE_DELAY", "8s")
	for _, phase := range []string{"version", "help"} {
		t.Run(phase, func(t *testing.T) {
			t.Setenv("PAIRROOM_CODEX_PROBE_DELAY_PHASE", phase)
			adapter := SlotFactory(false, model.RuntimeCodex)(Config{
				Actor: model.ActorSlot1, Command: os.Args[0], Repo: t.TempDir(), DataDir: t.TempDir(),
			}, func(model.RuntimeEvent) {})
			t.Cleanup(func() { stopWithin(adapter, 5*time.Second) })
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := adapter.Start(ctx); err != nil {
				t.Fatalf("Codex metadata %s delayed beyond the old 6s budget blocked Embedded startup: %v", phase, err)
			}
			if adapter.Actor() != model.ActorSlot1 || adapter.State() != model.StateIdle || adapter.SessionID() != "thread-new" {
				t.Fatalf("Codex slot1 did not complete initialization: actor=%s state=%s session=%q", adapter.Actor(), adapter.State(), adapter.SessionID())
			}
		})
	}
}

func TestCodexMetadataProbeBoundedAndCancellable(t *testing.T) {
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_PROBE_DELAY", "1h")
	t.Setenv("PAIRROOM_CODEX_PROBE_DELAY_PHASE", "version")
	cfg := Config{Actor: model.ActorSlot1, Runtime: model.RuntimeCodex, Command: os.Args[0]}
	t.Run("probe ceiling", func(t *testing.T) {
		before := codexProbeCommandTimeout
		codexProbeCommandTimeout = 500 * time.Millisecond
		t.Cleanup(func() { codexProbeCommandTimeout = before })
		_, err := ProbeRuntime(context.Background(), cfg)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "(--version)") {
			t.Fatalf("hung metadata command did not return a phase-specific timeout: %v", err)
		}
	})
	t.Run("shorter caller deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := ProbeRuntime(ctx, cfg)
		if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() == nil {
			t.Fatalf("Codex probe ignored the caller's shorter deadline: %v", err)
		}
	})
	t.Run("cancel before process start", func(t *testing.T) {
		phaseFile := filepath.Join(t.TempDir(), "probe-started")
		t.Setenv("PAIRROOM_CODEX_PROBE_PHASE_FILE", phaseFile)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ProbeRuntime(ctx, cfg)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled metadata probe = %v", err)
		}
		if _, err := os.Stat(phaseFile); !os.IsNotExist(err) {
			t.Fatalf("cancelled probe started a child: %v", err)
		}
	})
	t.Run("cancel running metadata command", func(t *testing.T) {
		phaseFile := filepath.Join(t.TempDir(), "probe-running")
		t.Setenv("PAIRROOM_CODEX_PROBE_PHASE_FILE", phaseFile)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := ProbeRuntime(ctx, cfg); done <- err }()
		deadline := time.After(5 * time.Second)
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := os.Stat(phaseFile); err == nil {
				break
			}
			select {
			case err := <-done:
				t.Fatalf("metadata command ended before the fixture barrier: %v", err)
			case <-deadline:
				t.Fatal("metadata command did not reach the fixture barrier")
			case <-tick.C:
			}
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("running metadata cancellation = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("running metadata command ignored cancellation")
		}
	})
}

func TestCodexProbeBudgetDoesNotExtendOtherRuntimes(t *testing.T) {
	if probeCommandTimeout != 6*time.Second || codexProbeCommandTimeout != 15*time.Second {
		t.Fatalf("default metadata budgets = %s/%s; want 6s/15s", probeCommandTimeout, codexProbeCommandTimeout)
	}
	t.Setenv("PAIRROOM_CODEX_HELPER", "1")
	t.Setenv("PAIRROOM_CODEX_PROBE_DELAY", "1h")
	t.Setenv("PAIRROOM_CODEX_PROBE_DELAY_PHASE", "version")
	before := probeCommandTimeout
	probeCommandTimeout = 300 * time.Millisecond
	t.Cleanup(func() { probeCommandTimeout = before })
	for _, kind := range []model.RuntimeKind{model.RuntimeClaude, model.RuntimeGrok, model.RuntimeGemini} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := ProbeRuntime(ctx, Config{Actor: model.ActorSlot1, Runtime: kind, Command: os.Args[0]})
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				t.Fatalf("%s used the Codex budget instead of its short metadata budget: %v", kind, err)
			}
		})
	}
}
