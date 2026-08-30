# Flexible workflows and provider profiles

PairRoom keeps Claude Code and Codex as the native harnesses. This layer adds
only the coordination that is missing when two independent terminal
processes are placed in one shared room.

## Natural-language workflow compilation

Addressed actor/action pairs are compiled in the order written by the user.
Both English and Chinese action names are accepted:

```text
Claude 规划，Codex review，Codex 执行，Claude audit
```

The Room exposes the compiled stages and current state. The sequence is not
a hard-coded product ceremony: the user can choose any order and repeat an
actor, for example `Claude plan → Codex review → Claude execute`, or
`Codex plan → Claude review → Codex execute → Claude audit`.

Stage policy is derived from the action:

- `plan` / `规划` uses Claude's native plan mode or Codex's read-only turn
  sandbox. It cannot silently become implementation.
- `review` / `审查` and `audit` / `复核` are independent and read-only.
- `execute` / `执行` receives the live Driver workspace and write-capable
  native policy.
- If planning or review precedes execution, PairRoom pauses at a human gate.
  Sending `批准执行当前计划` or `approve` approves only the displayed plan
  revision. A later plan revision requires another approval.

A stage that needs a decision ends with `@human` and
`[PAIRROOM:WAIT]`. The reply is associated with the same stage. Codex
`request_user_input` and MCP elicitation requests that cannot be represented
by the headless app-server client are converted into a visible Room question
and the native turn is interrupted safely; PairRoom no longer leaves a
five-minute silent wait with no exposed prompt.

Ordinary messages remain ordinary. PairRoom compiles a workflow only when it
sees at least two explicit actor/action pairs, so `@claude review this` and
`Codex execute this fix` keep their existing routing behavior.

## Provider profiles

Claude and Codex choose providers independently:

```json
{
  "providers": [
    {
      "name": "company-proxy",
      "api_key": "env:COMPANY_LLM_KEY",
      "agent_types": ["claudecode", "codex"],
      "endpoints": {
        "claudecode": "https://proxy.example/anthropic",
        "codex": "https://proxy.example/openai/v1"
      },
      "agent_models": {
        "claudecode": "claude-opus-4-1",
        "codex": "gpt-5.6-codex"
      },
      "codex": {
        "env_key": "PAIRROOM_COMPANY_KEY",
        "wire_api": "responses"
      }
    }
  ],
  "claude": {"command": "claude", "provider": "company-proxy"},
  "codex": {"command": "codex", "provider": "company-proxy"}
}
```

API keys may be literal for compatibility with cc-connect, but an environment
reference (`env:NAME` or `${NAME}`) is preferred. Keys are passed only in the
child process environment. They are never placed in command arguments,
RuntimeInfo, provider summaries, diagnostics, or the browser snapshot.

Inspect the effective, redacted assignment before starting the service:

```bash
pairroom providers --config pairroom.json
pairroom providers --config pairroom.json --json
```

### Referencing cc-connect providers

PairRoom can read the provider tables from an existing cc-connect TOML file
without copying its credentials:

```json
{
  "cc_connect": {
    "path": "~/.cc-connect/config.toml",
    "providers": ["company-proxy", "backup"],
    "prefix": "cc-"
  },
  "claude": {"command": "claude", "provider": "cc-company-proxy"},
  "codex": {"command": "codex", "provider": "cc-backup"}
}
```

The importer intentionally reads only `[[providers]]` and its provider
subtables (`models`, `env`, `endpoints`, `agent_models`, and `codex`).
cc-connect projects, platforms, sessions, hooks, and unrelated settings are
ignored. Explicit PairRoom profiles win on a same-name conflict.

Provider selection is process-start configuration. Changing it requires a
Room runtime or service restart; PairRoom does not pretend a live native
session can safely switch credentials or model endpoints mid-turn.
