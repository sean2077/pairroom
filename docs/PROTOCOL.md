# PairRoom Room 协议设计

> [文档首页](README.md) · [核心概念](CONCEPTS.md) · [架构](ARCHITECTURE.md) · [Multi-Room Service](MULTI_ROOM_SERVICE.md)

本文描述当前 Room Runtime 的消息、附件、工作区、运行时、审批、Room 浏览器会话和持久化语义。Service Management API、Project/Room lifecycle 与 Management Bearer/browser-session 认证见 [`MULTI_ROOM_SERVICE.md`](MULTI_ROOM_SERVICE.md) 和 [`MANAGEMENT_SHELL.md`](MANAGEMENT_SHELL.md)。

## 1. Actor

```text
user
claude
codex
system
```

`claude` 与 `codex` 是唯一可执行参与者。`user` 的指令权威高于自动 Agent 接力。

## 2. Message

核心字段：

```json
{
  "id": "msg-...",
  "seq": 42,
  "thread_id": "thread-...",
  "hop": 0,
  "from": "user",
  "to": ["claude", "codex"],
  "text": "@all review this diagram",
  "reply_to": "msg-...",
  "retry_of": "msg-...",
  "intent": "append",
  "supersedes": ["msg-..."],
  "attachments": [],
  "delivery": {},
  "processing": {},
  "created_at": "..."
}
```

### Thread

- 新的顶层用户消息创建新 `thread_id`；
- 引用回复继承被回复消息的 thread；
- Agent 自动接力继承源消息 thread；
- UI 可聚焦一个 thread，但底层公共时间线保持完整。

### Reply

`reply_to` 是展示与上下文引用，不意味着只有被回复者收到。实际目标由 `to` 决定。

### Retry

重试创建新消息并设置 `retry_of`。原消息永不修改，也不会重新执行未被选中的目标。

### Intent and supersede

用户消息可声明：

```text
append       尽可能补充当前协作过程
next_turn    不注入 active Turn，在下一个安全边界处理
supersede    取消/替代目标当前未完成指令
```

`supersede` 会创建新消息，并在 `supersedes` 中保存受影响消息 ID；旧记录保留但 Processing 进入 `superseded`，其迟到结果不能继续自动唤醒 Peer。

## 3. Attachment

持久化消息只保存：

```json
{
  "id": "att-...",
  "name": "diagram.png",
  "media_type": "image/png",
  "kind": "image",
  "size": 2831,
  "sha256": "...",
  "width": 640,
  "height": 360,
  "source": "user-upload",
  "created_at": "..."
}
```

绝对路径不进入 Message、Event、Snapshot 或 transcript。

内部 Adapter 边界使用：

```go
type AgentAttachment struct {
    Attachment
    Path string `json:"-"`
}
```

限制：

```text
formats: PNG/JPEG/GIF/WebP
max image: 5 MiB
max images/message: 8
max total/message: 20 MiB
max side: 8000 px
max pixels: 64 MP
```

同一附件 ID 在消息内去重。

## 4. Delivery lifecycle

Delivery 描述消息如何进入 Harness：

```text
pending → started
pending → injected
pending → queued
pending → failed
pending → skipped
```

含义：

- `started`：新原生 Turn；
- `injected`：Codex `turn/steer` 成功；
- `queued`：Claude 或内部安全队列等待下一个 Turn；
- `failed`：没有提交给 Harness；
- `skipped`：重启或策略在提交前使消息作废。

Transport 接受后发生的模型/工具错误不会反向把 `started/injected/queued` 改成 `failed`。

## 5. Processing lifecycle

Processing 描述 Harness 接受后的处理：

```text
waiting → working → completed
waiting → cancelled / failed / superseded
working → cancelled / failed / superseded
```

每个目标独立记录：

```text
state
detail
turn_id
last_updated_at
```

同一个 Codex Turn 可包含多次 `turn/steer`；每条 PairRoom 输入仍有独立 Processing 结算。

## 6. Rich text

Message text 使用 Markdown-like 安全子集。UI 不接受原始 HTML 执行。

支持：

```text
headings
paragraphs / line breaks
blockquote
ordered / unordered / task lists
table
fenced code
inline code / strong / em / strike
http(s)/mailto links
mentions
image references
```

Markdown 图片只有在能匹配消息附件时才内联显示。远程 URL 显示占位和显式“打开链接”，不会自动请求。

## 7. Native image projection

### Claude

每个图片转成原生 content block：

```json
{
  "type": "image",
  "source": {
    "type": "base64",
    "media_type": "image/png",
    "data": "..."
  }
}
```

图片块位于文本块之前。文件在编码前重新检查大小与 SHA-256。

### Codex

每个图片转成 App Server 输入：

```json
{
  "type": "localImage",
  "path": "/host/local/path/to/att-....png"
}
```

这个路径只存在于 PairRoom → Codex 的本地进程边界。

## 8. Runtime correlation

PairRoom 将房间消息关联到原生执行：

```text
MessageID
ThreadID
TurnID
SessionID / Codex ThreadID
CorrelationID
```

RuntimeEvent 常见类型：

```text
runtime.info.updated
session.updated
input.waiting
input.processing
input.completed
input.cancelled
input.failed
turn.started
turn.completed
text.delta
final
command.*
tool.*
plan.*
diff.*
usage.updated
approval.requested
error
log
```

公共最终回答进入 Message；token delta、命令和工具细节留在 Inspector event tail。

## 9. Claude control protocol

Claude 进程使用 `--permission-prompt-tool stdio` 启动。随后 PairRoom 在发送第一条用户输入前写入：

```json
{
  "type": "control_request",
  "request_id": "claude-control-...",
  "request": {
    "subtype": "initialize",
    "hooks": null
  }
}
```

只有收到匹配的 success `control_response` 后，Adapter 才进入 Idle。初始化失败或超时会使 Runtime 启动失败，而不是在没有审批通道的情况下继续运行。

Claude 发出的：

```text
control_request / can_use_tool
```

被转换为 PairRoom Approval。未知 subtype 返回 control error。

## 10. Approval

```json
{
  "id": "approval-...",
  "agent": "claude",
  "kind": "claude.toolApproval",
  "title": "Approve Claude Bash",
  "detail": {},
  "status": "pending",
  "requested_at": "..."
}
```

Resolution：

```json
{
  "decision": "accept",
  "message": "optional",
  "answers": {
    "exact question": "selected answer"
  }
}
```

通用决策：

```text
accept
acceptForSession
decline
cancel
```

Claude `AskUserQuestion` 的 `answers` 必须覆盖请求中的问题。Codex 只接受原 request schema 允许的结果；额外权限只能授予请求子集。

审批与具体运行时连接绑定。以下事件使未决审批过期：

```text
interrupt
stop
restart
runtime exit
runtime error
PairRoom restart
```

## 11. Participant role

```text
driver
reviewer
peer
```

Role 更新先应用 Adapter，再持久化房间状态。

### Claude reviewer

```text
permission_mode = plan
disallowedTools = Edit, Write, NotebookEdit, ExitPlanMode
```

到达控制层的写请求再次自动拒绝。

### Codex reviewer

`turn/start` 使用：

```json
{"sandboxPolicy":{"type":"readOnly"}}
```

### Driver/Peer

使用用户配置的原生 permission/sandbox 策略。Codex 的 `thread/start.sandbox` 使用 kebab-case 枚举（`read-only`、`workspace-write`、`danger-full-access`），而 `turn/start.sandboxPolicy.type` 使用 App Server policy 对象的 camelCase 类型（`readOnly`、`workspaceWrite`、`dangerFullAccess`）；Adapter 在各自的 wire 边界做规范化。

## 12. Workspace boundary

Participant snapshot 公开：

```text
kind / path
source_head
patch_sha256
dirty / untracked_count
read_only / read_only_enforced
refreshed_at / warnings
```

Driver 使用 live repository。Reviewer 使用 detached Git worktree，再应用 `git diff HEAD` 并复制 untracked regular files。创建失败、unsafe symlink 或 patch 应用失败会阻止 Reviewer 启动，不会静默退回 live writable tree。

## 13. Durable Turn summary

高频 token/command delta 可以只作为 transient SSE，但每个原生 Turn 都维护有界、持久化的摘要：

```text
agent / turn / correlation message
started_at / completed_at / duration
status / final summary / error
tools / commands / plans / diffs / usage
```

该摘要支持重启后 Inspector 查看，同时避免把无限命令输出写入内存快照。

## 14. Room browser session

启用 Token 时的浏览器认证流程：

```text
URL fragment bootstrap token
        → POST /api/v1/session with Bearer
        → HttpOnly + SameSite=Strict session cookie
        → per-session CSRF token for mutations
```

fragment 在交换后立即从地址栏移除；Token 不进入 URL query 或 Web Storage。命令行 API 客户端可继续使用 Bearer Header。Query token 不授权任何 endpoint。

所有内置 Web listener 只接受数字 loopback 地址；远程浏览器通过 SSH 本地端口转发连接服务端 loopback listener。

## 15. Routing

### Manual

Agent final 只展示，不自动发送给 Peer。

### Mentions

Agent final 包含以下目标才自动路由：

```text
@claude
@codex
@peer
@all
```

### Roundtable

默认将 Agent final 发送给另一 Agent，除非：

- 达到 `max_agent_hops`；
- 出现更晚的用户消息；
- Agent 使用停止标记；
- 目标不可用；
- 输入提交失败。

停止标记：

```text
[PAIRROOM:CONTINUE]
[PAIRROOM:CONSENSUS]
[PAIRROOM:WAIT]
[PAIRROOM:BLOCKED]
[PAIRROOM:DONE]
```

标记从展示文本移除，但路由决策进入事件日志。

## 16. User precedence

每个自动 thread 记录最新用户序列。若旧 Agent Turn 在更新的用户消息之后才完成：

```text
final response is preserved
but automatic peer routing is skipped
```

这保证审计完整，同时防止过期方案继续扩散。

## 17. Persistence

Store schema 7 的事件日志包含：

```text
room.created
service.room.provisioned
service.room.bindings.completed
service.room.binding.materialized
settings.updated
participant.updated
participants.batch.updated
message.created
message.delivery.updated
message.processing.updated
approval.updated
runtime.event
turn.summary.updated
system.notice
```

附件二进制不内嵌 JSONL；Message 中的附件 ID 指向 media store。

重启时：

```text
pending delivery → skipped
waiting/working processing → cancelled
pending approval → expired
```

原生 Session/Thread ID 保留，下一次输入尝试恢复供应商上下文。

Service 的 `new` binding 在首个原生输入被接受前没有 durable Session/Thread ID。该输入被接受后，`service.room.binding.materialized` 以 System actor 记录唯一的 `(agent, vendor_session_id)`；重放时这个事件覆盖对应的 deferred binding。一个 binding 不得 materialize 两次，也不得替换现有 durable Identity。

## 18. Export

Markdown/普通 JSON：

- 包含完整公共讨论；
- 包含附件名称、格式、大小和 ID；
- 不包含附件绝对路径；
- 不包含 Inspector event tail。

取证 JSON：

```text
/api/v1/export?format=json&include_events=1
```

额外包含 bounded/runtime event 数据，敏感度更高。
