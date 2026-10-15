// Package projectsetup inspects and installs Native prerequisites in one project.
// It never invokes a harness or the CLI's user-scoped skill installer.
package projectsetup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/sean2077/pairroom/internal/relayclient/skill"
)

var (
	ErrChanged      = errors.New("project setup changed; check again before confirming")
	ErrConfirmation = errors.New("existing project content requires explicit confirmation")
	ErrBlocked      = errors.New("project setup needs manual review")
)

// Mutations are small and serialized across callers, including two project IDs
// referring to the same target. External editors are guarded by revision checks.
var installMu sync.Mutex

var runtimes = []string{"claude", "codex", "grok", "gemini"}

type Item struct {
	Component         string `json:"component"`
	Path              string `json:"path"`
	State             string `json:"state"`
	Reason            string `json:"reason,omitempty"`
	Revision          string `json:"revision"`
	CanInstall        bool   `json:"can_install"`
	NeedsConfirmation bool   `json:"needs_confirmation"`
}

type Runtime struct {
	Runtime string `json:"runtime"`
	Skill   Item   `json:"skill"`
	Hook    Item   `json:"hook"`
}

type Status struct {
	Runtimes []Runtime `json:"runtimes"`
}

// Request selects exactly one component. A failed hook installation cannot
// silently leave a newly installed skill (or vice versa).
type Request struct {
	Runtime        string `json:"runtime"`
	Component      string `json:"component"`
	Revision       string `json:"revision"`
	ConfirmReplace bool   `json:"confirm_replace"`
}

type plan struct {
	item   Item
	before fileState
	after  []byte
}

func validRuntime(kind string) bool {
	for _, value := range runtimes {
		if kind == value {
			return true
		}
	}
	return false
}

// Inspect is read-only: no mkdir, trust changes, global skill reads or subprocesses.
// Current means project files are configured, not approved or loaded in a session.
func Inspect(root string) (Status, error) {
	dir, err := openProject(root)
	if err != nil {
		return Status{}, err
	}
	defer dir.Close()
	result := Status{Runtimes: make([]Runtime, 0, len(runtimes))}
	for _, kind := range runtimes {
		result.Runtimes = append(result.Runtimes, Runtime{
			Runtime: kind, Skill: makePlan(dir, root, kind, "skill").item,
			Hook: makePlan(dir, root, kind, "hook").item,
		})
	}
	return result, nil
}

// Install requires the exact revision returned by Inspect, even for first
// installation. Confirmation authorizes only this component's inspected bytes.
func Install(root string, request Request) (Status, error) {
	if !validRuntime(request.Runtime) || (request.Component != "skill" && request.Component != "hook") {
		return Status{}, errors.New("select a supported runtime and skill or hook")
	}
	installMu.Lock()
	defer installMu.Unlock()
	dir, err := openProject(root)
	if err != nil {
		return Status{}, err
	}
	defer dir.Close()
	p := makePlan(dir, root, request.Runtime, request.Component)
	if request.Revision == "" || request.Revision != p.item.Revision {
		return Status{}, ErrChanged
	}
	if p.item.State == "current" || p.item.State == "shared" {
		return Inspect(root)
	}
	if !p.item.CanInstall {
		return Status{}, fmt.Errorf("%w: %s", ErrBlocked, p.item.Reason)
	}
	if p.item.NeedsConfirmation && !request.ConfirmReplace {
		return Status{}, ErrConfirmation
	}
	if err := replaceFile(dir, p.item.Path, p.before, p.after); err != nil {
		return Status{}, err
	}
	return Inspect(root)
}

func makePlan(dir *os.Root, root, kind, component string) plan {
	path := hookPath(kind)
	if component == "skill" {
		prefix := "." + kind
		if kind == "codex" {
			prefix = ".agents"
		}
		path = prefix + "/skills/pairroom-relay/SKILL.md"
		// Gemini's shared alias has higher precedence than .gemini/skills. Never
		// install a hidden second copy when the alias is already present or unsafe.
		if kind == "gemini" {
			alias := ".agents/skills/pairroom-relay/SKILL.md"
			if protectedReason(root, alias) != "" {
				path = alias
			} else if state, err := readFile(dir, alias); err != nil || state.exists || state.parentExists {
				path = alias
			}
		}
	}
	p := plan{item: Item{Component: component, Path: path, State: "blocked"}}
	if reason := protectedReason(root, path); reason != "" {
		p.item.Reason = reason
		return p
	}
	before, err := readFile(dir, path)
	p.before = before
	if err != nil {
		p.item.Reason = "unsafe_path"
		return p
	}
	p.item.Revision = revision(path, before)
	if component == "skill" {
		p.after = []byte(skill.Content)
		switch {
		case before.exists && bytes.Equal(before.data, p.after):
			p.item.State = "current"
		case before.exists || before.parentExists:
			p.item.State, p.item.Reason = "conflict", "existing_skill"
			p.item.CanInstall, p.item.NeedsConfirmation = true, true
			if bytes.Contains(before.data, []byte("name: pairroom-relay")) && bytes.Contains(before.data, []byte("# pairroom-relay")) {
				p.item.State = "update_required"
			}
		default:
			p.item.State, p.item.CanInstall = "missing", true
		}
		return p
	}
	p.after, p.item.State, p.item.Reason, err = planHooks(before, kind)
	if err != nil {
		p.item.State, p.item.Reason = "blocked", "invalid_hooks"
		return p
	}
	p.item.CanInstall = p.item.State == "missing" || p.item.State == "update_required"
	p.item.NeedsConfirmation = p.item.CanInstall && before.exists
	if (kind == "grok" || kind == "claude") && sharesClaudeHooks() {
		other := "claude"
		if kind == "claude" {
			other = "grok"
		}
		otherPath := hookPath(other)
		blocked := func(reason string) plan {
			// A blocked item cannot be installed, so it never asks for confirmation.
			p.item.State, p.item.Reason, p.item.CanInstall, p.item.NeedsConfirmation = "blocked", reason, false, false
			return p
		}
		if reason := protectedReason(root, otherPath); reason != "" {
			return blocked(reason)
		}
		otherState, readErr := readFile(dir, otherPath)
		p.item.Revision += ":" + revision(otherPath, otherState)
		if readErr != nil {
			return blocked("shared_hook_unreadable")
		}
		_, otherStatus, _, otherErr := planHooks(otherState, other)
		ownPresent, otherPresent := hasRelayCommand(before.data), hasRelayCommand(otherState.data)
		if otherErr != nil {
			return blocked("shared_hook_unreadable")
		}
		if kind == "grok" && otherStatus == "current" && !ownPresent && p.item.State == "missing" {
			p.item.State, p.item.Reason, p.item.CanInstall, p.item.NeedsConfirmation = "shared", "claude_hook", false, false
			p.item.Path = otherPath
			return p
		}
		if otherPresent && (kind == "claude" || ownPresent || otherStatus != "current") {
			if kind == "grok" && !ownPresent {
				// Grok has no hook of its own to resolve here; it would reuse the
				// Claude hook, so the actionable item is the Claude update above.
				return blocked("claude_hook_update")
			}
			return blocked("shared_hook_conflict")
		}
	}
	return p
}

func revision(path string, state fileState) string {
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\x00%t\x00%t\x00%d\x00", path, state.exists, state.parentExists, state.mode)
	sum.Write(state.data)
	return hex.EncodeToString(sum.Sum(nil))
}

func hookPath(kind string) string {
	switch kind {
	case "claude":
		return ".claude/settings.json"
	case "codex":
		return ".codex/hooks.json"
	case "grok":
		return ".grok/hooks/pairroom.json"
	default:
		return ".gemini/settings.json"
	}
}

func sharesClaudeHooks() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GROK_CLAUDE_HOOKS_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func openProject(root string) (*os.Root, error) {
	if !filepath.IsAbs(root) {
		return nil, errors.New("project root must be absolute")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("project root must be a real directory")
	}
	if home, err := os.UserHomeDir(); err == nil {
		if homeInfo, err := os.Stat(home); err == nil && os.SameFile(info, homeInfo) {
			return nil, errors.New("refusing project setup in the user home directory")
		}
	}
	return os.OpenRoot(root)
}

// protectedReason reports why the selected target must not be written: it
// overlaps a user-level configuration directory ("global_path"), or a protection
// boundary could not be resolved at all ("global_unresolved"). An empty result
// means the target is outside every boundary the caller can prove.
func protectedReason(root, relative string) string {
	if canonical, err := filepath.EvalSymlinks(root); err == nil {
		root = canonical
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	var protected []string
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{".agents", ".claude", ".codex", ".grok", ".gemini"} {
			protected = append(protected, filepath.Join(home, name))
		}
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		if value := os.Getenv(name); value != "" {
			protected = append(protected, value)
		}
	}
	if value := os.Getenv("GEMINI_CLI_HOME"); value != "" {
		protected = append(protected, filepath.Join(value, ".gemini"))
	}
	if filepath.Separator == '/' {
		protected = append(protected, "/etc/codex")
	}
	for _, path := range protected {
		absolute, err := canonicalDestination(path)
		if err != nil {
			// An unresolved global boundary is not permission to write through it,
			// and it is reported as its own boundary rather than as an overlap the
			// user could not find.
			return "global_unresolved"
		}
		rel, err := filepath.Rel(absolute, target)
		if err == nil && filepath.IsLocal(rel) {
			return "global_path"
		}
	}
	return ""
}

// canonicalDestination resolves existing ancestors even when the final global
// configuration directory is missing (for example /var -> /private/var on macOS).
// A dangling link, inaccessible ancestor or loop fails closed, rather than
// assuming its lexical path cannot overlap the selected project's target.
func canonicalDestination(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	for ancestor := absolute; ; ancestor = filepath.Dir(ancestor) {
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			tail, err := filepath.Rel(ancestor, absolute)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, tail), nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(ancestor) == ancestor {
			return "", err
		}
		if _, statErr := os.Lstat(ancestor); !errors.Is(statErr, os.ErrNotExist) {
			// EvalSymlinks failed for a present (e.g. dangling symlink) or
			// unreadable entry. Never discard it as if it were a missing name.
			return "", err
		}
	}
}
