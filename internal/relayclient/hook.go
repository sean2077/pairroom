package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

type HookInput struct {
	Event                string  `json:"hook_event_name"`
	SessionID            string  `json:"session_id"`
	CWD                  string  `json:"cwd"`
	TranscriptPath       string  `json:"transcript_path"`
	LastAssistantMessage *string `json:"last_assistant_message"`
	StopHookActive       bool    `json:"stop_hook_active"`
	Error                string  `json:"error"`
}

// statePaths never follows symlinks, including a substituted .pairroom root.
func statePaths(root string) ([]string, error) {
	base := filepath.Join(root, ".pairroom")
	info, err := os.Lstat(base)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("relay workspace state must not be a symlink")
	}
	paths := []string{}
	err = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("relay state contains a symlink; refusing discovery")
		}
		rel, err := filepath.Rel(base, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		if d.IsDir() {
			if len(parts) > 5 {
				return filepath.SkipDir
			}
			return nil
		}
		if len(parts) == 5 && parts[0] == "rooms" && parts[2] == "slots" && parts[4] == "state.json" && safePart(parts[1]) && model.ActorID(parts[3]).ValidParticipant() {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}
func runHook(ctx context.Context, o options, in io.Reader, out, diagnostic io.Writer) error {
	// Keep sender reconciliation outside the park duration. The project hook has
	// 45 seconds; reserve acknowledgement/output time rather than using its edge.
	ctx, cancel := context.WithTimeout(ctx, 42*time.Second)
	defer cancel()
	data, err := io.ReadAll(io.LimitReader(in, (2<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 2<<20 {
		return errors.New("official hook payload exceeds limit")
	}
	var hook HookInput
	if json.Unmarshal(data, &hook) != nil {
		return errors.New("invalid official hook input")
	}
	if hook.Event != "Stop" && hook.Event != "StopFailure" {
		return writeJSON(out, map[string]any{})
	}
	kind := model.RuntimeKind(o.kind)
	if kind != model.RuntimeClaude && kind != model.RuntimeCodex {
		return errors.New("hook requires --runtime claude|codex")
	}
	if hook.SessionID == "" || hook.CWD == "" {
		return errors.New("official hook session_id and cwd are required; no transcript fallback")
	}
	root, err := workspace(ctx, hook.CWD)
	if err != nil {
		return err
	}
	paths, err := statePaths(root)
	if err != nil {
		return err
	}
	candidates := []string{}
	for _, path := range paths {
		var s State
		if err := readPrivate(path, &s); err != nil {
			return err
		}
		if s.Runtime != kind {
			continue
		}
		if s.SessionID == hook.SessionID || (s.SessionID == "" && s.Nonce != "" && hook.LastAssistantMessage != nil && strings.Contains(*hook.LastAssistantMessage, s.Nonce)) {
			candidates = append(candidates, filepath.Dir(path))
			continue
		}
		// Recover association acknowledged by the Service before a local crash. An
		// unrelated session receives only a pending binding, never inbox contents.
		if s.SessionID == "" && s.Generation > 0 {
			c, err := load(filepath.Dir(path))
			if err != nil {
				continue
			}
			c.State.SessionID = hook.SessionID
			var b relay.Binding
			if c.call(ctx, "inspect", nil, &b) == nil && b.SessionID == hook.SessionID {
				candidates = append(candidates, c.Dir)
			}
		}
	}
	if len(candidates) == 0 {
		return writeJSON(out, map[string]any{})
	}
	if len(candidates) != 1 {
		return errors.New("official session matches multiple local bindings; refusing ambiguous relay")
	}
	dir := candidates[0]
	release, err := lockSlot(ctx, dir)
	if err != nil {
		return err
	}
	c, err := load(dir)
	if err != nil {
		release()
		return err
	}
	cleanupAtomicTemps(dir)
	if c.State.SessionID != "" && c.State.SessionID != hook.SessionID {
		release()
		return relay.ErrAuth
	}
	if c.State.Generation == 0 {
		release()
		return errors.New("bind confirmation missing; run bind again before association")
	}
	c.State.SessionID = hook.SessionID
	var binding relay.Binding
	if err := c.call(ctx, "inspect", nil, &binding); err != nil {
		release()
		return err
	}
	if binding.SessionID == "" {
		if c.State.Nonce == "" || hook.LastAssistantMessage == nil || !strings.Contains(*hook.LastAssistantMessage, c.State.Nonce) {
			release()
			return writeJSON(out, map[string]any{})
		}
		if err := c.call(ctx, "associate", map[string]any{"nonce": c.State.Nonce, "session_id": hook.SessionID, "transcript_path": hook.TranscriptPath}, &binding); err != nil {
			release()
			return err
		}
	}
	if binding.SessionID != hook.SessionID {
		release()
		return relay.ErrAuth
	}
	next := c.State
	next.SessionID = hook.SessionID
	next.Nonce = ""
	if !hook.StopHookActive {
		next.Blocks = 0
	}
	if err := c.persist(next); err != nil {
		release()
		return err
	}
	if hook.Event == "StopFailure" {
		// A vendor error may contain tokens or partial reply text. Send only the
		// allowlisted observation; StopFailure cannot request continuation.
		err = c.call(ctx, "failure", map[string]string{"error": "api_error"}, nil)
		release()
		if err != nil {
			return err
		}
		return writeJSON(out, map[string]any{})
	}
	if hook.LastAssistantMessage == nil {
		release()
		return errors.New("Stop payload has no last_assistant_message; no transcript parsing or fake publication")
	}
	err = c.Publish(ctx, *hook.LastAssistantMessage)
	release()
	if err != nil {
		_, _ = fmt.Fprintln(diagnostic, "PairRoom: publication pending or unknown; inspect relay status. No new-ID replay was attempted.")
		return err
	}
	if hook.StopHookActive && c.State.Blocks >= relay.MaxBlocks {
		return writeJSON(out, map[string]any{})
	}
	return deliver(ctx, c, true, 30, out)
}

// deliver makes a single claim and writes one complete stdout record before ack.
// A failed/short write, killed CLI or missing acknowledgement becomes unknown in
// the Service. The hook's block count changes only for actual inbox messages.
func deliver(ctx context.Context, c *Client, hook bool, seconds int, out io.Writer) error {
	if seconds < 1 || seconds > 30 {
		return errors.New("wait timeout must be 1–30 seconds")
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline) - 4*time.Second
		if remaining < time.Second {
			if hook {
				return writeJSON(out, map[string]any{})
			}
			return context.DeadlineExceeded
		}
		if time.Duration(seconds)*time.Second > remaining {
			seconds = int(remaining / time.Second)
		}
	}
	var result struct {
		Claim *relay.Claim `json:"claim"`
	}
	if err := c.call(ctx, "wait", map[string]any{"park": hook, "timeout_seconds": seconds}, &result); err != nil {
		return err
	}
	if result.Claim == nil {
		if hook {
			return writeJSON(out, map[string]any{})
		}
		return nil
	}
	claim := result.Claim
	if claim.ID == "" || claim.Receipt == "" || claim.Envelope == "" {
		return errors.New("incomplete delivery claim; acknowledgement withheld")
	}
	if hook {
		release, err := lockSlot(ctx, c.Dir)
		if err != nil {
			return err
		}
		current, err := load(c.Dir)
		if err == nil && (current.State.BindID != c.State.BindID || current.State.Generation != c.State.Generation) {
			err = relay.ErrAuth
		}
		if err == nil {
			next := current.State
			next.Blocks++
			err = current.persist(next)
		}
		release()
		if err != nil {
			return err
		}
		if err := writeJSON(out, map[string]string{"decision": "block", "reason": claim.Envelope}); err != nil {
			return err
		}
	} else {
		bytes := []byte(claim.Envelope + "\n")
		n, err := out.Write(bytes)
		if err == nil && n != len(bytes) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return err
		}
	}
	if err := c.call(ctx, "ack", map[string]string{"id": claim.ID, "receipt": claim.Receipt}, nil); err != nil {
		return errors.New("stdout written but acknowledgement unavailable; inspect Room delivery state, never automatically replay")
	}
	return nil
}
