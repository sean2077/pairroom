package relayclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// A locator is a disposable, secret-free pointer, not another binding store.
// Its filename is scoped by runtime/session and workspace/slot; no transcript,
// credential, pending publication or vendor configuration is copied here.
// The original private state and the Service remain authoritative.
type sessionLocator struct {
	Schema     int           `json:"schema"`
	Workspace  string        `json:"workspace"`
	Room       string        `json:"room"`
	Slot       model.ActorID `json:"slot"`
	BindID     string        `json:"bind_id"`
	Generation uint64        `json:"generation"`
}

func locatorFor(s State) sessionLocator {
	return sessionLocator{1, s.Workspace, s.Room, s.Slot, s.BindID, s.Generation}
}

// existingDir is the read-only counterpart of secureDir. Discovery must never
// create directories in a candidate workspace or follow a state symlink.
func existingDir(root string, parts ...string) (string, error) {
	path := root
	for _, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("relay discovery path must not contain symlinks or non-directories")
		}
	}
	return path, nil
}

func locatorDirectory(caller nativeCaller, create bool) (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	// Hash even the runtime: environment/hook metadata is never a path part.
	parts := []string{"pairroom", "relay-sessions", relay.Digest(string(caller.runtime) + "\x00" + caller.session)}
	if create {
		if err := os.MkdirAll(base, 0700); err != nil {
			return "", err
		}
		return secureDir(base, parts...)
	}
	return existingDir(base, parts...)
}

func locatorFilename(s State) string {
	return relay.Digest(s.Workspace+"\x00"+s.Room+"\x00"+string(s.Slot)) + ".json"
}

// Called only with confirmed local state, under the binding's slot lock. Each
// binding has its own file, so concurrent sessions never rewrite a shared map.
func rememberSession(s State) error {
	if s.Schema != 2 || s.Generation == 0 || s.SessionID == "" || !safePart(s.BindID) || !safePart(s.Room) || !s.Slot.ValidParticipant() || !filepath.IsAbs(s.Workspace) {
		return errors.New("cannot index an unconfirmed relay binding")
	}
	dir, err := locatorDirectory(nativeCaller{runtime: s.Runtime, session: s.SessionID}, true)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, locatorFilename(s))
	want := locatorFor(s)
	var previous sessionLocator
	if err := readPrivate(path, &previous); err == nil && previous == want {
		return nil // no per-turn rewrites of an unchanged locator
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return relay.AtomicJSON(path, want)
}

func forgetSession(s State) error {
	dir, err := locatorDirectory(nativeCaller{runtime: s.Runtime, session: s.SessionID}, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	path := filepath.Join(dir, locatorFilename(s))
	var previous sessionLocator
	if err := readPrivate(path, &previous); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if previous != locatorFor(s) {
		return nil // never remove a newer binding's pointer
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func indexedSessions(caller nativeCaller) ([]State, error) {
	if caller.session == "" {
		return nil, nil
	}
	dir, err := locatorDirectory(caller, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	file, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	entries, err := file.ReadDir(129)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > 128 {
		return nil, errors.New("too many native session locators; inspect relay-sessions before retrying")
	}
	var states []State
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue // interrupted AtomicJSON temporary; never a discovery input
		}
		if !strings.HasSuffix(entry.Name(), ".json") || !entry.Type().IsRegular() {
			return nil, errors.New("invalid native session locator file")
		}
		var loc sessionLocator
		if err := readPrivate(filepath.Join(dir, entry.Name()), &loc); err != nil {
			return nil, err
		}
		if loc.Schema != 1 || !filepath.IsAbs(loc.Workspace) || !safePart(loc.Room) || !loc.Slot.ValidParticipant() || !safePart(loc.BindID) || loc.Generation == 0 {
			return nil, errors.New("invalid native session locator; inspect the disposable relay-sessions index")
		}
		canonical, err := filepath.EvalSymlinks(loc.Workspace)
		if err != nil || !sameWorkspace(canonical, loc.Workspace) {
			return nil, errors.New("bound relay workspace is unavailable or redirected; restore it before retrying")
		}
		stateDir, err := existingDir(loc.Workspace, ".pairroom", "rooms", loc.Room, "slots", string(loc.Slot))
		if errors.Is(err, os.ErrNotExist) {
			continue // unbound; a stale locator cannot restore state or credentials
		}
		if err != nil {
			return nil, err
		}
		var s State
		if err := readPrivate(filepath.Join(stateDir, "state.json"), &s); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		if s.Schema != 2 || s.Runtime != caller.runtime || s.SessionID != caller.session || locatorFor(s) != loc {
			continue // replaced/revoked identity: never follow the new session
		}
		if entry.Name() != locatorFilename(s) {
			return nil, errors.New("native session locator filename mismatch")
		}
		states = append(states, s)
	}
	return states, nil
}

// Filesystem spelling normalization is not worktree merging. Distinct linked
// worktrees retain distinct roots even when they share a Git common directory.
func sameWorkspace(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	return a == b || runtime.GOOS == "windows" && strings.EqualFold(a, b)
}

func matchingSessions(root string, caller nativeCaller) ([]State, error) {
	if caller.session == "" {
		return nil, nil
	}
	paths, err := statePaths(root)
	if err != nil {
		return nil, err
	}
	var states []State
	for _, path := range paths {
		var s State
		if err := readPrivate(path, &s); err != nil {
			return nil, err
		}
		if s.Schema != 2 || s.Generation == 0 || s.Runtime != caller.runtime || s.SessionID != caller.session {
			continue
		}
		if !safePart(s.Room) || !s.Slot.ValidParticipant() || !safePart(s.BindID) || !sameWorkspace(s.Workspace, root) || filepath.Clean(path) != filepath.Join(root, ".pairroom", "rooms", s.Room, "slots", string(s.Slot), "state.json") {
			return nil, errors.New("invalid local relay binding identity")
		}
		states = append(states, s)
	}
	return states, nil
}

// On a cold lookup only, use the Service's registered project roots to recover
// locators made by older clients. This is bounded metadata discovery, not a disk
// or transcript scan; custom Services remain selectable with --service-file.
func discoverSessions(ctx context.Context, caller nativeCaller, endpointPath string) ([]State, error) {
	if caller.session == "" {
		return nil, nil
	}
	if endpointPath == "" {
		var err error
		endpointPath, err = defaultEndpoint()
		if err != nil {
			return nil, err
		}
	}
	endpointPath, err := filepath.Abs(endpointPath)
	if err != nil {
		return nil, err
	}
	endpoint, err := relay.ReadEndpoint(endpointPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var snapshot serviceSnapshot
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
		return nil, err
	}
	if len(snapshot.Projects) > 128 {
		return nil, errors.New("too many projects for cold session discovery; pass --repo for the bound workspace")
	}
	var states []State
	seen := map[string]bool{}
	for _, project := range snapshot.Projects {
		if !filepath.IsAbs(project.Root) || seen[project.Root] {
			continue
		}
		seen[project.Root] = true
		canonical, err := filepath.EvalSymlinks(project.Root)
		if errors.Is(err, os.ErrNotExist) {
			continue // unrelated offline project; indexed missing roots fail above
		}
		if err != nil {
			return nil, err
		}
		matches, err := matchingSessions(canonical, caller)
		if err != nil {
			return nil, err
		}
		for _, s := range matches {
			if sameWorkspace(s.EndpointPath, endpointPath) {
				states = append(states, s)
			}
		}
	}
	return states, nil
}

func selectSessionWorkspace(ctx context.Context, states []State, action string, o *options) (string, error) {
	if action == "bind" && o.create {
		return "", errors.New("this native session is already associated; use pairroom relay bind to resume it, not bind --create")
	}
	var explicitRoot string
	if o.repoExplicit {
		var err error
		explicitRoot, err = workspace(ctx, o.repo)
		if err != nil {
			return "", err
		}
	}
	var endpoint string
	if o.endpoint != "" {
		var err error
		endpoint, err = filepath.Abs(o.endpoint)
		if err != nil {
			return "", err
		}
	}
	var matches []State
	for _, s := range states {
		if o.room != "" && o.room != s.Room || o.slot != "" && normalizeSlot(o.slot) != string(s.Slot) || explicitRoot != "" && !sameWorkspace(explicitRoot, s.Workspace) || endpoint != "" && !sameWorkspace(endpoint, s.EndpointPath) {
			continue
		}
		matches = append(matches, s)
	}
	if len(matches) == 0 {
		return "", errors.New("explicit relay target conflicts with this native session's binding; directory changes do not change Room ownership")
	}
	if len(matches) != 1 {
		return "", errors.New("native session matches multiple relay bindings; pass --repo/--service-file/--room/--slot explicitly")
	}
	s := matches[0]
	o.room, o.slot = s.Room, string(s.Slot)
	if action == "bind" && o.endpoint == "" {
		o.endpoint = s.EndpointPath
	}
	return s.Workspace, nil
}

// resolveSessionWorkspace is shared by foreground commands and approved hooks.
// cwd is a cold discovery hint, never the identity of an already-bound caller.
// It never chdirs: file arguments remain relative to the actual tool-call cwd.
func resolveSessionWorkspace(ctx context.Context, action string, o *options, caller nativeCaller) (string, error) {
	if action == "hook" {
		if session := sessionIDFromEnv(caller.runtime); session != "" && session != caller.session {
			known, err := indexedSessions(nativeCaller{runtime: caller.runtime, session: session})
			if err != nil {
				return "", err
			}
			if len(known) > 0 {
				return "", errHookSessionMismatch
			}
		}
	}
	states, err := indexedSessions(caller)
	if err != nil {
		return "", err
	}
	if len(states) > 0 {
		return selectSessionWorkspace(ctx, states, action, o)
	}
	var roots []string
	addRoot := func(path string) {
		if path == "" {
			return
		}
		root, e := workspace(ctx, path)
		if e != nil {
			return
		}
		for _, previous := range roots {
			if sameWorkspace(previous, root) {
				return
			}
		}
		roots = append(roots, root)
	}
	addRoot(o.repo)
	if caller.runtime == model.RuntimeClaude && !o.repoExplicit {
		addRoot(os.Getenv("CLAUDE_PROJECT_DIR"))
	}
	for _, root := range roots {
		matches, err := matchingSessions(root, caller)
		if err != nil {
			return "", err
		}
		states = append(states, matches...)
	}
	if len(states) == 0 && !o.repoExplicit && action != "hook" && !(action == "unbind" && o.localOnly) {
		states, err = discoverSessions(ctx, caller, o.endpoint)
		if err != nil {
			return "", err
		}
	}
	if len(states) > 0 {
		return selectSessionWorkspace(ctx, states, action, o)
	}
	if action == "hook" {
		// Preserve the existing environment/PID mismatch diagnostics, even when
		// no exact session matched. These paths never authorize a fallback.
		for _, root := range roots {
			paths, err := statePaths(root)
			if err != nil {
				return "", err
			}
			if _, err := boundHookCandidates(paths, caller.runtime, caller.session); err != nil {
				return "", err
			}
		}
		return "", nil // unbound sessions are inert, including outside Git
	}
	if o.repoExplicit {
		return workspace(ctx, o.repo)
	}
	if action == "bind" && o.room != "" {
		return roomWorkspace(ctx, *o)
	}
	if len(roots) > 1 {
		return "", errors.New("native project directory and current checkout differ; pass --repo to choose the Room workspace")
	}
	if len(roots) == 1 {
		return roots[0], nil
	}
	return "", errors.New("no relay binding or Git workspace found; pass --repo for the intended Room workspace")
}

func resolveCommandWorkspace(ctx context.Context, action string, o *options) (string, error) {
	// Reject mutually exclusive creation targets before any discovery I/O.
	if action == "bind" && o.create && o.room != "" {
		return "", errors.New("choose --create or --room, not both")
	}
	// Installation is an explicit project setup operation, not session routing.
	if action == "install" {
		return workspace(ctx, o.repo)
	}
	caller, err := currentNativeCaller()
	if err != nil {
		return "", err
	}
	return resolveSessionWorkspace(ctx, action, o, caller)
}

func roomWorkspace(ctx context.Context, o options) (string, error) {
	if !safePart(o.room) {
		return "", errors.New("invalid --room value")
	}
	if o.endpoint == "" {
		var err error
		o.endpoint, err = defaultEndpoint()
		if err != nil {
			return "", err
		}
	}
	endpoint, err := relay.ReadEndpoint(o.endpoint)
	if err != nil {
		return "", err
	}
	var snapshot serviceSnapshot
	if err := management(ctx, endpoint, http.MethodGet, "/api/v1/service", nil, &snapshot); err != nil {
		return "", err
	}
	room, ok := snapshot.findRoom(o.room)
	if !ok || room.HostMode != model.HostNative {
		return "", errors.New("bind requires a native-hosted Room")
	}
	for _, project := range snapshot.Projects {
		if project.ID == room.ProjectID {
			root, err := workspace(ctx, project.Root)
			if err != nil {
				return "", fmt.Errorf("Room workspace is unavailable: %w", err)
			}
			return root, nil
		}
	}
	return "", errors.New("Room project is missing from the Service")
}

// Recheck after taking the slot lock: discovery and lock acquisition can race
// an explicit replacement. Directory discovery never authorizes another session.
func validateCommandCaller(c *Client) error {
	caller, err := currentNativeCaller()
	if err != nil {
		return err
	}
	if caller.session != "" && (caller.session != c.State.SessionID || caller.runtime != c.State.Runtime) {
		return relay.ErrAuth
	}
	return nil
}
