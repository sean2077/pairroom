# PairRoom 房间与运行时协议

## 1. Actor

```text
user      房间所有者，最高权威
claude    官方 Claude Code participant
codex     官方 Codex participant
system    PairRoom 状态通知
```

只有 `claude` 与 `codex` 可作为消息运行时目标。

## 2. 角色

```text
driver    可实现、修改、验证并报告证据
reviewer  独立检查，默认不得修改
peer      平级讨论；只有明确要求时实现
```

将某个 Participant 切换为 `driver` 时，另一个当前 driver 自动改为 reviewer，保证默认只有一个 Driver。

角色是两层约束：

- 传给 Agent 的协作规则。
- Codex 的 sandbox 选择；reviewer 为 read-only。

它不是通用 OS sandbox。Claude 的只读 Reviewer 在 v0.1 主要依赖 Claude Code 自身权限模式与提示约束。

## 3. Message

```json
{
  "id": "msg-...",
  "seq": 17,
  "from": "user",
  "to": ["claude", "codex"],
  "text": "@all 先讨论方案",
  "reply_to": "",
  "thread_id": "thread-...",
  "hop": 0,
  "turn_id": "",
  "created_at": "2026-08-13T10:00:00Z",
  "delivery": {
    "claude": "started",
    "codex": "injected"
  }
}
```

### Target 解析

优先级：

1. API 显式 `to`。
2. 消息中的 `@claude`、`@codex`、`@all`。
3. 用户未指定时默认发送给两方。

Agent 生成的回复不会使用用户默认规则，其自动目标由房间 routing mode 决定。

## 4. AgentInput Envelope

发给官方 Harness 的用户内容不是伪造 system message，而是结构化普通用户消息：

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

Room system prompt 说明 envelope 的语义、用户权威、角色和 @mention 规则。供应商自身的项目配置与安全政策仍然优先。

## 5. DeliveryState

| 状态 | 语义 |
|---|---|
| `pending` | 消息已持久化，尚未调用目标运行时 |
| `started` | 目标创建了新 Turn |
| `injected` | 消息进入正在执行的 Turn |
| `queued` | 运行时或 PairRoom 在安全边界排队 |
| `failed` | 未成功提交；detail 给出错误 |
| `skipped` | 被用户新消息、hop 上限或路由策略阻止 |

DeliveryState 说明“输入如何进入 Harness”，不代表 Agent 已同意、完成或正确执行。

## 6. Routing

### Manual

Agent 最终回复不会自动触发另一个 Agent。用户始终可手工 `@claude`/`@codex`。

### Mentions

Agent 最终回复中出现以下显式 mention 时才触发：

```text
@claude
@codex
@peer
@all
```

`@human` / `@user` 只表示等待用户，不触发 Agent。

### Roundtable

默认将 Agent 最终回复发给 Peer。以下任一条件会停止：

- 达到 `max_agent_hops`。
- Agent 输出 stop control marker。
- Agent 请求 `@human`。
- 该 Turn 开始后出现了更新的用户消息。
- 参与者/运行时不可用或提交失败。

## 7. Roundtable 控制标记

只有 standalone marker 被识别，随后从公共文本隐藏：

```text
[PAIRROOM:CONTINUE]
[PAIRROOM:CONSENSUS]
[PAIRROOM:WAIT]
[PAIRROOM:BLOCKED]
[PAIRROOM:DONE]
```

`CONTINUE` 允许接力；其他四个停止。即使没有 marker，Roundtable 也会在安全约束下继续，因此 SystemPrompt 要求 Agent 在达成结论时显式停止。

## 8. 用户抢占规则

每个 Agent Turn 关联原始输入 message sequence。处理 final 时比较最新 human message sequence：

```text
latest_human_seq > source_message_seq
```

若成立：

- Agent final 仍然写入公共房间。
- 不继续自动转发给 Peer。
- 产生 skipped delivery/system notice，指出新用户指令已抢占旧接力。

这避免用户纠偏后，旧 Turn 仍把过时计划传播给另一 Agent。

## 9. Canonical RuntimeEvent

```text
session
state
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

所有 vendor-specific payload 可附加在 `data`，公共 UI 只依赖 canonical kind。

## 10. Event Store

每个状态变化先 append，再 publish。事件 envelope：

```json
{
  "seq": 42,
  "id": "evt-...",
  "room_id": "room-...",
  "kind": "message.delivery.updated",
  "actor": "system",
  "created_at": "2026-08-13T10:00:01Z",
  "data": {}
}
```

主要 domain event：

```text
room.created
room.settings.updated
participant.updated
message.created
message.delivery.updated
runtime.event
approval.updated
system.notice
```

## 11. HTTP API

```text
GET  /api/v1/health
GET  /api/v1/snapshot
GET  /api/v1/events                 SSE
POST /api/v1/messages
PUT  /api/v1/settings
POST /api/v1/participants/{actor}/{start|stop|restart|interrupt}
PUT  /api/v1/participants/{actor}/role
POST /api/v1/approvals/{id}
GET  /api/v1/git/status
GET  /api/v1/git/diff?staged=1
```

### 发送消息

```json
POST /api/v1/messages
{
  "text": "@all 请先讨论方案",
  "to": ["claude", "codex"],
  "reply_to": ""
}
```

返回 `202 Accepted` 表示消息已经持久化并开始异步投递，不表示模型 Turn 已经完成。

### SSE

```text
GET /api/v1/events?since=<last-seq>
```

事件名固定为 `pairroom`，SSE `id` 等于 domain sequence。客户端应以 sequence 去重并在断线后接续。

## 12. Approval

Codex 支持：

```text
accept
acceptForSession
decline
cancel
```

命令/文件审批返回 decision；permissions approval 仅回授 App Server 请求的精确权限对象。未知或未实现的 server request 不会被自动同意。
