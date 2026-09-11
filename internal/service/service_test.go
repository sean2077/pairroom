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
	"github.com/sean2077/pairroom/internal/version"
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

func TestMixedNewAndExistingBindingsRebuildAfterMaterialization(t *testing.T) {
	registry, project := testRegistry(t, testGitRepo(t))
	selected, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "Mixed bindings",
		Bindings: map[model.ActorID]BindingSpec{
			model.ActorClaude: {Mode: BindingNew},
			model.ActorCodex:  {Mode: BindingExisting, SessionID: "codex-kept"},
		},
	}, deferredNewProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	if binding := selected.Bindings[model.ActorClaude]; !binding.Pending || binding.Mode != BindingNew || binding.SessionID != "" {
		t.Fatalf("new choice was not deferred: %#v", binding)
	}
	if err := selected.Validate(); err != nil {
		t.Fatal(err)
	}
	appendFact := func(kind string, payload any) error { return appendServiceEvent(selected, kind, payload) }
	materialized, err := registry.MaterializeBinding(context.Background(), selected.ID, model.ActorClaude, "claude-native", appendFact)
	if err != nil {
		t.Fatal(err)
	}
	if got := materialized.Bindings[model.ActorClaude]; got.Pending || got.SessionID != "claude-native" {
		t.Fatalf("binding did not materialize: %#v", got)
	}

	reopened, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, ok := reopened.Room(selected.ID)
	if !ok || rebuilt.Bindings[model.ActorClaude].SessionID != "claude-native" || rebuilt.Bindings[model.ActorCodex].SessionID != "codex-kept" {
		t.Fatalf("materialization did not rebuild: %#v ok=%v", rebuilt, ok)
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

func TestRegistryRejectsStandaloneRoomWithoutChangingFiles(t *testing.T) {
	for _, schema := range []int{version.StoreSchema - 1, version.StoreSchema} {
		t.Run(fmt.Sprintf("schema-%d", schema), func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "rooms", "unsupported-room")
			if err := writeLegacyRoom(dir, testGitRepo(t), "unsupported-room", "Unsupported", "old-claude", "old-codex"); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "metadata.json")
			if err := os.WriteFile(marker, mustJSON(t, map[string]any{"format": "pairroom-jsonl", "schema_version": schema}), 0o600); err != nil {
				t.Fatal(err)
			}
			before := map[string][]byte{}
			for _, name := range []string{"events.jsonl", "metadata.json"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				before[name] = data
			}
			_, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
			if err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("standalone Room was accepted: %v", err)
			}
			for name, want := range before {
				if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != string(want) {
					t.Fatalf("rejection changed %s: %q, %v", name, got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "service-registry.json")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("rejected Room published a checkpoint: %v", err)
			}
		})
	}
}

func TestRegistryDoesNotFollowExternalCheckpointRooms(t *testing.T) {
	owner, project := testRegistry(t, testGitRepo(t))
	external, err := owner.ProvisionRoom(context.Background(), ProvisionRequest{
		ProjectID: project.ID, Name: "External", Bindings: specs(BindingExisting, BindingExisting, "external"),
	}, SyntheticProvisioner{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(external.DataDir, "events.jsonl")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, schema := range []int{1, 2} {
		t.Run(fmt.Sprintf("checkpoint-%d", schema), func(t *testing.T) {
			root := t.TempDir()
			snapshot := owner.Snapshot(true)
			snapshot.Schema = schema
			if err := os.WriteFile(filepath.Join(root, "service-registry.json"), mustJSON(t, snapshot), 0o600); err != nil {
				t.Fatal(err)
			}
			registry, err := OpenRegistry(context.Background(), RegistryConfig{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			if got := registry.Snapshot(true); len(got.Rooms) != 0 {
				t.Fatalf("external Room was loaded from checkpoint: %#v", got.Rooms)
			}
			for _, binding := range external.Bindings {
				if owner, ok := registry.BindingOwner(binding.Key()); ok {
					t.Fatalf("external binding was claimed by %q", owner)
				}
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
				t.Fatalf("external Event Log changed: %v", err)
			}
		})
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

// Deliberately unsupported fixtures exercise rejection, not a compatibility path.
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
		{"participant.updated", model.ActorClaude, model.ParticipantSnapshot{ID: model.ActorClaude, DisplayName: "Claude Code", MentionHandle: "@claude", Role: "driver", State: model.StateStopped, SessionID: claudeID, RuntimeKind: model.RuntimeClaude}},
		{"participant.updated", model.ActorCodex, model.ParticipantSnapshot{ID: model.ActorCodex, DisplayName: "Codex", MentionHandle: "@codex", Role: "reviewer", State: model.StateStopped, SessionID: codexID, RuntimeKind: model.RuntimeCodex}},
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

func TestRegistryRejectsRetiredServiceEvents(t *testing.T) {
	for _, kind := range []string{"service.room.bindings.completed", "service.legacy.imported"} {
		t.Run(kind, func(t *testing.T) {
			registry, project := testRegistry(t, testGitRepo(t))
			created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
				ProjectID: project.ID, Name: "Already bound", Bindings: specs(BindingNew, BindingNew, "bound"),
			}, SyntheticProvisioner{})
			if err != nil {
				t.Fatal(err)
			}
			if err := appendServiceEvent(created, kind, map[string]any{"bindings": created.Bindings, "updated_at": time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(created.DataDir, "events.jsonl")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()}); err == nil || !strings.Contains(err.Error(), "unsupported retired Room event") {
				t.Fatalf("retired event was accepted: %v", err)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
				t.Fatalf("rejection modified Room history: %v", err)
			}
		})
	}
}

func TestRegistryRequiresCurrentProvisioningAndExplicitSelections(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*roomProvisionedPayload)
		want   string
	}{
		{"schema-1", func(p *roomProvisionedPayload) { p.Schema = 1 }, "unsupported room service schema"},
		{"schema-2", func(p *roomProvisionedPayload) { p.Schema = 2 }, "unsupported room service schema"},
		{"future-schema", func(p *roomProvisionedPayload) { p.Schema = 5 }, "unsupported room service schema"},
		{"missing-collaboration", func(p *roomProvisionedPayload) { p.Collaboration = nil }, "requires collaboration"},
		{"missing-agents", func(p *roomProvisionedPayload) { p.Agents = nil }, "Agent selections"},
		{"missing-agent-2", func(p *roomProvisionedPayload) { delete(p.Agents, model.ActorCodex) }, "Agent selections"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry, project := testRegistry(t, testGitRepo(t))
			created, err := registry.ProvisionRoom(context.Background(), ProvisionRequest{
				ProjectID: project.ID, Name: "Current contract", Bindings: specs(BindingNew, BindingNew, "contract"),
			}, SyntheticProvisioner{})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(created.DataDir, "events.jsonl")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
			var event model.Event
			if err := json.Unmarshal([]byte(lines[1]), &event); err != nil {
				t.Fatal(err)
			}
			if event.Kind != EventRoomProvisioned {
				t.Fatalf("event 2 is %q", event.Kind)
			}
			var payload roomProvisionedPayload
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&payload)
			event.Data = mustJSON(t, payload)
			lines[1] = string(mustJSON(t, event))
			before := strings.Join(lines, "\n") + "\n"
			if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenRegistry(context.Background(), RegistryConfig{Root: registry.Root()}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid Room facts accepted: %v", err)
			}
			if after, err := os.ReadFile(path); err != nil || string(after) != before {
				t.Fatalf("invalid history was rewritten: %v", err)
			}
		})
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
