package relayclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sean2077/pairroom/internal/model"
	"github.com/sean2077/pairroom/internal/relay"
)

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
	kind := model.RuntimeKind(o.kind)
	if kind != model.RuntimeClaude && kind != model.RuntimeCodex && kind != model.RuntimeGrok {
		return errors.New("hook requires --runtime claude|codex|grok")
	}
	requested := kind
	hook, err := decodeNativeHook(data, kind == model.RuntimeGrok)
	if err != nil {
		return err
	}
	sharedGrok := false
	if requested == model.RuntimeClaude && hook.Event == "" {
		if grokHook, grokErr := decodeNativeHook(data, true); grokErr == nil && (grokHook.Event == "Stop" || grokHook.Event == "StopFailure") {
			hook = grokHook
			kind = model.RuntimeGrok
			sharedGrok = true
		}
	}
	if hook.Event != "Stop" && hook.Event != "StopFailure" {
		return writeJSON(out, map[string]any{})
	}
	if hook.SessionID == "" || hook.CWD == "" {
		return errors.New("official hook session_id and cwd are required; no transcript fallback")
	}
	root, err := workspace(ctx, hook.CWD)
	if err != nil {
		return err
	}
	if sharedGrok {
		present, _, err := ownRelayStopHook(root, model.RuntimeGrok)
		if err != nil {
			return err
		}
		if present {
			// Grok's own hook file will handle this Stop; stay inert to avoid double publish.
			return writeJSON(out, map[string]any{})
		}
	}
	paths, err := statePaths(root)
	if err != nil {
		return err
	}
	candidates, err := boundHookCandidates(paths, kind, hook.SessionID)
	if err != nil {
		return err
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
	// The binding was associated at bind from the harness environment, so the
	// official hook session must equal the recorded one. A mismatch fails closed
	// without re-associating the session.
	if c.State.SessionID != hook.SessionID {
		release()
		return relay.ErrAuth
	}
	if c.State.Generation == 0 {
		release()
		return errors.New("bind confirmation missing; use bind --replace explicitly")
	}
	// Stamp hook activity before any HTTP so even a failed inspect/confirm
	// leaves the locally observable last-hook marker fresh on the next
	// persist (the reservation and Blocks writes both carry it forward).
	c.State.LastHookAt = time.Now().UTC().Format(time.RFC3339)
	// Reserve the reply WAL before any metadata HTTP for this invocation: a
	// transient inspect/confirm failure then leaves a reconcilable pending
	// publication instead of losing this Stop reply without a trace. Only a
	// clean pending slot qualifies; an unresolved earlier publication keeps
	// the ordinary Publish path, which reconciles it first.
	reserved := hook.Event == "Stop" && hook.LastAssistantMessage != nil && !hook.Clipped &&
		len(*hook.LastAssistantMessage) <= relay.MaxBodyBytes && c.State.Pending == nil
	if reserved {
		if err := c.ReservePublication(*hook.LastAssistantMessage); err != nil {
			release()
			return err
		}
	}
	var binding relay.Binding
	if err := c.call(ctx, "inspect", nil, &binding); err != nil {
		release()
		return err
	}
	if binding.SessionID != hook.SessionID {
		release()
		return relay.ErrAuth
	}
	if err := captureClaudeInbox(c.Dir, c.State); err != nil {
		_, _ = fmt.Fprintln(diagnostic, "PairRoom: Claude external wake is unavailable; relay publication and collection remain available.")
	}
	// Opportunistically record the transcript path, which the harness environment
	// does not carry, so `relay peer` references stay available.
	if binding.TranscriptPath == "" && hook.TranscriptPath != "" {
		if err := c.call(ctx, "confirm", map[string]any{"session_id": hook.SessionID, "transcript_path": hook.TranscriptPath}, &binding); err != nil {
			release()
			return err
		}
	}
	next := c.State
	if !hook.StopHookActive {
		next.Blocks = 0
	}
	// Local-only observability for `relay status`; never sent to the Service.
	next.LastHookAt = time.Now().UTC().Format(time.RFC3339)
	if err := c.persist(next); err != nil {
		release()
		return err
	}
	if hook.Event == "StopFailure" {
		// A vendor error may contain tokens or partial reply text. Send only the
		// allowlisted observation; StopFailure cannot request continuation.
		category := "api_error"
		if kind == model.RuntimeGrok {
			category = hook.Error
		}
		err = c.call(ctx, "failure", map[string]string{"error": category}, nil)
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
	if hook.Clipped {
		// Never publish Grok's truncated prefix as a full response. Older pending
		// publication keeps its identity and is reconciled before recovery.
		err = c.Reconcile(ctx, false)
		release()
		if err != nil {
			return err
		}
		return grokContinuation(ctx, c, grokClippedReplyNotice, out)
	}
	if reserved {
		err = c.PublishReserved(ctx)
	} else {
		err = c.Publish(ctx, *hook.LastAssistantMessage)
	}
	release()
	if err != nil {
		// Spec §6 decoupling: a failed or uncertain publication retains its
		// pending state for the next hook's reconciliation and must not
		// suppress this hook's receive-side park. The diagnostic goes to
		// stderr; the stdout decision JSON and exit code stay intact so the
		// harness still honors any block below.
		_, _ = fmt.Fprintln(diagnostic, "PairRoom: publication pending or unknown; inspect relay status. No new-ID replay was attempted.")
	}
	if hook.StopHookActive && c.State.Blocks >= hookBlockLimit(kind) {
		return writeJSON(out, map[string]any{})
	}
	// Publishing remains independent of collection. A foreground tool may be
	// waiting through the native Stop boundary; never steal its next input.
	releaseCollector, err := acquireCollector(ctx, c.Dir)
	if errors.Is(err, errCollectorBusy) {
		return writeJSON(out, map[string]any{})
	}
	if err != nil {
		return err
	}
	defer releaseCollector()
	return deliver(ctx, c, true, 30, out)
}

// deliver makes a single claim and writes one complete stdout record before ack.
// A failed/short write, killed CLI or missing acknowledgement becomes unknown in
// the Service. The hook's block count changes only for actual inbox messages.
func deliver(ctx context.Context, c *Client, hook bool, seconds int, out io.Writer) error {
	_, err := deliverOnce(ctx, c, hook, seconds, out)
	return err
}

// deliverOnce reports whether one envelope was fully written and acknowledged.
// An empty successful poll can be renewed; every error must stop collection.
func deliverOnce(ctx context.Context, c *Client, hook bool, seconds int, out io.Writer) (bool, error) {
	if seconds < 1 || seconds > 30 {
		return false, errors.New("wait timeout must be 1–30 seconds")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Only Stop hooks need to reserve time inside their lifecycle deadline.
	// Foreground tools keep the caller context, including sub-second budgets,
	// so an already-queued envelope can still be collected and acknowledged.
	if deadline, ok := ctx.Deadline(); hook && ok {
		remaining := time.Until(deadline) - 4*time.Second
		if remaining < time.Second {
			return false, writeJSON(out, map[string]any{})
		}
		if time.Duration(seconds)*time.Second > remaining {
			seconds = int(remaining / time.Second)
		}
	}
	var result struct {
		Claim              json.RawMessage `json:"claim"`
		ForegroundRequired bool            `json:"foreground_required"`
	}
	if err := c.call(ctx, "wait", map[string]any{"park": hook, "timeout_seconds": seconds}, &result); err != nil {
		return false, err
	}
	if len(result.Claim) == 0 {
		return false, errors.New("wait response missing claim; outcome uncertain, collection stopped")
	}
	if result.ForegroundRequired {
		if !hook || c.State.Runtime != model.RuntimeGrok || strings.TrimSpace(string(result.Claim)) != "null" {
			return false, errors.New("invalid foreground-collection notice; acknowledgement withheld")
		}
		// This is readiness, not delivery. No receipt or inbox content has
		// left the Service. The native tool will collect the full envelope.
		return false, grokContinuation(ctx, c, "PairRoom has pending input. In the bound Room workspace, run pairroom relay wait and process its full result. This notice contains no peer reply.", out)
	}
	if strings.TrimSpace(string(result.Claim)) == "null" {
		if hook {
			return false, writeJSON(out, map[string]any{})
		}
		return false, nil
	}
	if hook && c.State.Runtime == model.RuntimeGrok {
		return false, errors.New("Grok hook received a claim instead of readiness; update CLI and Service together; acknowledgement withheld")
	}
	var claim relay.Claim
	if err := json.Unmarshal(result.Claim, &claim); err != nil {
		return false, errors.New("invalid delivery claim; acknowledgement withheld")
	}
	if claim.ID == "" || claim.Receipt == "" || claim.Envelope == "" {
		return false, errors.New("incomplete delivery claim; acknowledgement withheld")
	}
	if hook {
		release, err := lockSlot(ctx, c.Dir)
		if err != nil {
			return false, err
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
			return false, err
		}
		if err := writeJSON(out, map[string]string{"decision": "block", "reason": claim.Envelope}); err != nil {
			return false, err
		}
	} else {
		bytes := []byte(claim.Envelope + "\n")
		n, err := out.Write(bytes)
		if err == nil && n != len(bytes) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return false, err
		}
	}
	if err := c.call(ctx, "ack", map[string]string{"id": claim.ID, "receipt": claim.Receipt}, nil); err != nil {
		return false, errors.New("stdout written but acknowledgement unavailable; inspect Room delivery state, never automatically replay")
	}
	return true, nil
}
