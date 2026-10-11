# Agent-assisted setup

This page is written for a coding Agent ([Claude Code](https://code.claude.com/docs/en/overview), [Codex](https://github.com/openai/codex), [Grok Build](https://docs.x.ai/build/overview), or [Gemini CLI](https://github.com/google-gemini/gemini-cli)) that a user has asked to install PairRoom and check the environment; a human can follow it the same way. It sequences checks and routes to the owning guides instead of restating them. When this page and the installed binary disagree, that binary's `pairroom <command> --help` wins. Relative links resolve against `https://github.com/sean2077/pairroom/blob/main/docs/`; if the user already has a release installed, that tag's documentation matches it more closely.

A user can start with:

```text
Read https://raw.githubusercontent.com/sean2077/pairroom/main/docs/AGENT_SETUP.md
and help me install PairRoom and check my environment. Ask before each change.
```

## Rules for the Agent

- Ask before installing software, changing PATH, writing project files, installing a daemon, or running anything that can consume model quota. Show the exact command first.
- Never log in, approve hooks, grant folder trust, or answer a permission or elevation prompt for the user. Stop and hand those steps back.
- Never start a second Service over a data root that already has one (Desktop, an installed daemon, or a foreground `pairroom service`). A foreground Service also blocks a tool call; prefer the user opening Desktop or running it in their own terminal.
- Treat Management URLs, bootstrap tokens and `relay-endpoint.json` as credentials. Do not read, print, paste or publish their contents; refer to `relay-endpoint.json` only by path.
- Report every check as **pass**, **fail** or **needs-user**, with the command that produced it. A version string or an ordinary `doctor` pass does not prove authentication or model access.

## 1. Choose a path

| Path | Use it for | Steps |
|---|---|---|
| **Native local pair** — default and recommended | Keeping the user's own Claude Code, Codex (including Desktop), Grok Build, or Gemini CLI sessions on one host | 2–4, then 6 |
| **Native LAN host** | Hosting a shared Room for a colleague's remote session | 2–4 for the host's Runtime, then 6a–6c and the [LAN workflow](LAN_NATIVE.md#create-request-accept) |
| **Native LAN guest** | Joining a colleague's Room directly with CLI/hooks, without a local Service | 2, the guest's Runtime in 3, then 6a–6c with `preflight --join` and the [LAN workflow](LAN_NATIVE.md) |
| **Embedded** — optional | PairRoom-owned adapters and conversation controls, per-slot Provider overrides, or a Mock demonstration | 2–4, then 5 |

For a local pair, establish which Runtime each slot will use. Both slots may use the same Runtime and still differ in Provider and model. For a shared LAN Room, establish only this machine's Runtime: the colleague's joining session supplies its actual Runtime at admission.

Embedded can choose per-slot Providers through a read-only [CC Switch Provider reference](CONFIGURATION.md#cc-switch-provider-references); Native keeps each original session's configuration. [CC Switch](https://github.com/farion1231/cc-switch) is optional. Gemini Embedded uses native authentication only: CC Switch and effort overrides are unsupported, and exact resume after an accepted session's process exits is blocked rather than replaced. Prefer Native for Gemini sessions that must survive suspension or Service restart; see [Gemini boundaries](NATIVE_RELAY.md#gemini-cli). Try the intended Runtime/Provider combination on a small authorized task before relying on it.

## 2. Make `pairroom` available in this tool shell

Run `pairroom version`. If it prints a version here, continue with step 3. This shell is what Native uses: Desktop running, or `pairroom` working in another terminal, does not prove the CLI is on this shell's PATH.

Otherwise choose a channel with the user. [Installation](INSTALLATION.md) owns the details.

| Platform | Channel | Where the CLI ends up |
|---|---|---|
| Windows | Desktop `setup.exe` from [Releases](https://github.com/sean2077/pairroom/releases/latest) (machine-scoped, so Windows may ask the user for elevation); check [WinGet availability](INSTALLATION.md#winget) before choosing that channel | `C:\Program Files\PairRoom contributors\PairRoom\bin\pairroom.exe` by default. Setup adds that `bin` directory to the machine PATH unless the user opted out. |
| macOS | Desktop `.app.zip` from Releases | `PairRoom.app/Contents/Helpers/pairroom`. Desktop offers to link `/usr/local/bin/pairroom` to it on first start, or later from the menu bar item **Install Command Line Tool…**; the user enters the administrator password. |
| Linux (Debian/Ubuntu) | Desktop `.deb` | `/usr/local/bin/pairroom` |
| Linux AppImage | Desktop AppImage | Not included; install the matching CLI separately with `install.sh` |
| Linux, macOS, Git Bash | CLI `install.sh` | `/usr/local/bin` when writable, otherwise `~/.local/bin`; `PREFIX` overrides |

For `install.sh`, download it first:

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh -o install-pairroom.sh
```

Let the user read the file, then run it:

```bash
sh install-pairroom.sh
```

The installer verifies the release checksum and warns when the destination is not on PATH. [CLI installation](INSTALLATION.md#cli-on-any-platform) lists supported architectures and the version and destination overrides.

If the CLI is installed but not found, first ask the user to restart the harness: a running harness keeps the environment it started with, including after a Windows Setup that just added the PATH entry. If it is still missing, propose adding its directory to the user's PATH and wait for confirmation, then run `pairroom version` again in a new session. For Native, repeat this check in **both** harnesses; Codex Desktop and a terminal can see different PATHs.

Use one release: the CLI that Native sessions run must come from the same release as the Room's hosting Service. With Desktop, prefer its bundled CLI over a separately installed copy. A LAN guest needs no local Service; coordinate the CLI release with the host.

## 3. Check Git and the Runtimes

Verify Git and the Runtimes used on this machine. A LAN guest needs only the Runtime of its joining session.

From the user's project repository:

```bash
git --version
# Run only the version commands for the selected Runtimes.
claude --version
codex --version
grok --version
gemini --version
```

Ask the user to confirm that each selected harness is signed in and works independently in the intended repository. PairRoom never logs in. For Native, continue to step 4 (local host) or 6a (LAN guest); the relevant `relay preflight` in step 6c checks hooks, session eligibility, and transport. A two-adapter `doctor` check is not required for a guest.

**For Embedded**, also run `pairroom doctor --repo . --json`. By default it probes Agent 1 as Claude Code and Agent 2 as Codex: executable path, version and protocol surface. `"ok": true` means both were found and expose a supported protocol; it neither logs in nor calls a model. For another pair, for example one including Grok Build, write a small configuration file outside the repository and pass it:

```json
{"claude": {"runtime": "grok"}, "codex": {"runtime": "claude"}}
```

```bash
pairroom doctor --config /absolute/path/doctor-pair.json --repo . --json
```

In configuration files, the `claude` and `codex` objects are the historical keys for Agent 1 and Agent 2, not Runtime choices; `runtime` selects the harness. A failed Runtime means installing or updating that official CLI, or pointing `--claude-command`, `--codex-command`, `--grok-command` or `--gemini-command` at it (the flag follows the Runtime, not the slot). The links at the top of this page lead to each vendor's setup instructions.

Only with explicit consent, because it uses quota: add `--live` to the same command, keeping its `--config` and command-path overrides so the same pair is tested. For the custom pair above:

```bash
pairroom doctor --config /absolute/path/doctor-pair.json --repo . --live --json
```

For the default pair, `pairroom doctor --repo . --live --json` is enough. The live check starts a fresh adapter-driven native session per slot in a disposable Git workspace and requires a nonce reply. It demonstrates sign-in and a model response for that tested configuration; it does not test an existing Native session or its hooks. See [CLI reference](CLI_REFERENCE.md#installation-versus-runtime-availability).

For Embedded per-slot Providers, `pairroom providers --json` lists CC Switch Profiles read-only and redacted, including why an unsupported Profile is disabled. It never changes CC Switch's current Profile.

## 4. Find or start the Service

This step applies to hosting Rooms. Skip it when only joining a colleague's LAN Room: CLI/hooks connect directly, and a local dashboard is optional. One machine can also host its own Rooms and join other hosts through different native sessions, with no global server switch.

Exactly one Service owns a data root. Determine which one applies:

- Desktop has loaded Management (window or tray): use its Service. Desktop may own it or reuse an installed daemon; opening another Service is unnecessary.
- `pairroom daemon status` reports `running`: use it; `pairroom daemon open` opens the authenticated Management page.
- A foreground `pairroom service` already owns the intended data root: keep it running and use its Management page.
- None of those: ask the user to open Desktop, or to run `pairroom service` in their own terminal (`Ctrl+C` stops it). Run `pairroom daemon install` for a persistent background Service only with explicit consent.

If an installed daemon is stopped or unhealthy, resolve that installation through [Operations](OPERATIONS.md#desktop-lifecycle); do not start a competing owner for its root.

For a first look that calls no vendor CLI and spends no quota, use an unused, isolated data root with `pairroom service --mock --data-root "$HOME/.pairroom-demo"`. Select Embedded explicitly for the demonstration and stop the demo when finished. Mock verifies the control plane, not model quality. [Operations](OPERATIONS.md) owns Service lifecycle.

## 5. Embedded: the first Room

Use Management for this path; if the Agent has no authorized UI access, give the user this checklist:

1. Open Management (the Desktop window, or `pairroom daemon open`).
2. Register the repository's absolute path as a **Project**.
3. Change the default Native selection to **Embedded** when creating the Room. For each Agent choose Runtime, supported optional Provider, model and effort fields; empty fields inherit native configuration. Gemini uses native Provider settings and does not support effort overrides.
4. Both participants default to **YOLO**. For the first test, select read-only native permissions explicitly.
5. Send the first task from [Getting started](GETTING_STARTED.md#first-real-room).

**Settings → Diagnostics** in Management repeats these checks, offers the live check behind a confirmation, and downloads a safe report for support requests.

## 6. Native: bind two existing sessions

For a local pair, continue when steps 2–4 pass in each harness's tool shell and a non-Mock Service is running. [Native relay](NATIVE_RELAY.md) owns hook installation and transport behavior.

For a shared LAN Room, the host completes steps 2–4 for its own Runtime; the guest completes steps 2–3 and needs no local Service. Both follow 6a–6c below, then use the [LAN invitation workflow](LAN_NATIVE.md#create-request-accept) in place of 6d. The host enables the LAN listener and creates with `bind --create --share lan`; the guest joins through the invitation and receipt approval. An existing local Room cannot be converted to a shared Room.

**a. Install the hooks.** This writes project files, so confirm first. From the project worktree:

```bash
pairroom relay install --runtime claude,codex   # any of: claude (cc), codex, grok, gemini
```

It writes `.claude/settings.json` for Claude Code (Grok Build reuses it by default) and `.codex/hooks.json` for Codex, preserving unrelated entries, and installs the `pairroom-relay` skill into each harness's skill root. Gemini instead installs BeforeTool and AfterAgent in `.gemini/settings.json`; its official session identity comes from the approved BeforeTool hook, not a manufactured `GEMINI_SESSION_ID`. Show the user the resulting diff; committing the hook files is their decision.

**b. Stop for approval.** The user reviews and approves the exact hook in each harness: Codex `/hooks`; Claude Code project hook consent; Grok `/hooks`, press `r` to reload, then folder trust; Gemini `/hooks` and trusted workspace, approving/reloading both BeforeTool and AfterAgent. Follow each harness's reload or restart guidance so the hook and skill are loaded. Continue only after the user confirms.

**c. Preflight.** In each Agent session, run `pairroom relay preflight`. Before a local bind, this is a read-only check of the bare `pairroom` command on this shell's PATH, workspace/session eligibility, Service reachability/version, and the Runtime's required response hooks. Continue when `ready` is `true`; otherwise follow `next_steps` in order and rerun it. A local Service version mismatch only warns and still reports `ready`, so read `next_steps` either way. It cannot see hook approval.

Before an initial LAN join, use **`pairroom relay preflight --join`**. It checks the joining session's local prerequisites without requiring or contacting a local Service. Do not pass `--service-file` to join. After admission, ordinary `preflight` checks the recorded host directly and may activate a suspended host Room; it does not publish or collect a message. Use `relay doctor` after binding to compare the remote Service version. See [LAN observation and recovery](LAN_NATIVE.md#observe-and-recover).

**d. Create and join a local Room.** In the first session, run `/pairroom-relay <topic>`, or `pairroom relay bind --create --name "<topic>"` as a tool call. It prints `peer_join_local` and `peer_join`; the user gives one to the second session, whose Agent runs it as a tool call. Bind must run as the Agent's own tool call: it reads the official session ID and fails closed in a detached terminal or in Grok's `!` shell mode. Gemini must use a fresh standalone `run_shell_command` tool call so its BeforeTool identity observation is still valid. If creation succeeded but bind failed, follow the printed recovery command instead of repeating `--create`.

**e. Verify.** After a bound session has finished at least one turn, run `pairroom relay doctor` in it. In `local`, expect `protocol_match` and `service_version_match` to be `true`, `hook_installation` to be `installed`, and `last_hook_at` to be recent. That timestamp records the installed response hook running for this binding (Stop, or Gemini's AfterAgent), so it is the practical sign that approval took effect. `hook_approval` is always reported as `unknown`, and model acceptance is not reported. While `last_hook_at` is empty, `local.hook_hint` restates this check. Day-to-day relay use then belongs to the `pairroom-relay` skill.

## When a check fails

| Symptom | Action |
|---|---|
| `pairroom` not found in the tool shell | Step 2: fix PATH, restart the harness, check again |
| A `doctor` Runtime entry fails | Install or update that CLI, or pass its `--*-command` path |
| `relay preflight` is not `ready` | Follow its `next_steps` in order, then rerun it |
| `relay doctor` reports no matching binding | Expected before step 6d: it inspects a bound session, so use `relay preflight` until then |
| Bind cannot reach the Service | Step 4. A custom data root uses `--service-file <root>/relay-endpoint.json`, as a path |
| Initial LAN setup asks for a local Service | Use `relay preflight --join`, then the invitation command. Only the Room's host needs a Service |
| Bind rejects a missing or disabled hook | Steps 6a and 6b for that Runtime |
| Bind reports missing session identity | Run it as the Agent's tool call inside the intended session |
| `relay doctor` reports a version mismatch | Use the CLI from the Service's release (step 2) |
| `last_hook_at` stays empty after a finished turn | The hook is not approved or not loaded: review it again in the harness and reload |

Deeper diagnosis: [Native relay errors](TROUBLESHOOTING.md#native-relay-errors), indexed by exact message, and [Native relay troubleshooting](NATIVE_RELAY.md#troubleshooting).

## Report back

End with a short report the user can act on:

```text
PairRoom setup report
- CLI:            <version> at <path>                     [pass|fail]
- Git:            <version>                               [pass|fail]
- Agent 1:        <runtime> <version>                     [pass|fail]
- Agent 2:        <runtime> <version>                     [pass|fail]
- Login/model:    confirmed by user | live check passed   [needs-user|pass]
- Service:        Desktop | daemon | foreground | remote host (LAN guest) [pass|needs-user]
- Native preflight: ready | not ready (<first next step>)  [pass|fail]
- Native hooks:   installed for <runtimes>; last_hook_at=<timestamp or empty>  [pass|needs-user]
- Next step:      <the one thing the user should do now>
```

For a LAN guest, report only its own Runtime and mark the other local slot as not applicable. `last_hook_at` shows the hook ran; PairRoom has no way to read the approval itself. This report contains local paths. Before sharing it publicly, remove them, or use the safe report from **Settings → Diagnostics** instead.
