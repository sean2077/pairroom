package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/store"
)

func TestResolveRootRejectsRelativeExplicitPath(t *testing.T) {
	if _, err := ResolveRoot("relative/service-root"); err == nil || !strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("ResolveRoot relative error=%v", err)
	}
}

func TestDefaultRootIsIndependentOfCurrentWorkingDirectory(t *testing.T) {
	configRoot := filepath.Join(t.TempDir(), "config")
	switch runtime.GOOS {
	case "windows":
		t.Setenv("APPDATA", configRoot)
	case "darwin":
		home := t.TempDir()
		t.Setenv("HOME", home)
		configRoot = filepath.Join(home, "Library", "Application Support")
	default:
		t.Setenv("XDG_CONFIG_HOME", configRoot)
	}

	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(original); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	firstDirectory := t.TempDir()
	secondDirectory := t.TempDir()
	if err := os.Chdir(firstDirectory); err != nil {
		t.Fatal(err)
	}
	first, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(secondDirectory); err != nil {
		t.Fatal(err)
	}
	second, err := DefaultRoot()
	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(configRoot, "pairroom")
	if first != want || second != want {
		t.Fatalf("DefaultRoot changed with cwd: first=%q second=%q want=%q", first, second, want)
	}
	if !filepath.IsAbs(first) {
		t.Fatalf("DefaultRoot returned a relative path: %q", first)
	}
}

func testGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	command := exec.Command("git", "init", "-q", root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return root
}

func testRegistry(t *testing.T, repo string) (*Registry, Project) {
	t.Helper()
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return registry, project
}

func specs(claudeMode, codexMode BindingMode, suffix string) map[model.ActorID]BindingSpec {
	values := map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: claudeMode},
		model.ActorCodex:  {Mode: codexMode},
	}
	if claudeMode == BindingExisting {
		values[model.ActorClaude] = BindingSpec{Mode: BindingExisting, SessionID: "claude-existing-" + suffix}
	}
	if codexMode == BindingExisting {
		values[model.ActorCodex] = BindingSpec{Mode: BindingExisting, SessionID: "codex-existing-" + suffix}
	}
	return values
}

type recordingProvisioner struct {
	mu          sync.Mutex
	calls       []model.ActorID
	cleanups    int
	failActor   model.ActorID
	entered     chan model.ActorID
	release     <-chan struct{}
	newSequence atomic.Int64
}

type deferredNewProvisioner struct{}

func (deferredNewProvisioner) Provision(_ context.Context, _ Project, actor model.ActorID, spec BindingSpec, _ string) (Binding, func(context.Context) error, error) {
	if spec.Mode == BindingNew {
		return Binding{Agent: actor, Mode: BindingNew, Pending: true, BoundAt: time.Now().UTC()}, func(context.Context) error { return nil }, nil
	}
	return Binding{Agent: actor, Mode: BindingExisting, SessionID: spec.SessionID, BoundAt: time.Now().UTC()}, func(context.Context) error { return nil }, nil
}

func (p *recordingProvisioner) Provision(ctx context.Context, _ Project, actor model.ActorID, spec BindingSpec, _ string) (Binding, func(context.Context) error, error) {
	p.mu.Lock()
	p.calls = append(p.calls, actor)
	p.mu.Unlock()
	if p.entered != nil {
		select {
		case p.entered <- actor:
		case <-ctx.Done():
			return Binding{}, nil, ctx.Err()
		}
	}
	if p.release != nil {
		select {
		case <-p.release:
		case <-ctx.Done():
			return Binding{}, nil, ctx.Err()
		}
	}
	if actor == p.failActor {
		return Binding{}, nil, errors.New("synthetic vendor validation failure")
	}
	id := strings.TrimSpace(spec.SessionID)
	if spec.Mode == BindingNew {
		id = fmt.Sprintf("%s-new-%d", actor, p.newSequence.Add(1))
	}
	cleanup := func(context.Context) error {
		p.mu.Lock()
		p.cleanups++
		p.mu.Unlock()
		return nil
	}
	return Binding{Agent: actor, Mode: spec.Mode, SessionID: id, BoundAt: time.Now().UTC()}, cleanup, nil
}

func TestProjectResolverCanonicalizesRootSubdirectoryAndSymlink(t *testing.T) {
	repo := testGitRepo(t)
	subdir := filepath.Join(repo, "nested", "deeper")
	if err := os.MkdirAll(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	resolver := NewProjectResolver()
	var resolved []Project
	for _, input := range []string{repo, subdir, link} {
		project, err := resolver.Resolve(context.Background(), input)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", input, err)
		}
		resolved = append(resolved, project)
	}
	for _, project := range resolved[1:] {
		if project.ID != resolved[0].ID || project.Root != resolved[0].Root {
			t.Fatalf("equivalent path produced a different identity: %#v vs %#v", resolved[0], project)
		}
	}
}

func TestPendingNewBindingMaterializesAfterNativeInputAcceptance(t *testing.T) {
	repo := testGitRepo(t)
	registry, project := testRegistry(t, repo)
	created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Deferred native binding",
		Bindings:  specs(BindingNew, BindingNew, "deferred"),
	}, deferredNewProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
		binding := created.Bindings[actor]
		if !binding.Pending || binding.Mode != BindingNew || binding.SessionID != "" {
			t.Fatalf("%s binding was not deferred: %#v", actor, binding)
		}
	}
	if _, err := registry.CompleteBindings(context.Background(), created.ID, map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingExisting, SessionID: "replacement"},
	}, deferredNewProvisioner{}); err == nil || !strings.Contains(err.Error(), "selected bindings cannot be replaced") {
		t.Fatalf("deferred new selection was replaceable: %v", err)
	}

	appendFact := func(kind string, payload any) error {
		if kind != EventRoomBindingMaterialized {
			t.Fatalf("materialization kind=%q", kind)
		}
		return appendServiceEvent(created, kind, payload)
	}
	if _, err := registry.MaterializeBinding(context.Background(), created.ID, model.ActorClaude, "claude-native-session", func(string, any) error {
		return errors.New("synthetic append failure")
	}); err == nil || !strings.Contains(err.Error(), "synthetic append failure") {
		t.Fatalf("materialization append failure=%v", err)
	}
	if unchanged, ok := registry.Room(created.ID); !ok || !unchanged.Bindings[model.ActorClaude].Pending {
		t.Fatalf("failed materialization changed Registry: %#v ok=%v", unchanged, ok)
	}
	if _, owned := registry.BindingOwner(BindingKey{Agent: model.ActorClaude, SessionID: "claude-native-session"}); owned {
		t.Fatal("failed materialization retained native ownership")
	}
	materialized, err := registry.MaterializeBinding(context.Background(), created.ID, model.ActorClaude, "claude-native-session", appendFact)
	if err != nil {
		t.Fatal(err)
	}
	if got := materialized.Bindings[model.ActorClaude]; got.Pending || got.SessionID != "claude-native-session" || got.Mode != BindingNew {
		t.Fatalf("Claude binding was not materialized: %#v", got)
	}
	if got := materialized.Bindings[model.ActorCodex]; !got.Pending || got.SessionID != "" {
		t.Fatalf("Codex binding changed before its first accepted input: %#v", got)
	}
	if owner, ok := registry.BindingOwner(materialized.Bindings[model.ActorClaude].Key()); !ok || owner != created.ID {
		t.Fatalf("materialized binding owner=%q ok=%v", owner, ok)
	}
	if _, err := registry.MaterializeBinding(context.Background(), created.ID, model.ActorClaude, "replacement-session", appendFact); err == nil || !strings.Contains(err.Error(), "cannot be replaced") {
		t.Fatalf("binding replacement error=%v", err)
	}

	reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, ok := reopened.Room(created.ID)
	if !ok {
		t.Fatal("materialized Room was not rebuilt")
	}
	if got := rebuilt.Bindings[model.ActorClaude]; got.Pending || got.SessionID != "claude-native-session" {
		t.Fatalf("rebuilt Claude binding=%#v", got)
	}
	if got := rebuilt.Bindings[model.ActorCodex]; !got.Pending || got.SessionID != "" {
		t.Fatalf("rebuilt Codex binding=%#v", got)
	}
}

func TestLegacyNewBindingChoiceDefersAndRebuildsMaterialization(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	legacyDir := filepath.Join(t.TempDir(), "legacy-deferred-new")
	if err := writeLegacyRoom(legacyDir, repo, "legacy-deferred-new", "Legacy deferred new", "", "codex-kept"); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := registry.ImportLegacy(context.Background(), legacyDir)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := registry.CompleteBindings(context.Background(), legacy.ID, map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingNew},
	}, deferredNewProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if binding := selected.Bindings[model.ActorClaude]; !binding.Pending || binding.Mode != BindingNew || binding.SessionID != "" {
		t.Fatalf("legacy new choice was not deferred: %#v", binding)
	}
	if selected.HasBlockingPendingBindings() {
		t.Fatalf("selected deferred-new binding still blocks activation: %#v", selected.Bindings)
	}
	appendFact := func(kind string, payload any) error { return appendServiceEvent(selected, kind, payload) }
	materialized, err := registry.MaterializeBinding(context.Background(), selected.ID, model.ActorClaude, "claude-legacy-native", appendFact)
	if err != nil {
		t.Fatal(err)
	}
	if got := materialized.Bindings[model.ActorClaude]; got.Pending || got.SessionID != "claude-legacy-native" {
		t.Fatalf("legacy binding did not materialize: %#v", got)
	}

	reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, ok := reopened.Room(selected.ID)
	if !ok || rebuilt.Bindings[model.ActorClaude].SessionID != "claude-legacy-native" || rebuilt.Bindings[model.ActorCodex].SessionID != "codex-kept" {
		t.Fatalf("legacy materialization did not rebuild: %#v ok=%v", rebuilt, ok)
	}
}

func TestProjectRegistrationRejectsInvalidPathsWithoutPartialWrite(t *testing.T) {
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	invalid := []string{"relative/path", filepath.Join(t.TempDir(), "missing"), t.TempDir()}
	for _, path := range invalid {
		if _, err := registry.RegisterProject(context.Background(), path); err == nil {
			t.Fatalf("RegisterProject(%q) succeeded", path)
		}
		if got := registry.Snapshot(true); len(got.Projects) != 0 || len(got.Rooms) != 0 {
			t.Fatalf("invalid registration partially mutated registry: %#v", got)
		}
	}
}

func TestProjectRegistrationDeduplicatesEquivalentWorktreePaths(t *testing.T) {
	repo := testGitRepo(t)
	subdir := filepath.Join(repo, "sub")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	registry, project := testRegistry(t, repo)
	duplicate, err := registry.RegisterProject(context.Background(), subdir)
	if !errors.Is(err, ErrProjectAlreadyRegistered) {
		t.Fatalf("expected duplicate error, got project=%#v err=%v", duplicate, err)
	}
	if duplicate.ID != project.ID {
		t.Fatalf("duplicate returned wrong project: %#v vs %#v", duplicate, project)
	}
	if got := registry.Snapshot(true); len(got.Projects) != 1 {
		t.Fatalf("duplicate changed registry: %#v", got)
	}
}

func TestProvisionRoomSupportsAllBindingCombinations(t *testing.T) {
	repo := testGitRepo(t)
	registry, project := testRegistry(t, repo)
	provisioner := &recordingProvisioner{}
	modes := [][2]BindingMode{{BindingNew, BindingNew}, {BindingNew, BindingExisting}, {BindingExisting, BindingNew}, {BindingExisting, BindingExisting}}
	for index, mode := range modes {
		room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
			ProjectID: project.ID,
			Name:      fmt.Sprintf("Room %d", index+1),
			Bindings:  specs(mode[0], mode[1], fmt.Sprint(index)),
		}, provisioner)
		if err != nil {
			t.Fatalf("combination %v/%v: %v", mode[0], mode[1], err)
		}
		if room.ProjectID != project.ID || room.Lifecycle != RoomActive || room.DataDir == "" {
			t.Fatalf("unexpected room projection: %#v", room)
		}
		for _, actor := range []model.ActorID{model.ActorClaude, model.ActorCodex} {
			binding := room.Bindings[actor]
			if binding.Mode != specs(mode[0], mode[1], fmt.Sprint(index))[actor].Mode || binding.SessionID == "" {
				t.Fatalf("unexpected %s binding: %#v", actor, binding)
			}
			owner, ok := registry.BindingOwner(binding.Key())
			if !ok || owner != room.ID {
				t.Fatalf("binding owner=%q ok=%v for %#v", owner, ok, binding)
			}
		}
		events, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var kinds []string
		for _, event := range events {
			kinds = append(kinds, event.Kind)
			if strings.Contains(strings.ToLower(string(event.Data)), "vendor transcript") {
				t.Fatalf("vendor transcript leaked into PairRoom log: %s", event.Data)
			}
		}
		for _, required := range []string{"room.created", EventRoomProvisioned, "participant.updated"} {
			if !contains(kinds, required) {
				t.Fatalf("room log missing %q: %v", required, kinds)
			}
		}
	}
}

func TestProvisionRoomRejectsInvalidPendingBindingIdentity(t *testing.T) {
	repo := testGitRepo(t)
	registry, project := testRegistry(t, repo)
	provisioner := ProvisionerFunc(func(_ context.Context, _ Project, actor model.ActorID, spec BindingSpec, _ string) (Binding, func(context.Context) error, error) {
		return Binding{
			Agent: actor, Mode: spec.Mode, Pending: true,
			SessionID: string(actor) + "-resolved", BoundAt: time.Now().UTC(),
		}, func(context.Context) error { return nil }, nil
	})

	_, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Resolved bindings", Bindings: specs(BindingNew, BindingNew, "pending"),
	}, provisioner)
	if err == nil || !strings.Contains(err.Error(), "only a new binding without a session ID may be deferred") {
		t.Fatalf("expected invalid deferred identity error, got %v", err)
	}
	if got := registry.Snapshot(true); len(got.Rooms) != 0 {
		t.Fatalf("invalid pending bindings published a room: %#v", got.Rooms)
	}
}

func TestProvisionFailureLeavesNoVisibleRoomBindingOrDirectory(t *testing.T) {
	repo := testGitRepo(t)
	registry, project := testRegistry(t, repo)
	provisioner := &recordingProvisioner{failActor: model.ActorCodex}
	_, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Must roll back", Bindings: specs(BindingNew, BindingExisting, "rollback"),
	}, provisioner)
	if err == nil || !strings.Contains(err.Error(), "synthetic vendor") {
		t.Fatalf("expected vendor failure, got %v", err)
	}
	if got := registry.Snapshot(true); len(got.Rooms) != 0 {
		t.Fatalf("failed provisioning published a room: %#v", got.Rooms)
	}
	if _, ok := registry.BindingOwner(BindingKey{Agent: model.ActorCodex, SessionID: "codex-existing-rollback"}); ok {
		t.Fatal("failed provisioning retained binding ownership")
	}
	entries, err := os.ReadDir(registry.RoomsRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed provisioning left directories: %v", entries)
	}
	provisioner.mu.Lock()
	cleanups := provisioner.cleanups
	provisioner.mu.Unlock()
	if cleanups != 1 {
		t.Fatalf("cleanup count=%d, want 1", cleanups)
	}
}

func TestBindingIdentityIsExclusiveAcrossArchivedAndConcurrentRooms(t *testing.T) {
	repo := testGitRepo(t)
	registry, project := testRegistry(t, repo)
	shared := map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingExisting, SessionID: "shared-claude"},
		model.ActorCodex:  {Mode: BindingNew},
	}
	first, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "First", Bindings: shared}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ArchiveRoom(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "Second", Bindings: shared}, SyntheticProvisioner{}); !errors.Is(err, ErrBindingOwned) {
		t.Fatalf("archived binding was reusable: %v", err)
	}

	concurrent := map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingExisting, SessionID: "concurrent-claude"},
		model.ActorCodex:  {Mode: BindingExisting, SessionID: "concurrent-codex"},
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: fmt.Sprintf("Concurrent %d", index), Bindings: concurrent}, SyntheticProvisioner{})
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	var success, owned int
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrBindingOwned):
			owned++
		default:
			t.Fatalf("unexpected concurrent result: %v", err)
		}
	}
	if success != 1 || owned != 1 {
		t.Fatalf("success=%d owned=%d", success, owned)
	}
}

func TestRegistryRebuildsRoomsLifecycleAndBindingsFromEventLogs(t *testing.T) {
	repo := testGitRepo(t)
	root := t.TempDir()
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{ProjectID: project.ID, Name: "Before", Bindings: specs(BindingExisting, BindingExisting, "rebuild")}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	room, err = registry.RenameRoom(context.Background(), room.ID, "After")
	if err != nil {
		t.Fatal(err)
	}
	room, err = registry.ArchiveRoom(context.Background(), room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "service-registry.json")); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := rebuilt.Room(room.ID)
	if !ok || got.Name != "After" || got.Lifecycle != RoomArchived || got.ProjectID != project.ID {
		t.Fatalf("rebuilt room=%#v ok=%v", got, ok)
	}
	for _, binding := range got.Bindings {
		owner, ok := rebuilt.BindingOwner(binding.Key())
		if !ok || owner != got.ID {
			t.Fatalf("rebuilt binding owner=%q ok=%v", owner, ok)
		}
	}
}

func TestDefaultRootLegacyDiscoveryIsAutomaticAndNonDestructive(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	legacyDir := filepath.Join(serviceRoot, "rooms", "legacy-default-room")
	if err := writeLegacyRoom(legacyDir, repo, "legacy-default-room", "Legacy default", "legacy-default-claude", "legacy-default-codex"); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(legacyDir, "events.jsonl")
	before, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(eventPath)
	if err != nil {
		t.Fatal(err)
	}

	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	discovered, ok := registry.Room("legacy-default-room")
	if !ok || !discovered.Legacy || discovered.DataDir != legacyDir || discovered.HasPendingBindings() {
		t.Fatalf("unexpected discovered legacy Room: %#v ok=%v", discovered, ok)
	}
	if owner, ok := registry.BindingOwner(discovered.Bindings[model.ActorClaude].Key()); !ok || owner != discovered.ID {
		t.Fatalf("discovered Claude binding owner=%q ok=%v", owner, ok)
	}
	after, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatal("automatic legacy discovery modified events.jsonl")
	}
}

func TestLegacyImportIsExplicitAndNonDestructive(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	custom := filepath.Join(t.TempDir(), "custom-legacy")
	if err := writeLegacyRoom(custom, repo, "legacy-room", "Legacy", "legacy-claude", "legacy-codex"); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(custom, "events.jsonl")
	before, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Snapshot(true).Rooms) != 0 {
		t.Fatal("custom legacy room was auto-discovered")
	}
	room, err := registry.ImportLegacy(context.Background(), custom)
	if err != nil {
		t.Fatal(err)
	}
	if !room.Legacy || room.DataDir != custom {
		t.Fatalf("unexpected legacy projection: %#v", room)
	}
	after, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatal("legacy import modified events.jsonl")
	}
	restarted, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := restarted.Room(room.ID); !ok || got.DataDir != custom {
		t.Fatalf("checkpoint did not retain explicit legacy import: %#v ok=%v", got, ok)
	}
}

func TestLegacyPendingBindingsRequireAtomicCompletionBeforeActivation(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	custom := filepath.Join(t.TempDir(), "legacy-pending")
	if err := writeLegacyRoom(custom, repo, "legacy-pending-room", "Pending", "", "codex-kept"); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := registry.ImportLegacy(context.Background(), custom)
	if err != nil {
		t.Fatal(err)
	}
	if !legacy.Bindings[model.ActorClaude].Pending || legacy.Bindings[model.ActorCodex].Pending {
		t.Fatalf("unexpected pending projection: %#v", legacy.Bindings)
	}
	factoryCalls := atomic.Int64{}
	manager, err := NewRuntimeManager(registry, func(context.Context, Room) (RoomRuntime, error) {
		factoryCalls.Add(1)
		return nil, errors.New("factory must not run")
	}, RuntimeManagerConfig{Limit: 1, IdleTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Shutdown(context.Background())
	if _, err := manager.RequestActivation(legacy.ID); !errors.Is(err, ErrRoomBindingPending) {
		t.Fatalf("pending Room activation error=%v", err)
	}
	if factoryCalls.Load() != 0 {
		t.Fatal("pending Room reached the runtime factory")
	}

	completed, err := registry.CompleteBindings(context.Background(), legacy.ID, map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingNew},
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if completed.HasPendingBindings() {
		t.Fatalf("completion left pending bindings: %#v", completed.Bindings)
	}
	if completed.Bindings[model.ActorCodex].SessionID != "codex-kept" {
		t.Fatalf("completion replaced an existing binding: %#v", completed.Bindings)
	}
	for _, binding := range completed.Bindings {
		owner, ok := registry.BindingOwner(binding.Key())
		if !ok || owner != completed.ID {
			t.Fatalf("binding owner=%q ok=%v for %#v", owner, ok, binding)
		}
	}
	events, err := readEventsReadOnly(filepath.Join(custom, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Kind != EventRoomBindingsCompleted {
		t.Fatalf("last event=%q, want %q", events[len(events)-1].Kind, EventRoomBindingsCompleted)
	}

	restarted, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, ok := restarted.Room(completed.ID)
	if !ok || rebuilt.HasPendingBindings() || rebuilt.Bindings[model.ActorClaude].SessionID != completed.Bindings[model.ActorClaude].SessionID {
		t.Fatalf("binding completion did not rebuild: %#v ok=%v", rebuilt, ok)
	}
}

func TestBindingCompletionFailureLeavesLegacyLogAndOwnershipUnchanged(t *testing.T) {
	repo := testGitRepo(t)
	custom := filepath.Join(t.TempDir(), "legacy-pending")
	if err := writeLegacyRoom(custom, repo, "legacy-failure-room", "Pending", "", ""); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := registry.ImportLegacy(context.Background(), custom)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(custom, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &recordingProvisioner{failActor: model.ActorCodex}
	_, err = registry.CompleteBindings(context.Background(), legacy.ID, map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingNew},
		model.ActorCodex:  {Mode: BindingNew},
	}, provisioner)
	if err == nil || !strings.Contains(err.Error(), "synthetic vendor") {
		t.Fatalf("expected vendor failure, got %v", err)
	}
	after, err := os.ReadFile(filepath.Join(custom, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed binding completion appended a partial event")
	}
	got, _ := registry.Room(legacy.ID)
	if !got.HasPendingBindings() {
		t.Fatalf("failed completion changed projection: %#v", got.Bindings)
	}
	if _, owned := registry.BindingOwner(BindingKey{Agent: model.ActorClaude, SessionID: "claude-new-1"}); owned {
		t.Fatal("failed completion retained a binding reservation")
	}
}

func TestRegistryRefreshesUnavailableProjectWithoutDroppingRoom(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Durable", Bindings: specs(BindingNew, BindingNew, "unavailable"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	moved := repo + "-moved"
	if err := os.Rename(repo, moved); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	gotProject, ok := restarted.Project(project.ID)
	if !ok || gotProject.Available || gotProject.Diagnostic == "" || gotProject.Root != project.Root {
		t.Fatalf("unavailable Project projection=%#v ok=%v", gotProject, ok)
	}
	if gotRoom, ok := restarted.Room(room.ID); !ok || gotRoom.ProjectID != project.ID {
		t.Fatalf("Room was lost with unavailable Project: %#v ok=%v", gotRoom, ok)
	}
}

func TestCommittedRoomEventPoisonsRegistryWhenCheckpointCannotBeReplaced(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	registry, project := testRegistryWithRoot(t, serviceRoot, repo)
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Before checkpoint failure", Bindings: specs(BindingNew, BindingNew, "checkpoint"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(serviceRoot, "service-registry.json")
	if err := os.Remove(checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(checkpoint, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err = registry.RenameRoom(context.Background(), room.ID, "Committed despite checkpoint failure")
	if !errors.Is(err, ErrRegistryFailClosed) {
		t.Fatalf("rename error=%v", err)
	}
	if err := registry.Healthy(); !errors.Is(err, ErrRegistryFailClosed) {
		t.Fatalf("registry did not fail closed: %v", err)
	}
	projected, ok := registry.Room(room.ID)
	if !ok || projected.Name != "Committed despite checkpoint failure" {
		t.Fatalf("committed projection was rolled back: %#v ok=%v", projected, ok)
	}
	events, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Kind != EventRoomRenamed {
		t.Fatalf("durable event missing after checkpoint failure: %q", events[len(events)-1].Kind)
	}
	beforeCount := len(events)
	if _, err := registry.ArchiveRoom(context.Background(), room.ID); !errors.Is(err, ErrRegistryFailClosed) {
		t.Fatalf("poisoned registry accepted another mutation: %v", err)
	}
	events, err = readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != beforeCount {
		t.Fatalf("fail-closed mutation appended another event: before=%d after=%d", beforeCount, len(events))
	}

	if err := os.Remove(checkpoint); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, ok := restarted.Room(room.ID)
	if !ok || rebuilt.Name != projected.Name {
		t.Fatalf("event log did not rebuild committed rename: %#v ok=%v", rebuilt, ok)
	}
}

func TestRegistryRejectsDuplicateBindingCompletionDuringRebuild(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	custom := filepath.Join(t.TempDir(), "legacy-duplicate-completion")
	if err := writeLegacyRoom(custom, repo, "legacy-duplicate-completion", "Legacy", "", ""); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := registry.ImportLegacy(context.Background(), custom)
	if err != nil {
		t.Fatal(err)
	}
	completed, err := registry.CompleteBindings(context.Background(), legacy.ID, map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingNew},
		model.ActorCodex:  {Mode: BindingNew},
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	payload := roomBindingsCompletedPayload{Bindings: cloneBindings(completed.Bindings), UpdatedAt: time.Now().UTC()}
	if err := appendServiceEvent(completed, EventRoomBindingsCompleted, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot}); err == nil || !strings.Contains(err.Error(), "multiple binding-completion") {
		t.Fatalf("duplicate binding completion was accepted: %v", err)
	}
}

func TestRegistryRejectsLifecycleKindPayloadMismatch(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	registry, project := testRegistryWithRoot(t, serviceRoot, repo)
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Lifecycle integrity", Bindings: specs(BindingNew, BindingNew, "lifecycle"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if err := appendServiceEvent(room, EventRoomArchived, roomLifecyclePayload{Lifecycle: RoomActive, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot}); err == nil || !strings.Contains(err.Error(), `expected "archived"`) {
		t.Fatalf("mismatched lifecycle event was accepted: %v", err)
	}
}

func TestRegistryRejectsCrossRoomEventsDuringRebuild(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	registry, project := testRegistryWithRoot(t, serviceRoot, repo)
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Integrity", Bindings: specs(BindingNew, BindingNew, "integrity"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(room.DataDir, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatal("expected multiple events")
	}
	var event model.Event
	if err := json.Unmarshal([]byte(lines[1]), &event); err != nil {
		t.Fatal(err)
	}
	event.RoomID = "another-room"
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	lines[1] = string(encoded)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot}); err == nil || !strings.Contains(err.Error(), "belongs to room") {
		t.Fatalf("cross-Room corruption was accepted: %v", err)
	}
}

func testRegistryWithRoot(t *testing.T, root, repo string) (*Registry, Project) {
	t.Helper()
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	return registry, project
}

func writeLegacyRoom(dir, repo, roomID, name, claudeID, codexID string) error {
	eventStore, err := store.Open(dir)
	if err != nil {
		return err
	}
	defer eventStore.Close()
	created := time.Now().UTC().Add(-time.Hour)
	values := []struct {
		kind  string
		actor model.ActorID
		value any
	}{
		{"room.created", model.ActorSystem, model.RoomMeta{ID: roomID, Name: name, Repo: repo, CreatedAt: created}},
		{"participant.updated", model.ActorClaude, model.ParticipantSnapshot{ID: model.ActorClaude, DisplayName: "Claude Code", MentionHandle: "@claude", Role: model.RoleDriver, State: model.StateStopped, SessionID: claudeID, RuntimeKind: model.RuntimeClaude}},
		{"participant.updated", model.ActorCodex, model.ParticipantSnapshot{ID: model.ActorCodex, DisplayName: "Codex", MentionHandle: "@codex", Role: model.RoleReviewer, State: model.StateStopped, SessionID: codexID, RuntimeKind: model.RuntimeCodex}},
	}
	for _, value := range values {
		event, err := model.NewEvent(roomID, value.kind, value.actor, value.value)
		if err != nil {
			return err
		}
		if err := eventStore.Append(&event); err != nil {
			return err
		}
	}
	return eventStore.Close()
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func sorted(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func TestServiceLockExcludesConcurrentOwnersAndReleasesSafely(t *testing.T) {
	root := t.TempDir()
	first, err := AcquireServiceLock(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireServiceLock(root, false); !errors.Is(err, ErrServiceAlreadyRunning) {
		t.Fatalf("concurrent lock error=%v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireServiceLock(root, false)
	if err != nil {
		t.Fatalf("released lock could not be reacquired: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverServiceLockRefusesLiveOwner(t *testing.T) {
	root := t.TempDir()
	data, err := json.Marshal(map[string]any{
		"pid": os.Getpid(), "started_at": time.Now().UTC(), "nonce": "private",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "service.lock"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverServiceLock(root); !errors.Is(err, ErrServiceLockOwnerRunning) {
		t.Fatalf("RecoverServiceLock error = %v, want live-owner error", err)
	}
	if _, err := os.Stat(filepath.Join(root, "service.lock")); err != nil {
		t.Fatalf("live owner lock was removed: %v", err)
	}
}

func TestRecoverServiceLockRemovesExitedOwner(t *testing.T) {
	root := t.TempDir()
	data, err := json.Marshal(map[string]any{
		"pid": 99999999, "started_at": time.Now().UTC(), "nonce": "private",
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "service.lock")
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverServiceLock(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exited owner lock remains: %v", err)
	}
	assertNoRecoveringServiceLock(t, root)
}

func TestRecoverServiceLockLeavesReplacementCreatedAfterMove(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "service.lock")
	data, err := json.Marshal(map[string]any{
		"pid": 99999999, "started_at": time.Now().UTC(), "nonce": "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	replacement, err := json.Marshal(map[string]any{
		"pid": os.Getpid(), "started_at": time.Now().UTC(), "nonce": "new",
	})
	if err != nil {
		t.Fatal(err)
	}
	serviceLockRecoveryHook = func(_, livePath string) {
		if err := os.WriteFile(livePath, append(replacement, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { serviceLockRecoveryHook = nil })
	if err := RecoverServiceLock(root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("replacement lock missing: %v", err)
	}
	if !strings.Contains(string(got), `"nonce":"new"`) {
		t.Fatalf("replacement lock was overwritten: %s", got)
	}
	assertNoRecoveringServiceLock(t, root)
}

func TestAcquireServiceLockReportsExitedOwnerWithoutRecovering(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "service.lock")
	data, err := json.Marshal(map[string]any{
		"pid": 99999999, "started_at": time.Date(2026, 9, 2, 3, 55, 24, 0, time.UTC), "nonce": "stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = AcquireServiceLock(root, false)
	if !errors.Is(err, ErrServiceAlreadyRunning) || !strings.Contains(err.Error(), "process is not running") {
		t.Fatalf("stale owner acquire error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale lock was removed without recovery: %v", err)
	}
}

func TestServiceLockOwnerRunningReportsCurrentAndMissingPIDs(t *testing.T) {
	alive, err := ServiceLockOwnerRunning(ServiceLockInfo{PID: os.Getpid(), StartedAt: time.Now().UTC()})
	if err != nil || !alive {
		t.Fatalf("current pid alive=%v err=%v", alive, err)
	}
	alive, err = ServiceLockOwnerRunning(ServiceLockInfo{PID: 99999999, StartedAt: time.Now().UTC()})
	if err != nil || alive {
		t.Fatalf("missing pid alive=%v err=%v", alive, err)
	}
	alive, err = ServiceLockOwnerRunning(ServiceLockInfo{PID: 0, StartedAt: time.Now().UTC()})
	if err != nil || alive {
		t.Fatalf("zero pid alive=%v err=%v", alive, err)
	}
}

func assertNoRecoveringServiceLock(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "service.lock.recovering-") {
			t.Fatalf("left recovery residue %s", entry.Name())
		}
	}
}

func TestInspectServiceLockReportsSafeOwnerMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "service.lock"), []byte(`{"pid":12345,"started_at":"2026-09-02T03:55:24Z","nonce":"private"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	info, found, err := InspectServiceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if !found || info.PID != 12345 || !info.StartedAt.Equal(time.Date(2026, 9, 2, 3, 55, 24, 0, time.UTC)) {
		t.Fatalf("lock info = %#v, found=%v", info, found)
	}
}

func TestInspectServiceLockReportsMissingLockWithoutMutation(t *testing.T) {
	root := t.TempDir()
	info, found, err := InspectServiceLock(root)
	if err != nil {
		t.Fatal(err)
	}
	if found || info != (ServiceLockInfo{}) {
		t.Fatalf("missing lock info = %#v, found=%v", info, found)
	}
}

func TestRecoverServiceLockRefusesIncompleteMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "service.lock")
	if err := os.WriteFile(path, []byte(`{"pid":12345}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverServiceLock(root); err == nil || !strings.Contains(err.Error(), "cannot verify service lock owner") {
		t.Fatalf("incomplete lock recovery error = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("incomplete lock was removed: %v", err)
	}
}

func TestRecoverServiceLockRemovesOnlyTheSelectedRootLock(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	for _, dir := range []string{root, other} {
		data, err := json.Marshal(map[string]any{"pid": 99999999, "started_at": time.Now().UTC(), "nonce": "stale"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "service.lock"), append(data, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecoverServiceLock(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "service.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected lock still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "service.lock")); err != nil {
		t.Fatalf("unselected lock changed: %v", err)
	}
	assertNoRecoveringServiceLock(t, root)
}

func TestFailedLegacyImportLeavesNoPartialProjectOrBindingReservation(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	owned, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Binding owner",
		Bindings: map[model.ActorID]BindingSpec{
			model.ActorClaude: {Mode: BindingExisting, SessionID: "owner-claude"},
			model.ActorCodex:  {Mode: BindingExisting, SessionID: "shared-codex"},
		},
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}

	otherRepo := testGitRepo(t)
	custom := filepath.Join(t.TempDir(), "conflicting-legacy")
	if err := writeLegacyRoom(custom, otherRepo, "conflicting-legacy-room", "Conflicting legacy", "unowned-claude", "shared-codex"); err != nil {
		t.Fatal(err)
	}
	before := registry.Snapshot(true)
	if _, err := registry.ImportLegacy(context.Background(), custom); !errors.Is(err, ErrBindingOwned) {
		t.Fatalf("ImportLegacy error=%v, want ErrBindingOwned", err)
	}
	after := registry.Snapshot(true)
	if len(after.Projects) != len(before.Projects) || len(after.Rooms) != len(before.Rooms) {
		t.Fatalf("failed import partially changed registry: before=%#v after=%#v", before, after)
	}
	if _, ok := registry.Project(projectID(otherRepo)); ok {
		t.Fatal("failed import retained the legacy Project")
	}
	if owner, ok := registry.BindingOwner(BindingKey{Agent: model.ActorClaude, SessionID: "unowned-claude"}); ok {
		t.Fatalf("failed import retained a partial Claude reservation owned by %q", owner)
	}
	if owner, ok := registry.BindingOwner(BindingKey{Agent: model.ActorCodex, SessionID: "shared-codex"}); !ok || owner != owned.ID {
		t.Fatalf("existing binding owner changed: owner=%q ok=%v", owner, ok)
	}
}

func TestRegistryRejectsBindingCompletionForProvisionedRoom(t *testing.T) {
	serviceRoot := t.TempDir()
	repo := testGitRepo(t)
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot})
	if err != nil {
		t.Fatal(err)
	}
	project, err := registry.RegisterProject(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Already bound", Bindings: specs(BindingNew, BindingNew, "bound"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	payload := roomBindingsCompletedPayload{Bindings: cloneBindings(created.Bindings), UpdatedAt: time.Now().UTC()}
	if err := appendServiceEvent(created, EventRoomBindingsCompleted, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot}); err == nil || !strings.Contains(err.Error(), "provisioned Room cannot replace") {
		t.Fatalf("forged binding completion was accepted: %v", err)
	}
}

func TestRegistryRejectsLegacyCompletionThatReplacesExistingBinding(t *testing.T) {
	repo := testGitRepo(t)
	custom := filepath.Join(t.TempDir(), "legacy-binding-replacement")
	const roomID = "legacy-binding-replacement-room"
	if err := writeLegacyRoom(custom, repo, roomID, "Legacy replacement", "claude-keep", ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	payload := roomBindingsCompletedPayload{
		Bindings: map[model.ActorID]Binding{
			model.ActorClaude: {Agent: model.ActorClaude, Mode: BindingExisting, SessionID: "claude-replaced", BoundAt: now},
			model.ActorCodex:  {Agent: model.ActorCodex, Mode: BindingNew, SessionID: "codex-new", BoundAt: now},
		},
		UpdatedAt: now,
	}
	if err := appendServiceEvent(Room{ID: roomID, DataDir: custom}, EventRoomBindingsCompleted, payload); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.ImportLegacy(context.Background(), custom); err == nil || !strings.Contains(err.Error(), "replaces existing claude session") {
		t.Fatalf("legacy binding replacement was accepted: %v", err)
	}
}

func TestRelativeLegacyRepoPathDoesNotDependOnServiceWorkingDirectory(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "legacy-relative-repo")
	if err := writeLegacyRoom(custom, filepath.Join("relative", "repo"), "legacy-relative-room", "Legacy relative", "claude-relative", "codex-relative"); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	room, err := registry.ImportLegacy(context.Background(), custom)
	if err != nil {
		t.Fatal(err)
	}
	project, ok := registry.Project(room.ProjectID)
	if !ok {
		t.Fatal("legacy Project was not indexed")
	}
	if project.Available || filepath.IsAbs(project.Root) || project.Root != filepath.Clean(filepath.Join("relative", "repo")) {
		t.Fatalf("relative legacy repo was rebound to the Service cwd: %#v", project)
	}
}

func TestCanceledProvisionAndLifecycleDoNotCommit(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	ctx, cancel := context.WithCancel(context.Background())
	provisioner := ProvisionerFunc(func(_ context.Context, _ Project, actor model.ActorID, spec BindingSpec, _ string) (Binding, func(context.Context) error, error) {
		id := strings.TrimSpace(spec.SessionID)
		if id == "" {
			id = string(actor) + "-canceled-provision"
		}
		if actor == model.ActorCodex {
			cancel()
		}
		return Binding{Agent: actor, Mode: spec.Mode, SessionID: id, BoundAt: time.Now().UTC()}, func(context.Context) error { return nil }, nil
	})
	_, err := registry.ProvisionRoom(ctx, ProvisionRequest{
		ProjectID: project.ID, Name: "Canceled", Bindings: specs(BindingNew, BindingNew, "canceled"),
	}, provisioner)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ProvisionRoom error=%v, want context.Canceled", err)
	}
	if got := registry.Snapshot(true); len(got.Rooms) != 0 {
		t.Fatalf("canceled provisioning committed a Room: %#v", got.Rooms)
	}
	entries, err := os.ReadDir(registry.RoomsRoot())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("canceled provisioning left data directories: %v", entries)
	}

	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Durable", Bindings: specs(BindingNew, BindingNew, "durable"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := registry.RenameRoom(canceled, room.ID, "Must not commit"); !errors.Is(err, context.Canceled) {
		t.Fatalf("RenameRoom error=%v, want context.Canceled", err)
	}
	projected, ok := registry.Room(room.ID)
	if !ok || projected.Name != room.Name {
		t.Fatalf("canceled rename changed projection: %#v", projected)
	}
	after, err := readEventsReadOnly(filepath.Join(room.DataDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("canceled rename appended an event: before=%d after=%d", len(before), len(after))
	}
}

func TestCanceledLegacyBindingCompletionDoesNotCommit(t *testing.T) {
	repo := testGitRepo(t)
	custom := filepath.Join(t.TempDir(), "legacy-canceled-completion")
	if err := writeLegacyRoom(custom, repo, "legacy-canceled-completion", "Legacy canceled", "", ""); err != nil {
		t.Fatal(err)
	}
	registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := registry.ImportLegacy(context.Background(), custom)
	if err != nil {
		t.Fatal(err)
	}
	before, err := readEventsReadOnly(filepath.Join(custom, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	provisioner := ProvisionerFunc(func(_ context.Context, _ Project, actor model.ActorID, spec BindingSpec, _ string) (Binding, func(context.Context) error, error) {
		if actor == model.ActorCodex {
			cancel()
		}
		return Binding{Agent: actor, Mode: spec.Mode, SessionID: string(actor) + "-canceled-binding", BoundAt: time.Now().UTC()}, func(context.Context) error { return nil }, nil
	})
	_, err = registry.CompleteBindings(ctx, legacy.ID, map[model.ActorID]BindingSpec{
		model.ActorClaude: {Mode: BindingNew}, model.ActorCodex: {Mode: BindingNew},
	}, provisioner)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CompleteBindings error=%v, want context.Canceled", err)
	}
	projected, ok := registry.Room(legacy.ID)
	if !ok || !projected.HasPendingBindings() {
		t.Fatalf("canceled completion changed bindings: %#v", projected.Bindings)
	}
	after, err := readEventsReadOnly(filepath.Join(custom, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("canceled completion appended an event: before=%d after=%d", len(before), len(after))
	}
}

func TestRegistryRejectsInvalidLifecycleTransitionDuringRebuild(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	registry, project := testRegistryWithRoot(t, serviceRoot, repo)
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Lifecycle transition", Bindings: specs(BindingNew, BindingNew, "transition"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := appendServiceEvent(room, EventRoomArchived, roomLifecyclePayload{Lifecycle: RoomArchived, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := appendServiceEvent(room, EventRoomArchived, roomLifecyclePayload{Lifecycle: RoomArchived, UpdatedAt: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot}); err == nil || !strings.Contains(err.Error(), "already archived") {
		t.Fatalf("duplicate archive transition was accepted: %v", err)
	}
}

func TestBindingAndRequestValidationRejectAmbiguousIdentities(t *testing.T) {
	binding := Binding{Agent: model.ActorClaude, Mode: BindingExisting, SessionID: " session-id ", BoundAt: time.Now().UTC()}
	if err := binding.Validate(); err == nil || !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("ambiguous binding identity was accepted: %v", err)
	}
	request := ProvisionRequest{
		ProjectID: "project-test", Name: "Unexpected binding",
		Bindings: map[model.ActorID]BindingSpec{
			model.ActorClaude: {Mode: BindingNew},
			model.ActorCodex:  {Mode: BindingNew},
			model.ActorSystem: {Mode: BindingExisting, SessionID: "unexpected"},
		},
	}
	if err := request.Validate(); err == nil || !strings.Contains(err.Error(), "exactly two") {
		t.Fatalf("request with extra binding was accepted: %v", err)
	}
	if err := validateProvisionedProject(Project{ID: projectID("relative/repo"), Root: "relative/repo"}); err == nil {
		t.Fatal("relative provisioned Project Identity was accepted")
	}
}

func TestRegistryRejectsAlteredTranscriptBoundaryDuringRebuild(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	registry, project := testRegistryWithRoot(t, serviceRoot, repo)
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID,
		Name:      "Boundary integrity",
		Bindings:  specs(BindingNew, BindingNew, "boundary-integrity"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(room.DataDir, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) < 2 {
		t.Fatal("expected provisioning event")
	}
	var event model.Event
	if err := json.Unmarshal([]byte(lines[1]), &event); err != nil {
		t.Fatal(err)
	}
	if event.Kind != EventRoomProvisioned {
		t.Fatalf("event 2 kind=%q", event.Kind)
	}
	var payload roomProvisionedPayload
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatal(err)
	}
	payload.TranscriptBoundaryNotice = "Vendor transcript history is available."
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event.Data = encodedPayload
	encodedEvent, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	lines[1] = string(encodedEvent)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot}); err == nil || !strings.Contains(err.Error(), "transcript boundary") {
		t.Fatalf("altered transcript boundary policy was accepted: %v", err)
	}
}

func TestRegistryRejectsMissingTranscriptBoundaryDuringRebuild(t *testing.T) {
	repo := testGitRepo(t)
	serviceRoot := t.TempDir()
	registry, project := testRegistryWithRoot(t, serviceRoot, repo)
	room, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Missing boundary", Bindings: specs(BindingNew, BindingNew, "missing-boundary"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(room.DataDir, "events.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var event model.Event
	if err := json.Unmarshal([]byte(lines[1]), &event); err != nil {
		t.Fatal(err)
	}
	var payload roomProvisionedPayload
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		t.Fatal(err)
	}
	payload.TranscriptBoundaryNotice = ""
	event.Data, err = json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	lines[1] = string(mustJSON(t, event))
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: serviceRoot}); err == nil || !strings.Contains(err.Error(), "transcript boundary") {
		t.Fatalf("missing transcript boundary policy was accepted: %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestBindingValidationRequiresDurableBoundTime(t *testing.T) {
	binding := Binding{Agent: model.ActorClaude, Mode: BindingExisting, SessionID: "session-without-time"}
	if err := binding.Validate(); err == nil || !strings.Contains(err.Error(), "time") {
		t.Fatalf("binding without durable timestamp was accepted: %v", err)
	}
}
