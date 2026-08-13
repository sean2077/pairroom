# PairRoom 架构设计

## 1. 架构原则

PairRoom 采用“薄协作控制面 + 官方原生运行时”架构：

```text
Browser
  │ REST + SSE
  ▼
PairRoom Server
  ├── Room Engine
  │   ├── shared message timeline
  │   ├── routing / role policy
  │   ├── delivery + processing lifecycle
  │   └── stale-handoff / hop / stall guards
  ├── Event Store (append-only JSONL)
  ├── Git Inspector
  └── Runtime Adapters
      ├── runtime probe / capability negotiation
      ├── Claude Code stream-json
      ├── Codex app-server JSON-RPC
      └── deterministic Mock
```

PairRoom 不进入模型推理链，不执行 Agent 工具，也不把一种供应商协议转译成另一种模型 API。Adapter 只负责：

1. 探测、启动和恢复官方 Harness。
2. 提交结构化房间消息。
3. 将官方事件归一化成 `RuntimeEvent`。
4. 处理中断、停止及供应商明确支持的审批。

## 2. 模块边界

### `internal/model`

稳定的数据契约：

- Actor、ParticipantRole、AgentState。
- Message、DeliveryState、ProcessingState。
- RoomSettings、RoomSnapshot、RuntimeInfo。
- RuntimeEvent、Approval、append-only Event envelope。

该包不依赖 server、room 或具体 Agent。

### `internal/store`

JSONL 事件存储：

- 每行一个完整 JSON event。
- 单调递增 sequence。
- append 后 `fsync`，再发布到内存总线。
- 打开时修复仅位于文件尾部的半行 JSON；中间损坏仍是硬错误。
- `metadata.json` 记录格式、schema 和应用版本。
- 旧 schema 可前向升级；高于当前实现的 schema 安全拒绝。

事件存储是房间持久状态的事实源；`RoomSnapshot` 是事件重放得到的投影。

### `internal/bus`

进程内 pub/sub，用于把已持久化事件发送给 SSE 客户端。慢订阅者不会阻塞房间写路径；断线后浏览器可凭 sequence 从 snapshot tail 接续。

### `internal/room`

核心状态机：

- 创建/恢复房间。
- 接收用户消息并解析目标。
- 异步投递，避免 HTTP send 等待 CLI 启动或模型网络。
- 分离 transport delivery 与 runtime processing。
- 接收 Runtime final，形成公共 Agent 消息。
- 根据 Manual/Mentions/Roundtable 决定是否触发 Peer。
- 控制最大 hop、停止标记和用户新指令优先级。
- 重试、停止、重启、审批失效和无事件提醒。
- 持久化角色、设置、session/thread、消息生命周期和审计事件。

### `internal/prompt`

只定义协作增量，不替换供应商系统提示：

- 房间规则与用户最高权威。
- 结构化 AgentInput envelope。
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

Adapter 不能直接修改 RoomSnapshot，只能产生 `RuntimeEvent`；Room Engine 统一持久化和投影。

`ProbeRuntime` 是无副作用能力探测层：检查可执行文件、版本、必要协议入口与可选参数，不登录、不创建会话、不读取仓库。

### `internal/server`

- Go `net/http` REST API。
- SSE 增量事件流。
- 内嵌静态 SPA。
- Git status/diff 只读接口。
- Transcript export。
- Token、同源、Host 检查、CSP、安全响应头和请求体限制。

## 3. Claude Adapter

典型启动形态：

```text
claude -p
  --input-format stream-json
  --output-format stream-json
  [--verbose]
  [--include-partial-messages]
  [--replay-user-messages]
  [--forward-subagent-text]
  [--include-hook-events]
  [--append-system-prompt-file <pairroom-prompt>]
  [--permission-mode <configured>]
  [--session-id <uuid> | --resume <uuid>]
```

方括号中的参数由当前 CLI `--help` 协商，而不是无条件发送。

关键点：

- 一个长驻进程接收多条 stream-json 用户输入。
- 输入按到达顺序排队，PairRoom 为每条输入维护独立 correlation 与 processing 状态。
- `stream_event` 文本增量是 transient SSE；`result` 是 durable lifecycle/final 事件。
- 官方 session ID 持久化，支持时用 `--resume` 恢复。
- CLI 不支持可选功能时降级运行并在 RuntimeInfo 中警告。
- `Interrupt` 终止当前 Claude 进程；未完成输入由适配器标记取消，下次输入可恢复 session。
- Claude 交互审批尚未映射为 PairRoom Approval。

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

- stdio 每行一个 JSON-RPC/notification 对象。
- thread ID 持久化并恢复。
- 空闲时 `turn/start`；active turn 时优先 `turn/steer`。
- App Server 拒绝 steer 时保留消息并在下一安全 Turn 边界启动。
- `turn/started` 可能早于 `turn/start` response；`startingInput` 解决该顺序竞态。
- 同一 active Turn 可以包含多个房间输入；Turn 结束时所有关联输入都会被 settle，最终回答关联到最新输入。
- 命令、文件和追加权限请求转换为 PairRoom Approval。
- 未明确支持的 server request fail closed。
- Reviewer 使用 read-only sandbox；Driver/Peer 使用配置 sandbox。
- `turn/start` 与 `turn/steer` 使用公开的 `clientUserMessageId`，并读取 `userMessage.clientId` 做精确关联；内部队列仍作为旧版本/异常事件的回退。

## 5. 两层消息生命周期

### DeliveryState：输入如何进入 Harness

```text
pending → started | injected | queued | failed | skipped
```

Delivery 是 transport disposition，不表示任务已经完成。

### ProcessingState：进入 Harness 后发生了什么

```text
waiting → working → completed | cancelled | failed
```

终态不可被迟到的 started/working 事件覆盖。RuntimeError 只改变 processing，不覆盖已经成功的 delivery。

重试不会回写旧消息，而是创建：

```text
new_message.retry_of = old_message.id
```

这保留了完整审计链。

## 6. 公共消息与运行事件分离

```text
RuntimeTextDelta / Tool / Command / Diff / Plan
                         │
                         └── Work Inspector

RuntimeFinal
     │
     └── MessageCreated ── Shared Room
```

高频文本和命令增量只实时发布，不逐 token `fsync`。会影响可恢复状态的事件仍必须持久化后再发布。

## 7. 路由状态机

### 用户消息

```text
User send
  → persist MessageCreated
  → resolve targets
  → async Submit(target)
  → persist delivery disposition
  → project processing lifecycle
```

### Agent 最终回复

```text
RuntimeFinal
  → persist public MessageCreated
  → strip hidden control marker
  → evaluate routing mode
  → stale human message guard
  → hop budget guard
  → optional async Submit(peer)
```

当 Turn 源消息之后出现了更新用户消息，旧 final 仍显示，但自动 Peer handoff 被阻止。

## 8. 并发模型

- RoomSnapshot 由 `RWMutex` 保护。
- Event Store 在单锁内分配 sequence、append、sync。
- 用户 Send 只同步完成持久化，Adapter Submit 异步执行。
- 每个 Adapter 串行化进程启动和协议写入。
- Codex 额外使用 submit mutex，避免两个 goroutine 同时观察 idle 并发起两个 `turn/start`。
- delivery/processing 的 transition 验证与 append 在同一房间锁下完成，防止快错误被迟到 Submit 结果覆盖。
- SSE 先订阅再读取 snapshot tail；sequence 消除重放重复。

## 9. 故障与恢复

| 故障 | 行为 |
|---|---|
| PairRoom 写到半行后退出 | 下次打开截断损坏尾部再追加 |
| 事件日志中间损坏 | 启动失败，不静默丢数据 |
| Store schema 比应用新 | 安全拒绝启动 |
| Adapter Submit 失败 | delivery=`failed`，processing=`failed`，公共消息保留 |
| Agent 进程异常退出 | outstanding processing 被 settle，Participant 进入 error/stopped |
| PairRoom 重启 | pending delivery skipped；waiting/working processing cancelled；审批过期 |
| 浏览器 SSE 丢序 | 前端重新获取 snapshot 并重连 |
| 工作 Agent 长时间无事件 | 一次性 warning，不自动宣告失败 |
| 未知 Codex server request | fail closed |
| Agent 无限互答 | hop 上限、停止标记和用户抢占终止 |

## 10. 独立性

PairRoom 源码不引用第三方 Agent 编排项目；Go 核心没有第三方 module。运行时外部依赖只有用户主动安装的 Git、官方 Claude Code CLI、官方 Codex CLI/App Server 和浏览器。
