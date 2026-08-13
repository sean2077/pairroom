# PairRoom

PairRoom 是一个面向 **Claude Code + Codex** 的本地三方协作房间：你、Claude Code 和 Codex 出现在同一条 IM 式时间线中，两个 Agent 保留各自官方 CLI 的完整 Harness，并可以在你设定的路由规则下互相讨论、独立审查和接力工作。

> PairRoom 不依赖 OMA、CCCC、ccteam、wmux、Cherry Studio、Chatbox 或任何第三方 Agent 编排框架。原生模式只启动用户本机已有的官方 `claude` 与 `codex` 命令；项目本身使用 Go 标准库实现。

当前版本：**v0.1.0 MVP**

## 它解决什么问题

普通的“双模型聊天”只能利用模型差异，无法保留 Claude Code 与 Codex 各自的：

- Agent loop、上下文管理与会话恢复
- 项目级 `CLAUDE.md` / `AGENTS.md`
- Skills、MCP、Hooks 与原生工具
- 文件编辑、命令执行、沙箱和审批语义
- 子 Agent、计划、Diff、用量等运行时事件

PairRoom 不重写这些能力，而是在两套官方 Harness 之上增加一个很薄的协作层：

```text
┌──────────────────────── PairRoom Web UI ────────────────────────┐
│  Shared Room: You · Claude Code · Codex                         │
│  @mention · reply · delivery state · interrupt · role switch    │
│                                                                 │
│  Inspector: tools · commands · plans · diffs · usage · approval │
└───────────────────────────┬─────────────────────────────────────┘
                            │ REST + SSE
┌───────────────────────────▼─────────────────────────────────────┐
│ PairRoom daemon                                                  │
│ event log · room state · routing · canonical runtime events      │
└───────────────────┬───────────────────────────┬─────────────────┘
                    │                           │
        Claude Code stream-json          Codex app-server
                    │                           │
             official `claude`             official `codex`
```

## 已实现

### 三方 IM 房间

- 你、Claude Code、Codex 共用一条持久化时间线
- `@claude`、`@codex`、`@all`、`@peer` 路由
- 引用回复与线程关联
- 每个目标独立显示投递状态：
  - `started`：开始新 Turn
  - `injected`：注入 Codex 当前 Turn
  - `queued`：等待安全 Turn 边界
  - `failed`：投递失败
- 用户新消息优先于尚未完成的旧 Agent 自动接力

### 两套官方 Runtime

- Claude Code：双向 `stream-json` 长会话、流式文本、工具调用、会话恢复、子 Agent 文本转发
- Codex：`app-server` JSON-RPC、thread 恢复、`turn/start`、`turn/steer`、`turn/interrupt`、命令/文件/权限审批、Diff、计划和用量事件
- 不接管供应商登录、API Key、模型配置、Skills、MCP、Hooks 或项目指令文件

### 协作控制

- 三种路由模式：
  - `manual`：Agent 回答永不自动转给 Peer
  - `mentions`：只有显式 `@peer` / `@claude` / `@codex` 才触发 Peer
  - `roundtable`：双方自动交替，受 hop budget、控制标记和用户插话约束
- 三种角色：
  - `Driver`：负责实现和验证
  - `Reviewer`：负责独立审查
  - `Peer`：平级讨论
- 一键启动、停止、重启和打断单个 Agent
- Driver 一键切换，另一方同步成为 Reviewer

### Work Inspector

- Agent 状态、Session ID、当前 Turn
- 流式输出与完整最终回复分层
- 工具调用、命令输出、计划、Diff、用量、错误与日志
- Codex 命令、文件修改和追加权限审批
- 当前 Git 状态与工作区 Diff

### 本地可靠性

- `events.jsonl` append-only 事件日志
- 每条事件 `fsync` 后才向 UI 发布
- 进程崩溃留下的单条不完整尾行可安全忽略
- 重启后恢复房间消息、设置、角色和供应商 Session ID
- 本地 UI 通过 SSE 实时重放；序列号消除重复事件
- 无数据库、无 Node.js 运行时、无前端构建步骤

## 快速开始

### 1. Mock 模式验证界面

不需要安装或登录任何 Agent：

```bash
go run ./cmd/pairroom serve --repo . --mock
```

浏览器会打开 `http://127.0.0.1:7332`。Mock Agent 会模拟三方讨论、消息接力和流式输出。

### 2. 检查原生环境

```bash
go run ./cmd/pairroom doctor --repo /path/to/project
```

需要：

- Go 1.23+（仅构建 PairRoom 时需要）
- Git
- 官方 Claude Code CLI，命令为 `claude`
- 官方 Codex CLI，命令为 `codex`，且包含 `codex app-server`
- 两个 CLI 已按官方方式完成登录

### 3. 启动真实 Pair

```bash
go run ./cmd/pairroom serve --repo /path/to/project
```

或者构建单文件二进制：

```bash
make build
./dist/pairroom serve --repo /path/to/project
```

Windows PowerShell：

```powershell
go build -trimpath -o dist\pairroom.exe .\cmd\pairroom
.\dist\pairroom.exe serve --repo C:\path\to\project
```

## 常用命令

```bash
# 不自动启动 Agent，进入房间后手动启动
pairroom serve --repo . --auto-start=false

# 只在 Agent 明确 @Peer 时接力（默认）
pairroom serve --repo . --routing mentions

# 自动轮桌讨论，最多 8 次 Agent-to-Agent 接力
pairroom serve --repo . --routing roundtable --max-hops 8

# 完全手动路由
pairroom serve --repo . --routing manual

# 指定模型
pairroom serve --repo . \
  --claude-model opus \
  --codex-model gpt-5.6-sol \
  --codex-effort high

# 指定独立状态目录
pairroom serve --repo . --data-dir ./.pairroom-local
```

查看完整参数：

```bash
pairroom serve -help
```

## 配置文件

复制 [`examples/pairroom.example.json`](examples/pairroom.example.json)：

```json
{
  "listen": "127.0.0.1:7332",
  "room_name": "Claude × Codex",
  "routing_mode": "mentions",
  "max_agent_hops": 6,
  "auto_start": true,
  "claude": {
    "command": "claude",
    "model": "",
    "permission_mode": "auto"
  },
  "codex": {
    "command": "codex",
    "model": "",
    "effort": "high",
    "approval_policy": "unlessTrusted",
    "sandbox": "workspaceWrite"
  }
}
```

启动：

```bash
pairroom serve --config ./pairroom.json --repo /path/to/project
```

命令行参数覆盖配置文件。

## 在房间中如何使用

### 默认推荐：一个写入者，一个审查者

```text
Claude Code  Driver
Codex        Reviewer
```

向双方发送：

```text
@all 请先各自理解问题。Claude 提出实现方案，Codex 独立检查并发、兼容性和测试遗漏；未达成一致前不要修改。
```

让 Claude 实现后再触发审查：

```text
@claude 按已确认方案实现并运行测试。完成后 @codex 做完整 diff 审查。
```

随时介入：

```text
@codex 暂停当前方向，先检查对象生命周期。
@claude 不要改变公开 API，沿用现有 executor。
@all 先停止实现，解释两种方案的权衡。
```

### Roundtable 控制标记

在 `roundtable` 模式下，Agent 可在最终回复末尾使用：

```text
[PAIRROOM:CONTINUE]
[PAIRROOM:CONSENSUS]
[PAIRROOM:WAIT]
[PAIRROOM:BLOCKED]
[PAIRROOM:DONE]
```

标记不会显示在聊天时间线。`CONSENSUS`、`WAIT`、`BLOCKED`、`DONE` 会停止自动接力；达到 hop budget 或出现更新的用户消息也会停止。

## 数据与目录

默认状态目录按仓库绝对路径计算，位于用户配置目录下：

```text
pairroom/rooms/<repo-name>-<path-hash>/
├── events.jsonl
└── runtime/
    └── claude-pairroom-prompt.md
```

PairRoom 不保存供应商 API Key。Claude Code 与 Codex 的身份和凭据仍由官方 CLI 自己管理。

## 设计边界

PairRoom **负责**：

- 房间消息、路由、角色、投递状态和用户介入
- Claude/Codex 结构化协议适配
- Canonical runtime event、事件持久化和 Web UI

PairRoom **不负责**：

- 重写 Agent loop 或工具执行器
- 统一模型 API 或转发用户订阅凭据
- 替代 `CLAUDE.md`、`AGENTS.md`、Skills、MCP、Hooks
- 自动判断代码是否真正正确
- 在两个 Agent 同时写同一 working tree 时解决语义冲突

详细设计见：

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- [`docs/PROTOCOL.md`](docs/PROTOCOL.md)
- [`docs/ROADMAP.md`](docs/ROADMAP.md)
- [`SECURITY.md`](SECURITY.md)

## v0.1 限制

- 一个 PairRoom 进程只管理一个本地仓库和一个 Claude/Codex Pair
- Claude Code 在 stream-json 模式下，工作期间收到的新消息由官方 CLI 排队到后续 Turn；“打断”目前通过终止进程并在下一条消息恢复 Session 实现
- Codex 支持 `turn/steer`，因此工作期间的消息可以显示为 `injected`
- Codex 审批已映射到 UI；Claude 非交互审批 UI 尚未实现，当前由 `--claude-permission-mode` 和用户原有规则控制
- Codex Reviewer 使用 `readOnly` sandbox；Claude Reviewer 在 v0.1 主要依靠角色提示与用户权限配置，不构成强制文件系统隔离
- 还没有 Worktree 隔离、可视化文件树、语音、移动端原生客户端、多用户权限和远程 TLS
- 本环境未安装并登录真实的 `claude` / `codex`，所以仓库内完成了 Mock 端到端验证和协议单元测试，真实账号联调仍需要在安装了两个官方 CLI 的机器上执行

## 官方协议依据

PairRoom 的 Runtime Adapter 以供应商公开协议为准：

- Claude Code CLI reference: https://code.claude.com/docs/en/cli-reference
- Claude Code headless/streaming: https://code.claude.com/docs/en/headless
- Codex app-server: https://developers.openai.com/codex/app-server

## 开发

```bash
make fmt
make test
make vet
make build

# 数据竞争检测
make test-race

# 本地 Mock Demo
make demo
```

## License

MIT © 2026 Sean
