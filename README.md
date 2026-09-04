# PairRoom

**English** · [简体中文](#简体中文)

<p align="center">
  <img src="docs/images/pairroom-runtime-overview.png" alt="PairRoom Room View: two Agent slots collaborating turn by turn">
</p>

PairRoom is a local collaboration control plane for official Claude Code, Codex, and Grok Build harnesses. Each durable Room has two independent Agent slots. Either slot may select Claude Code, Codex, or Grok Build, and both slots may use the same runtime. PairRoom keeps each native CLI's sessions, tools, approvals, and sandbox; it only organizes the user, two Agents, and the project workspace into an observable, interruptible, and auditable collaboration.

Empty `provider`, `model`, `effort`, and `instructions` inherit the selected native CLI's user/global configuration. Add only explicit PairRoom overrides.

## Core model

PairRoom does not let two Agents wake each other concurrently like an IM group chat. Each Room has one **native Turn owner** at a time:

```text
user
  -> current Agent completes one native Turn
  -> reliable terminal boundary
  -> explicit @peer or HANDOFF + NEXT
  -> next Room FIFO item
```

This is not a mechanical A/B/A/B message rotation. The current Agent can run tools, update a plan, and accept steering inside one native Turn. The other Agent can start only after a reliable Turn-complete boundary.

Key properties:

- **Human authority**: the user can choose the target Agent, override later flow, approve, cancel, or stop;
- **Single owner**: two native runtimes never own execution at the same time, even when both slots use the same runtime;
- **Explicit handoff**: an Agent that addresses `@agent1`, `@agent2`, `@claude`, `@codex`, `@grok`, or `@peer` hands the reply to that peer. `@claude`/`@codex` remain Agent 1/Agent 2 slot aliases and also resolve to a unique matching runtime. When a human asks both Agents to interact, that address must be written; speaking only to the human does not start the other Agent. Without an explicit address, the reply must include both `HANDOFF` and `NEXT`. `@human`/`@user` returns the decision to the user;
- **Fail closed**: a process restart does not replay the in-memory FIFO, avoiding duplicate side effects;
- **Native harness first**: PairRoom does not rewrite the Claude Code, Codex, or Grok Build tool loops or permission models.

## Install

CLI (Linux / macOS / Git Bash):

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
```

Windows PowerShell:

```powershell
$tag = (Invoke-RestMethod https://api.github.com/repos/sean2077/pairroom/releases/latest).tag_name
curl.exe -fsSL -o pairroom.exe "https://github.com/sean2077/pairroom/releases/download/$tag/pairroom-cli-$tag-windows-amd64.exe"
```

Release assets are named by prefix: `pairroom-cli-vX.Y.Z-…` is the command line, `pairroom-desktop-vX.Y.Z-…` is the desktop package. Windows desktop uses `-setup.exe` (distinct from the CLI `.exe`); Linux uses `.deb` / `.AppImage`, and macOS uses `.app.zip`.

## Quick start

Verify PairRoom itself without starting real Agents:

```bash
go run ./cmd/pairroom service --mock
```

After the Management Shell opens:

1. Register a local Git Project;
2. Create a Room;
3. Choose Driver / Reviewer;
4. Send a single-Agent task, or describe a sequential workflow;
5. Watch Turn, tool activity, approvals, delivery, and error state in Room View.

Before using a real runtime, confirm that each selected CLI (`claude`, `codex`, and/or `grok`) is installed, signed in, and can work independently in the target repository. Empty provider/model/effort/instructions inherit that CLI's global config. Full steps are in [Getting Started](docs/GETTING_STARTED.md).

## Desktop

`desktop/` provides a native Windows, macOS, and Linux entry built with **Wails v3**. It is not a second PairRoom backend or frontend: the desktop host reuses the existing Management Shell, Room View, Service Registry, Runtime Manager, configuration, locks, and native Agent adapters.

On startup, the desktop host:

1. Validates and reuses an explicitly supplied authenticated numeric-loopback Management URL;
2. Discovers an installed `pairroom daemon` and, if it is stopped, starts it and waits for an authenticated Management URL;
3. Starts a PairRoom Service in the current desktop process only when no daemon is installed. If a daemon is installed but unreachable, it fails closed and does not start a second Service.

Closing the main window only hides to the system tray and does not interrupt an active Agent. Explicit quit shuts down only an embedded Service owned by the desktop host, draining along the existing native-Turn boundary; an external daemon is unaffected. Build, dependency, and package notes are in [PairRoom Desktop](desktop/README.md). Browser and CLI entry points remain fully available.

## Documentation

- [Documentation map](docs/README.md)
- [Core concepts and relay semantics](docs/CONCEPTS.md)
- [Configuration and providers](docs/CONFIGURATION.md)
- [CLI reference](docs/CLI_REFERENCE.md)
- [Architecture and invariants](docs/ARCHITECTURE.md)
- [Operations, backup, and restore](docs/OPERATIONS.md)
- [Upgrading](docs/UPGRADING.md)
- [Contributing](CONTRIBUTING.md)

## Development verification

Root module:

```bash
make docs-check
make check
make smoke
```

Desktop module:

```bash
make desktop-build
make desktop-package
```

`make desktop-build` builds the current-platform desktop host and bundled `pairroom` CLI. `make desktop-package` builds the current-platform production installer or app bundle (Windows NSIS, including `PairRoom.exe` and `bin\pairroom.exe`). Artifacts land in `desktop/bin/`. Desktop module tests still run from `desktop/`: `cd desktop && go test -count=1 ./...`.

`docs-check` verifies documentation links, source paths, CLI flags, HTTP routes, and JSON configuration fields so docs cannot silently drift as the code evolves. The desktop module uses a separate Go toolchain and dependency lock and does not change the root module's standard-library-only boundary.

## Status and boundaries

PairRoom is still evolving quickly. Breaking changes to the CLI, HTTP API, Event Log, and Agent protocol are recorded in [CHANGELOG](CHANGELOG.md) and [Upgrading](docs/UPGRADING.md). Current Mock E2E can verify scheduling, persistence, and recovery, but it does not replace real Claude Code / Codex / Grok Build native E2E. Unsigned desktop CI packages also do not replace production signing and macOS notarization.

## License

[MIT](LICENSE)

---

# 简体中文

<p align="center">
  <img src="docs/images/pairroom-runtime-overview.png" alt="PairRoom Room View：两个 Agent 槽位在同一 Room 中按 Turn 协作">
</p>

PairRoom 是一个运行在本机的协作控制面，协调官方 Claude Code、Codex 与 Grok Build。每个持久 Room 有两个独立的 Agent 槽位；任一槽位都可以选择 Claude Code、Codex 或 Grok Build，两个槽位也可以使用相同 runtime。它保留各官方 CLI 的会话、工具、审批与沙箱能力，只负责把用户、两个 Agent 和项目工作区组织成可观察、可中断、可审计的协作过程。

空的 `provider`、`model`、`effort` 和 `instructions` 会继承所选原生 CLI 的用户/全局配置。只在 PairRoom 中填写需要覆盖的值。

## 核心模型

PairRoom 不让两个 Agent 像 IM 群聊一样并发互相唤醒。每个 Room 同一时刻只有一个 **native Turn owner**：

```text
user
  -> current Agent completes one native Turn
  -> reliable terminal boundary
  -> explicit @peer or HANDOFF + NEXT
  -> next Room FIFO item
```

这不是机械的 A/B/A/B 消息轮换。当前 Agent 可以在一个 native Turn 内执行工具、更新计划并接受 steering；只有在可靠的 Turn 结束边界之后，另一个 Agent 才能开始。

关键性质：

- **Human authority**：用户可以指定目标 Agent、覆盖后续流程、审批、取消或停止；
- **Single owner**：两个 native runtime 不会同时拥有执行权，即使两个槽位选择了相同 runtime；
- **Explicit handoff**：Agent 明确 `@claude`、`@codex` 或 `@peer` 即表示把回复交给该 peer 槽位（`claude` 是 Agent 1，`codex` 是 Agent 2）；人类要求双方互动时必须写出该地址，只对人类说话不会启动另一位；没有明确地址时，必须同时给出 `HANDOFF` 与 `NEXT`；`@human`/`@user` 则回到用户决策；
- **Fail closed**：进程重启不自动重放内存 FIFO，避免重复执行有副作用的操作；
- **Native harness first**：PairRoom 不重写 Claude Code、Codex 或 Grok Build 的工具循环与权限模型。

## 安装

CLI（Linux / macOS / Git Bash）：

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
```

Windows PowerShell：

```powershell
$tag = (Invoke-RestMethod https://api.github.com/repos/sean2077/pairroom/releases/latest).tag_name
curl.exe -fsSL -o pairroom.exe "https://github.com/sean2077/pairroom/releases/download/$tag/pairroom-cli-$tag-windows-amd64.exe"
```

Release 资产按前缀区分：`pairroom-cli-vX.Y.Z-…` 是命令行，`pairroom-desktop-vX.Y.Z-…` 是桌面包。Windows 桌面是 `-setup.exe`（与 CLI 的 `.exe` 区分）；Linux 用 `.deb` / `.AppImage`，macOS 用 `.app.zip`。

## 快速体验

只验证 PairRoom 本身，不启动真实 Agent：

```bash
go run ./cmd/pairroom service --mock
```

Management Shell 打开后：

1. 注册一个本地 Git Project；
2. 创建 Room；
3. 选择 Driver / Reviewer；
4. 发送一个单 Agent 任务，或描述一个顺序工作流；
5. 在 Room View 中观察 Turn、工具活动、审批、投递与错误状态。

使用真实 runtime 前，先分别确认所选 CLI（`claude`、`codex` 和/或 `grok`）已安装、已登录，并能在目标仓库独立工作。空的 provider/model/effort/instructions 会继承该 CLI 的全局配置。完整步骤见 [Getting Started](docs/GETTING_STARTED.md)。

## 桌面端

`desktop/` 提供基于 **Wails v3** 的 Windows、macOS 与 Linux 原生入口。它不是第二套 PairRoom 后端或前端：桌面 Host 直接复用现有 Management Shell、Room View、Service Registry、Runtime Manager、配置、锁和 native Agent adapters。

启动时，桌面端会按顺序：

1. 验证并复用显式提供的 authenticated numeric-loopback Management URL；
2. 发现已安装的 `pairroom daemon`，停止状态会先由桌面端启动并等待 authenticated Management URL；
3. 只有没有安装 daemon 时，才在当前桌面进程中启动 PairRoom Service；已安装但不可达时 fail closed，不启动第二个 Service。

关闭主窗口只会隐藏到系统托盘，不会中断活动 Agent。显式退出只关闭桌面端拥有的内嵌 Service，并沿现有 native-Turn drain 边界优雅退出；外部 daemon 不受影响。构建、依赖和安装包说明见 [PairRoom Desktop](desktop/README.md)。浏览器和 CLI 入口保持完整可用。

## 文档入口

- [文档地图](docs/README.md)
- [核心概念与接力语义](docs/CONCEPTS.md)
- [配置与 Provider](docs/CONFIGURATION.md)
- [CLI 参考](docs/CLI_REFERENCE.md)
- [架构与不变量](docs/ARCHITECTURE.md)
- [运维、备份与恢复](docs/OPERATIONS.md)
- [升级说明](docs/UPGRADING.md)
- [贡献指南](CONTRIBUTING.md)

## 开发验证

根模块：

```bash
make docs-check
make check
make smoke
```

桌面模块：

```bash
make desktop-build
make desktop-package
```

`make desktop-build` 构建当前平台的桌面 Host 和捆绑的 `pairroom` CLI，`make desktop-package` 构建当前平台的生产安装包或应用包（Windows 为 NSIS 安装包，内含 `PairRoom.exe` 与 `bin\pairroom.exe`），产物位于 `desktop/bin/`。桌面模块测试仍从 `desktop/` 目录运行：`cd desktop && go test -count=1 ./...`。

`docs-check` 会校验文档链接、源码路径、CLI 参数、HTTP 路由和 JSON 配置字段，防止文档在代码继续演进后静默漂移。桌面模块使用独立的 Go toolchain 和依赖锁，不改变根模块的标准库依赖边界。

## 状态与边界

PairRoom 仍在快速演进。CLI、HTTP API、Event Log 和 Agent 协议的 breaking change 会记录在 [CHANGELOG](CHANGELOG.md) 与 [Upgrading](docs/UPGRADING.md)。当前 Mock E2E 可以验证调度、持久化和恢复链路，但不能替代真实 Claude Code / Codex / Grok Build native E2E；桌面 CI 的 unsigned packages 也不能替代生产签名与 macOS notarization。

## License

[MIT](LICENSE)
