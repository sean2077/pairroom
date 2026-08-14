package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/agent"
	"github.com/sean2077/pairroom/internal/archive"
	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/room"
)

// TestNativeSessionMaterializationAndExactResume is intentionally opt-in: it
// exercises the installed, authenticated official Claude Code and Codex CLIs,
// consumes real model turns, and can take several minutes. The second round
// rebuilds the Registry and adapters from disk so it proves exact native resume
// rather than merely reusing live processes.
func TestNativeSessionMaterializationAndExactResume(t *testing.T) {
	if os.Getenv("PAIRROOM_NATIVE_E2E") != "1" {
		t.Skip("set PAIRROOM_NATIVE_E2E=1 to exercise authenticated Claude Code and Codex runtimes")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	repo := testGitRepo(t)
	commit := exec.Command("git", "-C", repo, "-c", "user.name=PairRoom Native E2E", "-c", "user.email=pairroom-e2e@example.invalid", "commit", "--allow-empty", "-m", "initial")
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("create native E2E repository HEAD: %v: %s", err, output)
	}
	serviceRoot := t.TempDir()
	registry, project := testRegistryWithRoot(t, serviceRoot, repo)
	provisioner := NewNativeProvisioner(NativeProvisionerConfig{
		Claude: agent.Config{Command: "claude", PermissionMode: "auto"},
		Codex:  agent.Config{Command: "codex", ApprovalPolicy: "untrusted", Sandbox: "readOnly"},
	})
	created, err := registry.ProvisionRoom(ctx, ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Native session materialization E2E",
		Bindings: map[model.ActorID]BindingSpec{
			model.ActorClaude: {Mode: BindingNew},
			model.ActorCodex:  {Mode: BindingNew},
		},
	}, provisioner)
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		if binding := created.Bindings[actor]; !binding.Pending || binding.SessionID != "" {
			t.Fatalf("%s binding was materialized before a real input: %#v", actor, binding)
		}
	}

	runtimeConfig := EmbeddedRuntimeConfig{
		AutoStart: true, RoutingMode: model.RoutingManual, MaxAgentHops: 2, StallWarningSeconds: -1,
		Claude: agent.Config{Command: "claude", PermissionMode: "auto"},
		Codex:  agent.Config{Command: "codex", ApprovalPolicy: "untrusted", Sandbox: "readOnly"},
	}
	first := activateNativeE2ERuntime(t, ctx, registry, created.ID, runtimeConfig)
	waitForNativeMarker(t, ctx, first.engine, model.ActorClaude, "PAIRROOM_CLAUDE_NATIVE_ROUND_1")
	waitForNativeMarker(t, ctx, first.engine, model.ActorCodex, "PAIRROOM_CODEX_NATIVE_ROUND_1")
	waitForNativeIdle(t, ctx, first)
	materialized, ok := registry.Room(created.ID)
	if !ok {
		t.Fatal("materialized Room disappeared from Registry")
	}
	firstIDs := make(map[model.ActorID]string, 2)
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		binding := materialized.Bindings[actor]
		if binding.Pending || strings.TrimSpace(binding.SessionID) == "" {
			t.Fatalf("%s binding was not materialized by its first accepted input: %#v", actor, binding)
		}
		firstIDs[actor] = binding.SessionID
	}
	shutdownNativeE2EManager(t, ctx, first.manager)

	rebuilt, err := OpenRegistry(ctx, RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	rebuiltRoom, ok := rebuilt.Room(created.ID)
	if !ok {
		t.Fatal("Room did not rebuild from its Event Log")
	}
	for actor, sessionID := range firstIDs {
		if got := rebuiltRoom.Bindings[actor]; got.Pending || got.SessionID != sessionID {
			t.Fatalf("rebuilt %s binding=%#v, want durable ID %q", actor, got, sessionID)
		}
	}

	second := activateNativeE2ERuntime(t, ctx, rebuilt, created.ID, runtimeConfig)
	waitForNativeMarker(t, ctx, second.engine, model.ActorClaude, "PAIRROOM_CLAUDE_NATIVE_ROUND_2")
	waitForNativeMarker(t, ctx, second.engine, model.ActorCodex, "PAIRROOM_CODEX_NATIVE_ROUND_2")
	waitForNativeIdle(t, ctx, second)
	resumed, ok := rebuilt.Room(created.ID)
	if !ok {
		t.Fatal("resumed Room disappeared from Registry")
	}
	for actor, sessionID := range firstIDs {
		if got := resumed.Bindings[actor].SessionID; got != sessionID {
			t.Fatalf("%s exact resume replaced %q with %q", actor, sessionID, got)
		}
	}
	shutdownNativeE2EManager(t, ctx, second.manager)

	events, err := readEventsReadOnly(filepath.Join(created.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	materializationCount := 0
	for _, event := range events {
		if event.Kind == EventRoomBindingMaterialized {
			materializationCount++
		}
	}
	if materializationCount != 2 {
		t.Fatalf("materialization events=%d, want exactly one per native binding", materializationCount)
	}
	if report := archive.Verify(created.DataDir); !report.OK {
		t.Fatalf("strict Room data verification failed: %v", report.Errors)
	}
}

type nativeE2ERuntime struct {
	manager *RuntimeManager
	runtime *embeddedRuntime
	engine  *room.Engine
}

func activateNativeE2ERuntime(t *testing.T, ctx context.Context, registry *Registry, roomID string, cfg EmbeddedRuntimeConfig) nativeE2ERuntime {
	t.Helper()
	manager, err := NewRuntimeManager(registry, EmbeddedRuntimeFactory(registry, cfg), RuntimeManagerConfig{
		Limit: 1, IdleTimeout: time.Hour, PollInterval: 20 * time.Millisecond, CloseTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := manager.Shutdown(cleanupCtx); err != nil {
			t.Errorf("cleanup native E2E runtime manager: %v", err)
		}
	})
	value, _, err := manager.Activate(ctx, roomID)
	if err != nil {
		shutdownNativeE2EManager(t, context.Background(), manager)
		t.Fatal(err)
	}
	embedded, ok := value.(*embeddedRuntime)
	if !ok {
		shutdownNativeE2EManager(t, context.Background(), manager)
		t.Fatalf("runtime type=%T, want *embeddedRuntime", value)
	}
	return nativeE2ERuntime{manager: manager, runtime: embedded, engine: embedded.engine}
}

func waitForNativeMarker(t *testing.T, ctx context.Context, engine *room.Engine, actor model.ActorID, marker string) {
	t.Helper()
	message, err := engine.Send(ctx, room.SendRequest{
		Text: "Reply with exactly " + marker + " and nothing else. Do not call tools.", To: []model.ActorID{actor},
	})
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := engine.Snapshot()
		for _, candidate := range snapshot.Messages {
			if candidate.From == actor && candidate.ReplyTo == message.ID && strings.Contains(candidate.Text, marker) {
				return
			}
		}
		for _, candidate := range snapshot.Messages {
			if candidate.ID == message.ID && candidate.Processing[actor] == model.ProcessingFailed {
				t.Fatalf("%s native turn failed: %s", actor, candidate.ProcessingDetail[actor])
			}
		}
		if participant := snapshot.Participants[actor]; participant.State == model.StateError {
			t.Fatalf("%s native runtime failed: %s", actor, participant.LastError)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s marker %q: %v", actor, marker, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitForNativeIdle(t *testing.T, ctx context.Context, runtime nativeE2ERuntime) {
	t.Helper()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for runtime.runtime.Busy() {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for native runtime to become idle: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func shutdownNativeE2EManager(t *testing.T, parent context.Context, manager *RuntimeManager) {
	t.Helper()
	if manager == nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown native E2E runtime manager: %v", err)
	}
}
