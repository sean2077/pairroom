# Gemini CLI Native integration

[Gemini CLI](https://github.com/google-gemini/gemini-cli) is supported as a **Native-only** participant. This page owns its vendor-specific hook and identity bridge. [Native relay](NATIVE_RELAY.md) still owns the shared workflow, [CLI reference](CLI_REFERENCE.md#native-relay-commands) owns message options, and [Protocol](PROTOCOL.md#native-host-protocol-v8) owns durable delivery.

Gemini can occupy either slot and pair with Claude Code, Codex, Grok Build, or another Gemini session. Handles are `@gemini` for a single Gemini participant and `@gemini0` / `@gemini1` for a duplicate pair. Use the handles returned by bind, not a guessed vendor-to-slot mapping.

## Setup and commands

Install/authenticate Gemini separately and use the same PairRoom build for CLI and Service. In the project worktree, run:

```bash
pairroom relay install --runtime gemini
```

Installation preserves unrelated `.gemini/settings.json` entries and merges two named command hooks: `BeforeTool` (matching only `run_shell_command`) and `AfterAgent`. Both call `pairroom relay hook --runtime gemini`. Their `timeout` is **45000 milliseconds (45 seconds)**; the value `45` would mean only 45 milliseconds in Gemini's schema. Installation does not enable disabled hooks, grant project trust, or change model/provider/permission settings. Review `/hooks panel` and project trust, then restart Gemini so both hooks are loaded.

The bundled skill is installed at `~/.gemini/skills/pairroom-relay/SKILL.md`. `GEMINI_CLI_HOME` overrides the **parent home**: the path becomes `$GEMINI_CLI_HOME/.gemini/skills/pairroom-relay/SKILL.md`, not `$GEMINI_CLI_HOME/skills/...`. Externally managed skill symlinks remain read-only. Use `/skills reload` or restart; ask Gemini to use the `pairroom-relay` skill rather than assuming a skill name is automatically a slash command.

Ask the **Gemini agent** to execute these as direct `run_shell_command` calls:

```bash
pairroom relay preflight
pairroom relay bind --create --name "Review" --peer-runtime codex
```

Pass the printed join command to the peer. To create a Gemini/Gemini pair, use `--peer-runtime gemini`; the generated peer command includes its slot. Existing bindings resume with `pairroom relay bind`. Explicit peer selection is necessary when the Service's default pair contains no Gemini slot; that mismatch is rejected before creation, not silently converted to a different pair.

Use a command beginning with `pairroom relay` (or `pairroom.exe relay` on Windows). Select the working directory with the tool's `dir_path` or PairRoom's `--repo`, not `cd ... &&`, pipelines, wrapper shells, aliases, or an absolute executable path. Keep `pairroom` on PATH. The narrow command shape is intentional: the identity hook does not rewrite arbitrary shell programs. Human shell mode and detached terminals are not an alternative source of native identity.

## Why a BeforeTool bridge is necessary

Gemini documents `GEMINI_CLI=1` in shell tools, but documents `GEMINI_SESSION_ID` in **hook** environments. PairRoom does not assume that a hook-only variable reaches ordinary shell commands, infer identity from cwd/PID, scrape transcripts, or ask the user/model to copy a session ID.

The approved `BeforeTool` hook reads the official `session_id` and rewrites only a direct relay command through Gemini's `hookSpecificOutput.tool_input` interface. It adds a shell-quoted, namespaced identity selector for that child invocation; it does not change approval decisions or original arguments. POSIX assignment and Windows PowerShell quoting are separate. The foreground command requires Gemini's shell marker, rejects conflicting recognized harness ancestry, and retains normal credential/generation/session checks. Prefer separate native sessions over nesting one harness inside another.

Missing bridge metadata fails before `bind --create` provisions a Room. Fix hook installation, approval/loading, and the direct command shape; **never manually export a fabricated session identity**. A confirmed binding follows the session across worktrees using the existing locator and verification rules.

## AfterAgent and delivery limits

Only the official `AfterAgent.prompt_response` is a final response. PairRoom does not substitute the user's prompt, `AfterModel` output, another vendor's `last_assistant_message`, `SessionEnd`, or a transcript. Unbound sessions and unrelated tool events remain inert. Reply routing, private unaddressed replies, publication WAL, sequence numbers, FIFO claims, and uncertain-outcome recovery use the existing Native implementation.

An eligible `AfterAgent` can deliver a queued envelope with `decision: "deny"` and `reason` to request another Gemini turn. It keeps the same bounded hook park and continuation budget as the generic Native receive path. The claim is persisted before stdout; acknowledgement follows a complete write. `handed_off` is not proof that Gemini read or accepted the envelope. Sending explicitly and also addressing the peer in the final answer creates two publications; omit the final peer handle after an intentional `send`.

There is **no Service-initiated Gemini external wake**. Once the hook window/continuation chain ends, input stays queued until the session runs a foreground `wait`/`exchange` or a human resumes it. A background shell process alone does not prove that its output wakes the model. Use foreground collection within the actual Gemini tool lifetime during active discussions.

Embedded adapters, headless process orchestration, CC Switch provider materialization, permission overrides, and a Gemini entry in the Embedded runtime catalog are not part of this integration. An attempted Gemini Embedded factory fails explicitly instead of falling through to Claude. No protocol/schema version or module dependency is changed.

## Diagnostics and validation

`preflight --runtime gemini` checks both hooks and the skill. `doctor`/`status` show whether `AfterAgent` has run for the binding. Installed is not approved: user/system settings and the harness's trust UI remain authoritative. Inspect `hooksConfig.enabled`, named/command entries in `hooksConfig.disabled`, and `/hooks panel`; installation never reenables them.

Tests cover official payload decoding, Unix/PowerShell quoting, inert unrelated events, disabled/missing hooks, millisecond timeouts, install preservation, home-directory semantics, creator-first and duplicate-runtime identities, Embedded rejection, and synthetic CLI/HTTP/journal round trips across four peer runtimes. These tests do not certify an authenticated installed Gemini release. Before declaring vendor E2E verified, record the Gemini version/OS, approve the installed hooks, exercise create/join and bidirectional Unicode replies, observe `last_hook_at`, and verify that a changed/unbound session cannot publish. Do not record credentials or claim external wake from a bounded hook continuation.

Primary interface references, reviewed for this integration:

- [Hook events, input/output schemas and milliseconds](https://geminicli.com/docs/hooks/reference/)
- [Hook installation, environment, consent and management](https://geminicli.com/docs/hooks/)
- [Shell tool and platform shells](https://geminicli.com/docs/tools/shell/)
- [Agent skills](https://geminicli.com/docs/cli/skills/)
- [Configuration, GEMINI_CLI_HOME and hooksConfig](https://geminicli.com/docs/reference/configuration/)
