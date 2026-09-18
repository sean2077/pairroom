package relayclient

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

// installRuntimeChoices is the interactive selection order. Each is a real native
// runtime with its own project hook location and skill directory.
var installRuntimeChoices = []struct {
	kind  model.RuntimeKind
	label string
}{
	{model.RuntimeCodex, "Codex"},
	{model.RuntimeClaude, "Claude Code (cc)"},
	{model.RuntimeGrok, "Grok Build"},
}

func parseRuntimeToken(token string) (model.RuntimeKind, bool) {
	switch strings.ToLower(strings.TrimSpace(token)) {
	case "claude", "cc", "claude-code", "claudecode":
		return model.RuntimeClaude, true
	case "codex":
		return model.RuntimeCodex, true
	case "grok", "grok-build", "grokbuild":
		return model.RuntimeGrok, true
	}
	return "", false
}

// parseRuntimeList parses a comma/space-separated --runtime value into
// deduplicated canonical runtimes, preserving first-seen order.
func parseRuntimeList(value string) ([]model.RuntimeKind, error) {
	fields := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	var kinds []model.RuntimeKind
	seen := map[model.RuntimeKind]bool{}
	for _, field := range fields {
		kind, ok := parseRuntimeToken(field)
		if !ok {
			return nil, fmt.Errorf("unknown --runtime %q; choose claude (cc), codex, or grok", field)
		}
		if !seen[kind] {
			seen[kind] = true
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) == 0 {
		return nil, errors.New("no runtime selected")
	}
	return kinds, nil
}

// stdinIsTTY reports whether in is an interactive terminal, using only the
// standard library (the dependency invariant forbids golang.org/x/term).
func stdinIsTTY(in io.Reader) bool {
	file, ok := in.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func readLine(in io.Reader) (string, error) {
	line, err := bufio.NewReader(in).ReadString('\n')
	trimmed := strings.TrimRight(line, "\r\n")
	if trimmed == "" && err != nil {
		return "", err
	}
	return trimmed, nil
}

// promptInstallRuntimes offers a multi-select to a human at a terminal. The menu
// goes to diagnostic so the JSON result on stdout stays parseable.
func promptInstallRuntimes(in io.Reader, diagnostic io.Writer) ([]model.RuntimeKind, error) {
	fmt.Fprintln(diagnostic, "Install PairRoom relay hooks for which harnesses? Enter numbers or names, comma-separated (empty cancels):")
	for i, choice := range installRuntimeChoices {
		fmt.Fprintf(diagnostic, "  %d) %s\n", i+1, choice.label)
	}
	fmt.Fprint(diagnostic, "Select: ")
	line, err := readLine(in)
	if err != nil {
		return nil, errors.New("could not read the selection; rerun pairroom relay install or pass --runtime claude|codex|grok")
	}
	if strings.TrimSpace(line) == "" {
		return nil, errors.New("no harness selected; rerun pairroom relay install or pass --runtime claude|codex|grok")
	}
	var resolved []string
	for _, token := range strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if n, convErr := strconv.Atoi(token); convErr == nil && n >= 1 && n <= len(installRuntimeChoices) {
			resolved = append(resolved, string(installRuntimeChoices[n-1].kind))
			continue
		}
		resolved = append(resolved, token)
	}
	return parseRuntimeList(strings.Join(resolved, ","))
}

// selectInstallRuntimes resolves which harnesses to install for: an explicit
// --runtime list wins; inside a recognized native session the harness is
// inferred; at an interactive terminal the user is prompted; otherwise the
// command fails with the valid options instead of hanging an agent tool call.
func selectInstallRuntimes(flagValue string, in io.Reader, diagnostic io.Writer) ([]model.RuntimeKind, error) {
	if strings.TrimSpace(flagValue) != "" {
		return parseRuntimeList(flagValue)
	}
	if _, name, ok := harnessAncestor(); ok {
		if kind, isHarness := harnessRuntimes[name]; isHarness {
			return []model.RuntimeKind{kind}, nil
		}
	}
	if stdinIsTTY(in) {
		return promptInstallRuntimes(in, diagnostic)
	}
	return nil, errors.New("install needs --runtime claude|codex|grok (comma-separated) when run non-interactively outside a native session; inside a terminal it prompts, and inside a recognized session it infers the harness")
}

const grokSharesClaudeHooksNotice = "Grok Build reuses Claude Code project hooks by default, so PairRoom does not write a second Grok hook file when a Claude Code PairRoom Stop hook is present or being installed, and it removes PairRoom's extra Grok Stop hook if that leftover file is still there. Review the Claude Code project hook; Grok's /hooks (press r to reload) still shows that reused definition, and folder trust remains a human decision. If Claude-hook compatibility is disabled ([compat.claude] hooks = false or GROK_CLAUDE_HOOKS_ENABLED=false), install Grok with that compatibility off so PairRoom writes .grok/hooks/pairroom.json."

// grokReusesClaudeHooks reports Grok Build's default Claude-hook compatibility.
// Only the documented env override is consulted; config.toml is left to the harness.
func grokReusesClaudeHooks() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GROK_CLAUDE_HOOKS_ENABLED"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

// runInstall writes the relay skill for each selected harness and the project
// hook files that harness actually needs. Grok Build's default Claude Code
// compatibility layer already runs `.claude/settings.json` Stop hooks, so a
// second `.grok/hooks/pairroom.json` would double-fire; skip writing it and
// strip a leftover PairRoom Grok Stop hook when a Claude Code PairRoom Stop
// hook is present or selected in this same install.
func runInstall(root string, kinds []model.RuntimeKind, _ io.Reader, out, _ io.Writer) error {
	seen := map[model.RuntimeKind]bool{}
	var selected []model.RuntimeKind
	want := map[model.RuntimeKind]bool{}
	for _, kind := range kinds {
		if seen[kind] {
			continue
		}
		seen[kind] = true
		want[kind] = true
		selected = append(selected, kind)
	}
	claudePresent, _, err := ownRelayStopHook(root, model.RuntimeClaude)
	if err != nil {
		return err
	}
	shareClaudeHooks := grokReusesClaudeHooks() && (want[model.RuntimeClaude] || claudePresent)
	skipGrokHooks := want[model.RuntimeGrok] && shareClaudeHooks
	installed := []string{}
	var skipped []string
	for _, kind := range selected {
		if kind == model.RuntimeGrok && skipGrokHooks {
			skipped = append(skipped, string(kind))
		} else if err := editHooks(root, kind, false); err != nil {
			return err
		}
		if err := installSkill(kind); err != nil {
			return err
		}
		installed = append(installed, string(kind))
	}
	var removed []string
	if shareClaudeHooks {
		grokPresent, _, err := ownRelayStopHook(root, model.RuntimeGrok)
		if err != nil {
			return err
		}
		if grokPresent {
			if err := editHooks(root, model.RuntimeGrok, true); err != nil {
				return err
			}
			removed = append(removed, string(model.RuntimeGrok))
		}
	}
	notice := "Review and approve the exact project hook in each harness (Codex: /hooks; Grok: /hooks, press r to reload, then review project folder trust; Claude Code: project hook consent). This command does not grant native trust. Keep pairroom on PATH. Real authenticated bidirectional E2E remains release-gated."
	if len(skipped) > 0 || len(removed) > 0 {
		notice = grokSharesClaudeHooksNotice + " " + notice
	}
	result := map[string]any{
		"installed": installed,
		"notice":    notice,
		"next_steps": []string{
			"Create a room and bind this session: pairroom relay bind --create --name \"<topic>\" (skill: /pairroom-relay <topic>)",
			"The peer session joins with the printed peer_join command, or zero-flag inside a recognized session: pairroom relay bind",
			"After both sessions bind, give the agents the task and desired collaboration",
		},
	}
	if len(skipped) > 0 {
		result["hooks_skipped"] = skipped
	}
	if len(removed) > 0 {
		result["hooks_removed"] = removed
	}
	return writeJSON(out, result)
}

func hookCommand(kind model.RuntimeKind) string {
	return "pairroom relay hook --runtime " + string(kind)
}
func hookPath(root string, kind model.RuntimeKind) (string, error) {
	switch kind {
	case model.RuntimeClaude:
		return filepath.Join(root, ".claude", "settings.json"), nil
	case model.RuntimeCodex:
		return filepath.Join(root, ".codex", "hooks.json"), nil
	case model.RuntimeGrok:
		return filepath.Join(root, ".grok", "hooks", "pairroom.json"), nil
	default:
		return "", errors.New("native relay supports Claude Code, Codex and Grok Build")
	}
}
func readHooks(path string) (map[string]any, error) {
	value := map[string]any{}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return value, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 2<<20 {
		return nil, errors.New("hook configuration must be a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(data, &value); err != nil || value == nil {
		return nil, errors.New("invalid existing hook configuration; refusing to overwrite")
	}
	return value, nil
}
func ownRelayStopHook(root string, kind model.RuntimeKind) (present, disabled bool, err error) {
	path, err := hookPath(root, kind)
	if err != nil {
		return false, false, err
	}
	config, err := readHooks(path)
	if err != nil {
		return false, false, err
	}
	if disabled, _ := config["disableAllHooks"].(bool); disabled {
		return false, true, nil
	}
	hooks, _ := config["hooks"].(map[string]any)
	groups, _ := hooks["Stop"].([]any)
	for _, raw := range groups {
		group, _ := raw.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			async, _ := entry["async"].(bool)
			timeout, _ := entry["timeout"].(float64)
			if entry["type"] == "command" && entry["command"] == hookCommand(kind) && !async && timeout >= 45 {
				return true, false, nil
			}
		}
	}
	return false, false, nil
}

func installed(root string, kind model.RuntimeKind) error {
	present, disabled, err := ownRelayStopHook(root, kind)
	if err != nil {
		return err
	}
	if disabled {
		return errors.New("hooks are disabled; native association requires an approved Stop hook")
	}
	if present {
		return nil
	}
	if kind == model.RuntimeGrok && grokReusesClaudeHooks() {
		present, disabled, err = ownRelayStopHook(root, model.RuntimeClaude)
		if err != nil {
			return err
		}
		if disabled {
			return errors.New("hooks are disabled; native association requires an approved Stop hook")
		}
		if present {
			return nil
		}
		return errors.New("zero approved relay-hook setup is unsupported: run pairroom relay install --runtime grok, or install --runtime claude so Grok can reuse that project hook, review the project hooks in your harness, then bind again")
	}
	return fmt.Errorf("zero approved relay-hook setup is unsupported: run pairroom relay install --runtime %s, review the project hooks in your harness, then bind again", kind)
}

// editHooks changes only exact PairRoom hook commands, preserving other hooks
// and project settings. Approval remains owned by the native harness.
func editHooks(root string, kind model.RuntimeKind, remove bool) error {
	path, err := hookPath(root, kind)
	if err != nil {
		return err
	}
	relDir, err := filepath.Rel(root, filepath.Dir(path))
	if err != nil {
		return err
	}
	if _, err = secureDir(root, strings.Split(relDir, string(filepath.Separator))...); err != nil {
		return err
	}
	config, err := readHooks(path)
	if err != nil {
		return err
	}
	hooks, ok := config["hooks"].(map[string]any)
	if !ok {
		if config["hooks"] != nil {
			return errors.New("invalid hooks map")
		}
		hooks = map[string]any{}
	}
	events := []string{"Stop"}
	if kind == model.RuntimeClaude || kind == model.RuntimeGrok {
		events = append(events, "StopFailure")
	}
	for _, event := range events {
		groups, ok := hooks[event].([]any)
		if !ok && hooks[event] != nil {
			return errors.New("invalid hook event groups")
		}
		kept := []any{}
		for _, raw := range groups {
			group, ok := raw.(map[string]any)
			if !ok {
				return errors.New("invalid hook group")
			}
			entries, ok := group["hooks"].([]any)
			if !ok {
				return errors.New("invalid hook entries")
			}
			rest := []any{}
			for _, rawEntry := range entries {
				entry, _ := rawEntry.(map[string]any)
				if entry["command"] != hookCommand(kind) {
					rest = append(rest, rawEntry)
				}
			}
			if len(rest) > 0 {
				group["hooks"] = rest
				kept = append(kept, group)
			}
		}
		if !remove {
			kept = append(kept, map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hookCommand(kind), "timeout": 45}}})
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	config["hooks"] = hooks
	return relay.AtomicJSON(path, config)
}

// The canonical public skill lives at skills/pairroom-relay/SKILL.md,
// distributable through skill installers (npx skills, plugin manifest). This
// projection is embedded for `relay install`; a freshness test keeps the two
// byte-identical.
//
//go:embed skill/pairroom-relay/SKILL.md
var skillContent string

// skillHeadings are the headings PairRoom's own projections have shipped. A
// projection installed by an earlier `relay install` must stay upgradable; only
// genuinely unrelated content at that path is refused.
var skillHeadings = []string{"# pairroom-relay", "# PairRoom Native relay", "# PairRoom relay"}

func ownedSkill(data []byte) bool {
	for _, heading := range skillHeadings {
		if bytes.Contains(data, []byte(heading)) {
			return true
		}
	}
	return false
}

func installSkill(kind model.RuntimeKind) error {
	// Product skill discovery follows the selected host, not .agents/ SSOT.
	homeVar := map[model.RuntimeKind]string{
		model.RuntimeClaude: "CLAUDE_CONFIG_DIR",
		model.RuntimeCodex:  "CODEX_HOME",
		model.RuntimeGrok:   "GROK_HOME",
	}[kind]
	if homeVar == "" {
		return errors.New("unsupported native skill runtime")
	}
	home := os.Getenv(homeVar)
	parts := []string{"skills", "pairroom-relay"}
	var err error
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil {
			return err
		}
		parts = append([]string{"." + string(kind)}, parts...)
	} else {
		home, err = filepath.Abs(home)
		if err != nil {
			return err
		}
		if err = os.MkdirAll(home, 0700); err != nil {
			return err
		}
		info, err := os.Lstat(home)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must be a directory, not a symlink", homeVar)
		}
	}
	// The host/skills parents must be real directories PairRoom creates itself;
	// keep them on the strict fail-closed path. Only the leaf skill directory is
	// allowed to be a symlink, because external skill installers (cc-switch,
	// `npx skills`) legitimately symlink a managed skill into place.
	parent, err := secureDir(home, parts[:len(parts)-1]...)
	if err != nil {
		return err
	}
	return installSkillLeaf(parent, parts[len(parts)-1])
}

// installSkillLeaf installs the bundled pairroom-relay skill into parent/name.
//
// A real directory is created if needed and written. A leaf that an external
// skill installer already symlinked in is recognized as installed only when it
// resolves to a directory holding PairRoom-owned content; PairRoom never writes
// through that symlink into the installer's store. Dangling links, links that
// resolve to a non-directory, and unrelated content all fail closed.
func installSkillLeaf(parent, name string) error {
	dir := filepath.Join(parent, name)
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.Mkdir(dir, 0700); err != nil {
			return err
		}
		return writeSkillFile(dir)
	case err != nil:
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return fmt.Errorf("pairroom-relay skill at %s is a broken symlink; repair it with your skill installer", dir)
		}
		target, err := os.Stat(resolved)
		if err != nil || !target.IsDir() {
			return fmt.Errorf("pairroom-relay skill at %s is a symlink that does not resolve to a directory", dir)
		}
		// Externally managed: accept an existing PairRoom-owned skill as already
		// installed, but never write through the symlink.
		data, err := os.ReadFile(filepath.Join(resolved, "SKILL.md"))
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("pairroom-relay skill at %s is a symlink with no SKILL.md; install it with your skill installer", dir)
		}
		if err != nil {
			return err
		}
		if !ownedSkill(data) {
			return errors.New("an unrelated pairroom-relay skill exists; refusing to overwrite")
		}
		return nil
	}
	if !info.IsDir() {
		return fmt.Errorf("pairroom-relay skill path %s must be a directory", dir)
	}
	return writeSkillFile(dir)
}

// writeSkillFile upgrades the bundled skill into a real PairRoom-owned
// directory, refusing to clobber genuinely unrelated content.
func writeSkillFile(dir string) error {
	path := filepath.Join(dir, "SKILL.md")
	if data, err := os.ReadFile(path); err == nil && !ownedSkill(data) {
		return errors.New("an unrelated pairroom-relay skill exists; refusing to overwrite")
	}
	return atomicText(path, skillContent, 0600)
}
func atomicText(path, text string, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("refusing to replace non-regular file")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pairroom-config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(mode); err == nil {
		_, err = tmp.WriteString(text)
	}
	if err == nil {
		err = tmp.Sync()
	}
	closeErr := tmp.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp.Name(), path)
}
func ignoreWorkspace(root string) error {
	path := filepath.Join(root, ".gitignore")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New(".gitignore must be a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == ".pairroom/" || strings.TrimSpace(line) == "/.pairroom/" {
			return nil
		}
	}
	text := string(data)
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	text += ".pairroom/\n"
	return atomicText(path, text, 0644)
}
