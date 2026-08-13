# PairRoom 架构设计

## 1. 总览

PairRoom 采用“薄控制面 + 原生运行时”架构。

```text
Browser
  │ REST + SSE
  ▼
PairRoom Server
  ├── Room Engine
  │   ├── message/router
  │   ├── role/policy
  │   ├── delivery projection
  │   └── stale-handoff guard
  ├── Event Store (JSONL)
  ├── Git Inspector
  └── Runtime Adapters
      ├── Claude Code stream-json
      ├── Codex app-server JSON-RPC
      └── deterministic Mock
```

PairRoom 不进入模型推理链，不执行 Agent 的工具，也不把供应商输出转换成另一个模型协议。Adapter 只负责：

1. 启动和恢复官方 Harness。
2. 提交一条结构化 PairRoom 消息。
3. 将官方事件归一化为 `RuntimeEvent`。
4. 处理中断、停止和可支持的审批。

## 2. 模块边界

### `internal/model`

稳定的数据契约：

- Actor、角色与运行状态。
- Message、DeliveryState。
- RoomSettings、RoomSnapshot。
- RuntimeEvent、Approval。
- Append-only Event envelope。

该包不依赖 server、room 或具体 Agent。

### `internal/store`

JSONL 事件存储：

- 每行一个完整 JSON event。
- 分配单调递增 sequence。
- append 后 `Sync`，再发布到内存总线。
- 重放时允许忽略最后一条不完整尾行，用于进程被终止时恢复。
- metadata 记录 schema/version 信息。

事件存储是房间状态的事实源；`RoomSnapshot` 是重放得到的投影。

### `internal/bus`

进程内 pub/sub，用于把已持久化事件发送给 SSE 客户端。慢订阅者不会阻塞房间写路径；断线后可凭 sequence 从 snapshot tail 接续。

### `internal/room`

核心状态机：

- 创建/恢复房间。
- 接收用户消息并解析目标。
- 异步投递，避免 UI send 等待模型启动或网络。
- 接收 runtime final，形成公共 Agent 消息。
- 根据 Manual/Mentions/Roundtable 决定是否触发 Peer。
- 控制最大 hop、停止标记和用户新指令优先级。
- 持久化角色、设置、session/thread ID、审批和运行时事件。

### `internal/prompt`

只定义协作增量，不替换官方系统提示：

- Room 规则。
- 结构化消息 envelope。
- Driver/Reviewer/Peer 角色规则。
- @mention 和 Roundtable 控制标记。

### `internal/agent`

统一接口：

```go
type Adapter interface {
    Actor() model.ActorID
    Start(context.Context) error
    Submit(context.Context, model.AgentInput) (model.DeliveryState, error)
    Interrupt(context.Context) error
    Stop(context.Context) error
    ResolveApproval(context.Context, string, string) error
    State() model.AgentState
    SessionID() string
}
```

Adapter 不能直接写 RoomSnapshot。它只能产生 `RuntimeEvent`，由 Room Engine 统一持久化和投影。

### `internal/server`

- Go `net/http` REST API。
- SSE 增量事件流。
- 内嵌静态 SPA。
- Git status/diff 的只读接口。
- Token、同源检查、CSP、安全响应头和请求体大小限制。

## 3. Claude Adapter

启动形态：

```text
claude -p
  --input-format stream-json
  --output-format stream-json
  --verbose
  --include-partial-messages
  --replay-user-messages
  --forward-subagent-text
  --append-system-prompt-file <pairroom-prompt>
  --permission-mode <configured>
  --session-id <uuid> | --resume <uuid>
```

关键点：

- 使用一个长驻进程处理多条输入。
- 后续用户消息写入同一 stream-json stdin；忙碌时标记为 `queued`。
- `stream_event` 文本增量进入 Inspector，`result` 生成公共最终回复。
- 官方 session ID 持久化，进程重启后用 `--resume`。
- `Interrupt` 终止当前 Claude 进程；下次输入恢复同一 session。
- Claude Code 的交互审批目前没有映射为 PairRoom Approval，适配器返回 `ErrApprovalUnsupported`。

## 4. Codex Adapter

启动形态：

```text
codex app-server
```

握手与生命周期：

```text
initialize
initialized
thread/resume | thread/start
turn/start
turn/steer
turn/interrupt
```

关键点：

- stdio 上每行一个 JSON-RPC/notification 对象。
- thread ID 持久化并恢复。
- 空闲时 `turn/start`；存在 active turn 时优先 `turn/steer`，成功返回 `injected`。
- App Server 不能接收 steer 时进入本地队列，当前 Turn 完成后再 start。
- `turn/started` 可能早于 `turn/start` response；`startingInput` 用于解决事件顺序竞态。
- command/file/permissions approval 请求转换为 PairRoom Approval。
- 未明确支持的 server request fail closed，而不是自动批准。
- Reviewer 角色使用 read-only sandbox；Driver/Peer 使用配置的 sandbox。

## 5. 公共消息与运行时事件分离

PairRoom 不把全部模型流和命令输出塞进聊天：

```text
RuntimeTextDelta / RuntimeToolStarted / RuntimeDiffUpdated
                         │
                         └── Work Inspector

RuntimeFinal
     │
     └── MessageCreated ── Shared Room
```

这避免一个长命令输出破坏 IM 可读性，同时保留可审计性。

## 6. 路由状态机

### 用户消息

```text
User send
  → persist MessageCreated
  → resolve targets
  → async Submit to each target
  → persist per-target delivery state
```

### Agent 最终回复

```text
RuntimeFinal
  → persist public MessageCreated
  → strip hidden control marker
  → evaluate routing mode
  → stale human message guard
  → hop budget guard
  → optional async Submit to peer
```

在 Agent Turn 开始之后出现了更晚的用户消息时，该 Turn 的自动 Peer handoff 会被 `skipped`。最终回复仍显示在公共房间，只有陈旧接力被阻止。

## 7. 并发模型

- RoomSnapshot 由 `RWMutex` 保护。
- 用户发送只同步完成持久化，实际 Agent 提交异步执行。
- 每个 adapter 自己串行化 stdin/RPC 写入。
- Codex 使用 submit mutex 防止两个目标消息同时发起 `turn/start`。
- Event Store 在单锁内分配 sequence、append、sync。
- SSE 先订阅再取 snapshot tail，以减少 handoff 窗口；sequence 去重。

## 8. 故障与恢复

| 故障 | 行为 |
|---|---|
| PairRoom 被终止 | JSONL 最后一条若不完整，重放时忽略尾行 |
| Agent 进程退出 | Participant 进入 stopped/error；历史 session/thread 保留 |
| 提交失败 | 对目标写入 `failed`，公共消息不丢失 |
| 浏览器断线 | SSE 自动重连，以 latest sequence 接续 |
| 新用户指令到达 | 陈旧自动 Agent handoff 被跳过 |
| 未知 Codex server request | fail closed，并产生错误/日志事件 |
| Agent 无限互答 | hop 上限和控制标记终止 |

## 9. 独立性

PairRoom 源码不引用任何第三方编排项目；Go 核心没有第三方 module。运行时外部依赖只有用户主动安装的：

- Git（Inspector 使用）。
- 官方 Claude Code CLI。
- 官方 Codex CLI/App Server。
- 浏览器。

这些是被管理的终端工具，不是 PairRoom 的代码库依赖或服务端依赖。
