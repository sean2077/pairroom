# PairRoom HTTP API 参考

PairRoom 有两个独立的本地 API 面：

- **Management API**：Service 级 Project、Room 与 Runtime 管理；
- **Room API**：单个 active Room 的消息、附件、审批、参与者与 SSE。

两者使用不同的 server、token/session scope。API 仅绑定数字 loopback；远程使用通过 SSH 本地端口转发。

## 认证

非浏览器客户端使用：

```http
Authorization: Bearer <token>
```

浏览器先用 fragment 中的 bootstrap token 调用 `POST /api/v1/session`，换取 HttpOnly Session 与 CSRF token。Session 认证下的 mutating request 必须发送 `X-PairRoom-CSRF`；Bearer 客户端不使用浏览器 CSRF。Query-string token 被拒绝。

Management Session 不能用于 Room API，Room Session 也不能用于 Management API。

## Management API

| Method | Path | 作用 |
|---|---|---|
| `POST` | `/api/v1/session` | Bearer bootstrap 创建浏览器 Session |
| `GET` | `/api/v1/session` | 读取当前 Session / CSRF 状态 |
| `DELETE` | `/api/v1/session` | 注销浏览器 Session |
| `GET` | `/api/v1/service` | Service snapshot：Project、Room、Runtime、policy、summary、capabilities |
| `POST` | `/api/v1/projects` | 登记 Project，body `{ "path": "/abs/worktree" }` |
| `POST` | `/api/v1/projects/{project}/refresh` | 重新解析 Project availability / canonical root |
| `DELETE` | `/api/v1/projects/{project}` | 注销空 Project，body 必须精确确认 `confirm_project_id` |
| `POST` | `/api/v1/projects/{project}/rooms` | 创建 Room，body 含 `name` 与两个 `bindings` |
| `POST` | `/api/v1/rooms/{room}/activate` | 请求 Room Runtime；可能返回 queued 或 active |
| `POST` | `/api/v1/rooms/{room}/suspend` | 在安全边界挂起 Runtime |
| `POST` | `/api/v1/rooms/{room}/bindings` | 为 pending Room 补全 Binding |
| `PATCH` | `/api/v1/rooms/{room}` | Rename，body `{ "name": "..." }` |
| `POST` | `/api/v1/rooms/{room}/archive` | interrupt active Turn、settle、suspend 后归档 |
| `POST` | `/api/v1/rooms/batch-archive` | body `{ "room_ids": [...] }`，最多 100 |
| `POST` | `/api/v1/rooms/{room}/restore` | 恢复 archived lifecycle |
| `DELETE` | `/api/v1/rooms/{room}` | 永久删除 archived Room，要求 `acknowledge_data_loss: true` |
| `POST` | `/api/v1/rooms/batch-delete` | 批量永久删除，要求 `room_ids` 与 data-loss 确认 |
| `POST` | `/api/v1/maintenance/room-deletions/retry` | 重试 committed deletion 的物理清理 |
| `POST` | `/api/v1/import` | 非破坏性导入 legacy Room，body `{ "path": "/abs/room" }` |

### Binding body

Room provisioning / completion 使用：

```json
{
  "name": "Example Room",
  "bindings": {
    "claude": { "mode": "new" },
    "codex": { "mode": "existing", "session_id": "..." }
  }
}
```

字段名以 `internal/service/types.go` 的 `BindingSpec` 为准。Existing Binding 会在发布 Room 前精确验证；失败不产生可见半成品。

### Batch 结果

Batch archive/delete 逐项返回 `status`、Room/Removal 或结构化 `error` / `code`。重复 ID 被计入 `duplicates_ignored`；调用者必须检查每项，不能只看 HTTP 200。

## Room API

| Method | Path | 作用 |
|---|---|---|
| `POST` | `/api/v1/session` | 创建 Room-scoped 浏览器 Session |
| `GET` | `/api/v1/session` | 读取 Room Session |
| `DELETE` | `/api/v1/session` | 删除 Room Session |
| `GET` | `/api/v1/health` | 版本、commit、build date 与当前时间 |
| `GET` | `/api/v1/snapshot` | 当前 Room projection；`message_limit` 可限制消息窗口 |
| `GET` | `/api/v1/messages` | 历史分页；`before_seq`、`limit` |
| `GET` | `/api/v1/events` | SSE；`since` 是 durable sequence cursor |
| `POST` | `/api/v1/messages` | 发送 `room.SendRequest`；返回 `202 Accepted` 不代表完成 |
| `POST` | `/api/v1/attachments` | multipart `file` 上传图片 |
| `GET` | `/api/v1/attachments/{id}` | 读取经 hash/metadata 验证的附件 |
| `DELETE` | `/api/v1/attachments/{id}` | 删除尚未进入 durable Transcript 的附件 |
| `POST` | `/api/v1/messages/{id}/retry` | 创建引用旧消息的新 Retry |
| `POST` | `/api/v1/messages/{id}/cancel` | 取消指定消息/目标；可能扩大到 active native Turn |
| `GET` | `/api/v1/export` | `format=markdown|json`；JSON 可用 `include_events=1` |
| `PUT` | `/api/v1/settings` | 更新 `RoomSettings`；routing 只接受 `turns` |
| `POST` | `/api/v1/participants/{actor}/{action}` | `start|stop|restart|interrupt` |
| `PUT` | `/api/v1/participants/{actor}/role` | 设置 `driver|reviewer|peer` |
| `POST` | `/api/v1/approvals/{id}` | 提交 `ApprovalResolution` |
| `GET` | `/api/v1/git/status` | `git status --short --branch` |
| `GET` | `/api/v1/git/diff` | unstaged diff；`staged=1` 查看 staged diff |

## Message request

`POST /api/v1/messages` 使用 `internal/room.SendRequest`。常见字段包括：

```json
{
  "text": "...",
  "to": ["claude"],
  "target_role": "driver",
  "reply_to": "msg-...",
  "intent": "append",
  "attachments": [
    {
      "id": "att-...",
      "name": "diagram.png",
      "media_type": "image/png",
      "kind": "image",
      "size": 12345,
      "sha256": "..."
    }
  ]
}
```

`attachments` 使用上传接口返回的完整 attachment metadata，不是仅传 ID。目标必须解析为单个 participant；`to` 与 `target_role` 不应解析出相互冲突的目标，多个 recipient 被拒绝。客户端应继续观察 Message Processing / Turn terminal，而不是把 `202` 当作执行成功。

其他 mutation body：

| Endpoint | Body |
|---|---|
| Retry | 可省略；或 `{ "to": ["codex"] }` 重新选择单一目标 |
| Cancel | `{ "target": "claude" }`，目标必须是原消息的 participant |
| Settings | `{ "routing_mode": "turns", "max_agent_hops": 6, "stall_warning_seconds": 300 }` |
| Participant role | `{ "role": "driver|reviewer|peer" }` |
| Approval | `{ "decision": "...", "message": "...", "answers": { "question": "answer" } }`；具体 decision 集合由对应 native request 决定 |

## SSE

`GET /api/v1/events?since=N` 先订阅再 snapshot，避免 snapshot/subscription handoff 丢事件；durable sequence 去重。`Seq == 0` 的 transient telemetry 会实时发送但不会推进 durable cursor。连接每 20 秒发送 heartbeat comment。

断线恢复使用：

1. 重新获取 snapshot；
2. 以上次 durable sequence 重连 SSE；
3. 忽略重复 durable event；
4. 不假设 transient delta 会重放。

## 错误处理

参数与业务前置条件通常返回 `400` / `409`，认证返回 `401` / `403`，Vendor/Runtime 边界可能返回 `502` / `503`。Destructive operation 应读取结构化 error code，尤其是 active Room deletion、busy suspend、Binding ownership 与 cleanup uncertain。

实现事实源：`internal/service/management.go` 与 `internal/server/server.go`。
