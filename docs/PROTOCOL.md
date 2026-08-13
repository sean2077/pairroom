# PairRoom 房间与运行时协议

## 1. Actor

```text
user      房间所有者，最高权威
claude    官方 Claude Code participant
codex     官方 Codex participant
system    PairRoom 状态通知
```

只有 `claude` 与 `codex` 可作为 Runtime target。

## 2. 角色

```text
driver    可实现、修改、验证并报告证据
reviewer  独立检查，默认不得修改
peer      平级讨论；只有明确要求时实现
```

将某一参与者切换为 Driver 时，另一当前 Driver 自动变成 Reviewer，默认保持单写入者。

角色同时影响：

- 传给 Agent 的协作规则。
- Codex 每个 Turn 的 sandbox；Reviewer 为 read-only。

角色不等价于通用 OS sandbox。Claude Reviewer 仍需保守权限或独立 checkout 才能获得强隔离。

## 3. Message

```json
{
  "id": "msg-...",
  "seq": 17,
  "from": "user",
  "to": ["claude", "codex"],
  "text": "@all 先讨论方案",
  "reply_to": "",
  "retry_of": "",
  "thread_id": "thread-...",
  "hop": 0,
  "turn_id": "",
  "created_at": "2026-08-13T10:00:00Z",
  "delivery": {
    "claude": "started",
    "codex": "injected"
  },
  "processing": {
    "claude": "working",
    "codex": "completed"
  },
  "processing_turn": {
    "claude": "turn-a",
    "codex": "turn-b"
  }
}
```

### Target 解析优先级

1. API 显式 `to`。
2. 文本中的 `@claude`、`@codex`、`@all`。
3. 用户未指定时默认双方。

Agent 回复不会使用用户默认规则；自动目标由 routing mode 决定。

## 4. AgentInput Envelope

发给官方 Harness 的内容是普通用户消息，不伪装成供应商 system message：

```text
[PairRoom message]
message_id: msg-...
thread_id: thread-...
hop: 1
from: Claude Code
to: Codex
reply_to: msg-...
current_role: reviewer
role_rule: ...
routing_mode: mentions
remaining_agent_hops: 5

--- message body ---
...
--- end message ---
```

Room 增量提示说明用户权威、角色和 @mention 规则；供应商自身安全政策和项目配置仍然优先。

## 5. DeliveryState

| 状态 | 语义 |
|---|---|
| `pending` | 已持久化，尚未调用 Runtime |
| `started` | 创建新 Turn |
| `injected` | 进入正在执行的 Turn |
| `queued` | 在 PairRoom/Runtime 安全边界排队 |
| `failed` | 未成功提交 |
| `skipped` | 消息已持久化，但在进入 Runtime 前因重启或显式策略而作废 |

Delivery 只说明输入如何进入 Harness。

## 6. ProcessingState

| 状态 | 语义 |
|---|---|
| `waiting` | 等待 Runtime 开始 |
| `working` | Runtime 已确认处理 |
| `completed` | Runtime 成功完成 |
| `cancelled` | 被中断、停止或恢复逻辑取消 |
| `failed` | 输入未开始或执行过程中失败；结合 DeliveryState 判断是否曾进入 Harness |
| `superseded` | 被明确替代；当前为协议保留终态 |

合法转移：

```text
empty/waiting → waiting | working | terminal
working       → working | terminal
terminal      → same terminal only
```

迟到的初始状态不能覆盖终态。

## 7. Retry

```text
POST /api/v1/messages/{id}/retry
{
  "to": ["codex"]
}
```

只有 delivery failed/skipped 或 processing failed/cancelled/superseded 的目标可重试。返回的是一条新消息；原消息不变。

## 8. Routing

### Manual

Agent final 不自动触发 Peer。

### Mentions

只有显式出现以下 mention 才触发：

```text
@claude
@codex
@peer
@all
```

`@human` / `@user` 表示等待用户，不触发 Agent。

### Roundtable

默认把 Agent final 发给 Peer，以下任一条件停止：

- 达到 `max_agent_hops`。
- Agent 输出 stop control marker。
- Agent 请求 `@human`。
- 源消息之后出现更新用户消息。
- Runtime 不可用或投递失败。

## 9. Roundtable 控制标记

只有独立一行 marker 被识别，并从公共文本隐藏：

```text
[PAIRROOM:CONTINUE]
[PAIRROOM:CONSENSUS]
[PAIRROOM:WAIT]
[PAIRROOM:BLOCKED]
[PAIRROOM:DONE]
```

`CONTINUE` 允许接力；其余停止。

## 10. 用户抢占

每个 Agent final 关联源消息 sequence：

```text
latest_human_seq > source_message_seq
```

成立时：

- final 仍写入公共房间。
- 不再自动转给 Peer。
- 产生 system notice 说明旧接力被更新用户指令取代。

## 11. Canonical RuntimeEvent

```text
session
runtime.info
state
input.processing
input.completed
input.cancelled
input.failed
turn.started
turn.completed
text.delta
tool.started
tool.completed
command.output
plan.updated
diff.updated
usage.updated
approval.requested
approval.resolved
final
log
error
```

Vendor payload 可附在 `data`；公共 UI 只依赖 canonical kind。

高频 `text.delta` / command progress 可作为 sequence=0 的 transient SSE；需要恢复的 lifecycle event 必须写入事件日志。

## 12. Domain Event

```text
room.created
room.settings.updated
participant.updated
message.created
message.delivery.updated
message.processing.updated
runtime.event
approval.updated
system.notice
```

Event envelope：

```json
{
  "seq": 42,
  "id": "evt-...",
  "room_id": "room-...",
  "kind": "message.processing.updated",
  "actor": "codex",
  "created_at": "2026-08-13T10:00:01Z",
  "data": {}
}
```

## 13. HTTP API

```text
GET  /api/v1/health
GET  /api/v1/snapshot
GET  /api/v1/events                           SSE
GET  /api/v1/export?format=markdown|json
POST /api/v1/messages
POST /api/v1/messages/{id}/retry
PUT  /api/v1/settings
POST /api/v1/participants/{actor}/{start|stop|restart|interrupt}
PUT  /api/v1/participants/{actor}/role
POST /api/v1/approvals/{id}
GET  /api/v1/git/status
GET  /api/v1/git/diff?staged=1
```

`POST /messages` 返回 `202 Accepted` 只表示消息已持久化并开始异步投递，不表示 Agent 已完成。

### SSE

```text
GET /api/v1/events?since=<last-seq>
```

事件名固定为 `pairroom`。durable event 的 SSE `id` 等于 domain sequence；sequence=0 的 transient event 不设置 SSE id。

## 14. Approval

Codex UI 支持：

```text
accept
acceptForSession
decline
cancel
```

Permissions approval 只能回授 App Server 原请求中的权限子集。未知/未实现 server request 不会自动同意。

## 15. Store metadata

```json
{
  "format": "pairroom-jsonl",
  "schema_version": 2,
  "app_version": "0.2.0"
}
```

读取规则：

- 旧 schema：执行当前兼容迁移并重写 metadata。
- 当前 schema：直接打开。
- 未来 schema：拒绝，避免旧程序破坏新格式。
