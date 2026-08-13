# PairRoom

PairRoom 是一个面向 **Claude Code + Codex** 的本地三方协作房间：你、Claude Code 和 Codex 出现在同一条 IM 式时间线中，两个 Agent 保留各自官方 CLI 的 Harness，并按房间策略互相讨论、独立审查和接力工作。

> PairRoom 不依赖 OMA、CCCC、ccteam、wmux、Cherry Studio、Chatbox 或其他 Agent 编排框架。原生模式只启动用户本机已有的官方 `claude` 与 `codex` 命令；Go 核心没有第三方 module，前端没有 npm 依赖和构建步骤。

当前版本：**v0.2.0**

## 为什么需要 PairRoom

普通“双模型聊天”主要利用模型差异，通常无法完整保留 Claude Code 与 Codex 各自的：

- Agent loop、上下文管理与原生会话恢复
- 项目级 `CLAUDE.md` / `AGENTS.md`
- Skills、MCP、Hooks 与原生工具
- 文件编辑、命令执行、沙箱和审批语义
- 子 Agent、计划、Diff、用量等运行时事件

PairRoom 不重写这些能力，而是在两套官方 Harness 之上增加一层很薄的协作与可观察性控制面：

```text
┌──────────────────────── PairRoom Web UI ────────────────────────┐
│ Shared Room: You · Claude Code · Codex                          │
│ @mention · reply · search · retry · interrupt · role switch     │
│                                                                  │
│ Inspector: tools · commands · plans · diffs · usage · approvals │
└───────────────────────────┬─────────────────────────────────────┘
                            │ REST + SSE
┌───────────────────────────▼─────────────────────────────────────┐
│ PairRoom daemon                                                  │
│ event log · room state · routing · lifecycle · capability probe │
└───────────────────┬───────────────────────────┬─────────────────┘
                    │                           │
        Claude Code stream-json          Codex app-server
                    │                           │
             official `claude`             official `codex`
```

## v0.2.0 已实现

### 三方 IM 房间

- 你、Claude Code、Codex 共用一条持久化时间线
- `@claude`、`@codex`、`@all`、`@peer` 路由
- 引用回复、消息搜索、深浅主题和响应式布局
- 每条消息可直接筛选其关联的 Turn、工具、命令和运行事件
- Markdown / JSON 会话导出
- 用户新消息优先于尚未完成的旧 Agent 自动接力

### 两层消息状态

PairRoom 将“消息进入 Harness”与“Agent 实际处理结果”明确分开。

投递状态：

| 状态 | 含义 |
|---|---|
| `pending` | 消息已经持久化，等待提交 |
| `started` | 新建原生 Turn |
| `injected` | 注入正在执行的 Codex Turn |
| `queued` | 等待安全 Turn 边界 |
| `failed` | 没有成功提交给运行时 |
| `skipped` | 已持久化，但在进入运行时前因重启或显式策略作废 |

处理状态：

| 状态 | 含义 |
|---|---|
| `waiting` | 已排队，尚未开始执行 |
| `working` | 原生运行时正在处理 |
| `completed` | 运行时确认处理完成 |
| `cancelled` | 被中断、停止或异常重启取消 |
| `failed` | 输入未开始或执行过程中失败；结合投递状态判断是否曾进入 Harness |
| `superseded` | 被明确的新指令取代；预留给后续编辑/替换工作流 |

失败或取消的目标可以一键重试。重试会创建一条新的、带 `retry_of` 引用的消息，不会篡改历史记录。

### 两套官方 Runtime

- **Claude Code**：双向 `stream-json` 长会话、流式文本、工具事件、会话恢复、可选子 Agent 文本和 Hook 事件
- **Codex**：`app-server` JSON-RPC、thread 恢复、`turn/start`、`turn/steer`、`turn/interrupt`、命令/文件/权限审批、计划、Diff 和用量事件
- 运行前自动探测命令、版本、协议入口与可选能力
- Claude 可选参数按当前 CLI 的 `--help` 自动协商，旧版本不会因为未知的非必要参数直接启动失败
- UI 展示实际 Runtime 版本、协议、能力和诊断警告
- 不接管供应商登录、API Key、模型配置、Skills、MCP、Hooks 或项目说明文件

### 协作控制

三种路由模式：

- `manual`：Agent 回答不会自动转给 Peer
- `mentions`：只有显式 `@peer` / `@claude` / `@codex` 才触发 Peer；默认推荐
- `roundtable`：双方自动往返，但受 hop budget、停止标记和用户插话约束

三种角色：

- `Driver`：负责实现和验证
- `Reviewer`：负责独立审查
- `Peer`：平级讨论

支持启动、停止、重启和打断单个 Agent，以及 Driver/Reviewer 快速切换。

### Work Inspector

- Agent 状态、Session/Thread、当前 Turn
- 流式文本与公共最终回复分层展示
- 工具调用、命令输出、计划、Diff、用量、错误与日志
- 按 Agent 或消息/Turn 筛选工作过程
- 当前 Git 状态与 staged/unstaged Diff
- Codex 命令、文件修改和追加权限审批
- 工作 Agent 长时间没有运行事件时给出提醒，而不是直接误判为失败

### 本地可靠性与安全

- `events.jsonl` append-only 事件日志；每个领域状态变化 `fsync` 后才发布
- 崩溃留下的半行 JSON 会在下次启动时安全截断，随后可继续追加
- metadata schema 版本检查：旧 schema 自动升级，未来 schema 安全拒绝
- 重启后恢复消息、设置、角色和供应商 Session/Thread ID
- 重启、停止或运行时退出时，遗留的 processing 与审批会被明确取消/过期，避免“幽灵进行中”
- SSE 序列号去重，前端发现事件缺口会自动重新同步 snapshot
- 默认仅绑定 loopback；无 Token 时拒绝非 loopback Host，降低 DNS rebinding 风险
- 无数据库、无 Node.js 运行时、无前端构建步骤

## 快速开始

### 1. 先体验 Mock 模式

不需要安装或登录任何 Agent：

```bash
go run ./cmd/pairroom serve --repo . --mock
```

浏览器会打开 `http://127.0.0.1:7332`。Mock Agent 会模拟三方讨论、接力、运行过程和完成状态。

### 2. 检查真实环境

```bash
go run ./cmd/pairroom doctor --repo /path/to/project
```

机器可读报告：

```bash
pairroom doctor --repo /path/to/project --json
```

需要：

- Go 1.23+（只有从源码构建 PairRoom 时需要）
- Git
- 官方 Claude Code CLI，命令为 `claude`
- 官方 Codex CLI，命令为 `codex`，且包含 `codex app-server`
- 两个 CLI 已按官方方式完成登录

`doctor` 会检查真实可执行文件路径、版本、协议入口和可选能力，而不创建供应商会话，也不读取仓库内容。

### 3. 启动真实 Pair

```bash
go run ./cmd/pairroom serve --repo /path/to/project
```

或构建单文件二进制：

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

# 只有显式 @Peer 时继续（默认）
pairroom serve --repo . --routing mentions

# 自动轮桌讨论，最多 8 次 Agent-to-Agent 接力
pairroom serve --repo . --routing roundtable --max-hops 8

# 完全手动路由
pairroom serve --repo . --routing manual

# 5 分钟没有运行事件时提醒；-1 关闭
pairroom serve --repo . --stall-warning-seconds 300

# 可选：覆盖供应商 CLI 的模型选择
pairroom serve --repo . \
  --claude-model your-claude-model \
  --codex-model your-codex-model \
  --codex-effort high

# 指定独立状态目录
pairroom serve --repo . --data-dir ./.pairroom-local

# 自定义 CLI 包装命令
pairroom doctor --repo . \
  --claude-command /path/to/claude-wrapper \
  --codex-command /path/to/codex-wrapper
```

查看完整参数：

```bash
pairroom serve -help
pairroom doctor -help
```

## 配置文件

复制 [`examples/pairroom.example.json`](examples/pairroom.example.json)：

```json
{
  "listen": "127.0.0.1:7332",
  "room_name": "Claude × Codex",
  "routing_mode": "mentions",
  "max_agent_hops": 6,
  "stall_warning_seconds": 300,
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

## 推荐使用方式

默认保持一个写入者、一个只读审查者：

```text
Claude Code  Driver
Codex        Reviewer
```

第一条消息可以是：

```text
@all 请分别独立理解仓库和任务。Claude 提出实现方案，Codex 检查并发、兼容性和测试遗漏；没有达成共识前不要修改代码。
```

开始实现和审查：

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

标记不会显示在公共聊天。`CONSENSUS`、`WAIT`、`BLOCKED`、`DONE` 会停止自动接力；达到 hop budget 或出现更新的用户消息也会停止。

## 数据目录与导出

默认状态目录按仓库绝对路径计算，位于用户配置目录：

```text
pairroom/rooms/<repo-name>-<path-hash>/
├── events.jsonl
├── metadata.json
└── runtime/
    └── claude-pairroom-prompt.md
```

普通 JSON/Markdown 导出只包含房间状态和讨论，不默认包含可能含有大量命令输出、路径和工具参数的 Inspector event tail。完整取证 JSON 可调用：

```text
GET /api/v1/export?format=json&include_events=1
```

PairRoom 不保存供应商 API Key；Claude Code 与 Codex 的身份和凭据仍由官方 CLI 管理。

## 设计边界

PairRoom **负责**：

- 房间消息、路由、角色、投递与处理状态、用户介入
- Claude/Codex 结构化协议适配和能力探测
- Canonical runtime event、事件持久化、审批和 Web UI

PairRoom **不负责**：

- 重写 Agent loop 或工具执行器
- 统一模型 API 或转发用户订阅凭据
- 替代 `CLAUDE.md`、`AGENTS.md`、Skills、MCP、Hooks
- 自动判断代码是否真正正确
- 在两个 Agent 同时写同一 working tree 时解决语义冲突

详细设计：

- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- [`docs/PROTOCOL.md`](docs/PROTOCOL.md)
- [`docs/PRODUCT_PLAN.md`](docs/PRODUCT_PLAN.md)
- [`docs/VALIDATION.md`](docs/VALIDATION.md)
- [`docs/RUNTIME_COMPATIBILITY.md`](docs/RUNTIME_COMPATIBILITY.md)
- [`docs/UPGRADING.md`](docs/UPGRADING.md)
- [`SECURITY.md`](SECURITY.md)

## 当前限制

- 一个 PairRoom 进程只管理一个本地仓库和一个 Claude/Codex Pair
- Claude Code stream-json 当前不提供与 Codex `turn/steer` 完全相同的主动 Turn 注入语义；工作期间的新消息由长会话按原生输入顺序处理
- Codex 审批已映射到 UI；Claude 非交互审批仍由 Claude Code permission mode 和用户现有规则控制
- Codex Reviewer 使用 read-only sandbox；Claude Reviewer 仍不是操作系统级只读隔离
- 尚未提供自动 Worktree 隔离、多房间、可视化文件树、原生移动客户端、多用户权限和内建 TLS
- 当前执行环境没有安装并登录真实 `claude` / `codex`，因此本发布完成了 Mock 端到端、协议单元测试和跨平台构建；真实账号联调仍应先在非关键仓库进行

## 开发

```bash
make fmt
make test
make race
make vet
make build
make release

# 本地 Mock Demo
make demo
```

## License

MIT © 2026 Sean
