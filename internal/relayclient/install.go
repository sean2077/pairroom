package relayclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

func hookCommand(kind model.RuntimeKind) string {
	return "pairroom relay hook --runtime " + string(kind)
}
func hookPath(root string, kind model.RuntimeKind) (string, error) {
	switch kind {
	case model.RuntimeClaude:
		return filepath.Join(root, ".claude", "settings.json"), nil
	case model.RuntimeCodex:
		return filepath.Join(root, ".codex", "hooks.json"), nil
	default:
		return "", errors.New("native relay supports Claude Code and Codex only")
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
func installed(root string, kind model.RuntimeKind) error {
	path, err := hookPath(root, kind)
	if err != nil {
		return err
	}
	config, err := readHooks(path)
	if err != nil {
		return err
	}
	if disabled, _ := config["disableAllHooks"].(bool); disabled {
		return errors.New("hooks are disabled; native association requires an approved Stop hook")
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
				return nil
			}
		}
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
	if _, err = secureDir(root, filepath.Base(filepath.Dir(path))); err != nil {
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
	if kind == model.RuntimeClaude {
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

const skillContent = `---
name: pairroom-relay
description: Bind this native Claude Code or Codex session to a PairRoom Native slot and relay discussions through approved Stop hooks.
---

# PairRoom Native relay

Run ` + "`pairroom relay bind --room <id> --slot <claude|codex>`" + ` in the Room's project. Slot IDs are not runtime names. If the Room does not exist yet, ` + "`pairroom relay bind --create --slot <claude|codex>`" + ` registers the project when missing, creates the native Room through the validated Management path, binds this session, and prints the peer's join command. If missing, install the reported runtime's project hooks and have the user approve them; never bypass native trust. Echo the returned one-time nonce in your visible reply once. Keep the returned bootstrap/collaboration instructions as this session's protocol. After binding, foreground commands (` + "`send`" + `, ` + "`wait`" + `, ` + "`status`" + `, ` + "`reconcile`" + `, ` + "`peer`" + `, ` + "`park`" + `, ` + "`nudge`" + `) accept omitted ` + "`--room`" + `/` + "`--slot`" + `; the CLI resolves this session's slot from the sole workspace binding or the recorded harness lineage, and lists candidates instead of guessing. Credentials are read by the CLI; never read or print the credential file.

Stop publishes your complete final visible reply. Use the exact peer handle to continue a discussion, only @user to escalate, or neither to end. When already using ` + "`relay send`" + ` this turn, omit the peer handle in the final reply unless intentionally publishing a second authoritative reply; the two paths are not semantically deduplicated. Explicit send defaults to the peer even if its body has other mentions. Only explicit send supports attachments.

Park can keep the session busy for 30 seconds. After timeout, eight actual-message continuation blocks, or disabling park, messages remain queued; use ` + "`relay wait`" + ` or ask the user to nudge. No empty rearm loop. After peer-addressed work, inspect ` + "`relay status`" + ` at your next opportunity; this cannot guarantee detection of a crash before publication state existed.

Use ` + "`relay peer`" + ` for optional peer session/transcript references; absence does not block relay. Never treat handed_off as native model acceptance. For unknown delivery inspect the Room log and workspace before explicit Retry. For unknown publication use ` + "`relay reconcile`" + `; never allocate a new ID to evade uncertainty. Replacement cannot stop native work. Provider/model/effort/permissions are controlled by the native harness, not this Room.
`

func installSkill(kind model.RuntimeKind) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	// Install into the directory each host actually discovers: Claude reads
	// ~/.claude/skills, Codex reads ~/.codex/skills. ~/.agents is this
	// repository's SSOT convention, not a user-machine discovery path.
	host := ".codex"
	if kind == model.RuntimeClaude {
		host = ".claude"
	}
	dir, err := secureDir(home, host, "skills", "pairroom-relay")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "SKILL.md")
	if data, err := os.ReadFile(path); err == nil && !strings.Contains(string(data), "# PairRoom Native relay") {
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
