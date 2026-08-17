# Multi-Project / Multi-Room Service

> [文档首页](README.md) · [快速上手](GETTING_STARTED.md) · [核心概念](CONCEPTS.md) · [Management Shell](MANAGEMENT_SHELL.md) · [运维手册](OPERATIONS.md)

`pairroom service` 是当前推荐的日常入口：一个与当前工作目录无关的本地控制面管理多个 canonical Git Project、每个 Project 下的多个 durable Room，以及受容量约束、按需启动的 Room Runtime。

兼容命令 `pairroom serve --repo ...` 仍然存在，但它直接启动一个单 Room Runtime，不提供 Registry、Project/Room 管理或跨 Room 容量调度。

## 1. 最小启动

```bash
pairroom service
```

常见配置：

```bash
pairroom service \
  --listen 127.0.0.1:7332 \
  --runtime-limit 4 \
  --idle-timeout 20m \
  --shutdown-timeout 10m \
  --routing mentions
```

首次体验建议：

```bash
pairroom service --mock
```

Mock 使用同一 Management/Room/Store/Runtime 流程，但两个 Agent 为确定性实现，不要求 Vendor CLI 登录。

### 1.1 Listener

Service、兼容 `serve` 和所有 Room Runtime 只接受数字 loopback 地址。允许 `127.0.0.1`/`::1`，拒绝通配、LAN、公网、主机名和 `localhost`。远程使用通过 SSH 本地端口转发。

### 1.2 Data root

显式 `--data-root` 必须为绝对路径。省略时使用操作系统用户配置目录下的 PairRoom 根目录，因此从不同工作目录启动仍打开同一 Registry。

### 1.3 Management Token

未提供 `--token` 时 Service 生成随机 Bearer Token，并放入 Management 启动 URL fragment。Shell 读取后立即清除 fragment，用 Bearer 调用 `POST /api/v1/session`，然后使用 Service-scoped `HttpOnly`、`SameSite=Strict` Session Cookie；写操作还要求只保存在标签页内存中的 CSRF Token。刷新可从仍有效的 Cookie 恢复会话，Service 重启或会话过期后需要重新打开完整启动 URL。CLI/API 客户端可继续直接使用 Bearer Header。

Management Shell 与 Room View 的认证链路不同，详见 [安全策略](../SECURITY.md)。

## 2. 后台运行

`pairroom daemon` 把同一个 Service 安装到系统服务管理器：

```bash
pairroom daemon install --runtime-limit 4 --idle-timeout 20m
pairroom daemon open
pairroom daemon status
pairroom daemon logs -f
```

后台 Service 不主动打开浏览器。`pairroom daemon open` 从当前和轮转日志读取候选 Management URL，拒绝非数值 loopback、非 HTTP、缺少 fragment token 或无法认证当前 Service 的地址，然后才调用系统默认浏览器；bootstrap token 不写入可重建的 daemon metadata。

Linux 使用 systemd，macOS 使用 launchd，Windows 使用当前用户 Task Scheduler。安装会固定二进制路径、工作目录、PATH、代理环境、日志位置和完整 Service 参数，并自动加上后台所需的 `--no-browser` 与控制文件。

修改配置时必须重装完整定义：

```bash
pairroom daemon install --force -- \
  --data-root /absolute/path/to/pairroom-data \
  --runtime-limit 4 \
  --idle-timeout 20m
```

`daemon restart` 只重启已有定义。完整运维语义见 [运维手册](OPERATIONS.md)。

## 3. 管理信息架构

Management Shell 保留 Room View 作为实际协作界面，自身只负责控制面：

| 页面 | 用途 |
|---|---|
| Overview | Service 健康、Project/Room 计数、capacity、活动/排队/失败 Runtime、attention items |
| Projects | 跨 Project/Room 搜索、可用性筛选、登记 Project、Legacy Import |
| Project detail | 路径复检、空 Project 安全注销、创建 Room、打开、改名、归档、恢复、补全 Binding |
| Runtimes | phase、busy、capacity occupation、queue position、last used、错误、安全挂起 |
| Settings | 当前标签页界面偏好、有效 Runtime policy、daemon 指引、Service diagnostics、capabilities |

所有 mutation 使用带校验的表单/Dialog，不依赖 `window.prompt` 或 `window.confirm`。窄屏使用抽屉导航与卡片式 Runtime 行。

## 4. Project Identity

Project 代表一个 canonical Git worktree。登记时服务端执行：

1. 要求绝对路径；
2. 验证目录存在且可访问；
3. 解析符号链接；
4. 执行 `git rev-parse --show-toplevel`；
5. 再次 canonicalize；
6. 生成稳定 Project ID 并去重。

因此：

- 同一 worktree 的根目录、子目录与 symlink 只能登记一次；
- 两个 Git worktree 即使来自同一仓库，也是两个 Project；
- Service 不扫描用户目录；
- Management Shell 不提供服务端文件系统浏览器；
- Project 路径不可用时保留登记与 Room 历史，但阻止需要工作区的操作；
- 路径复检只更新 available/diagnostic，不静默迁移 stable Project identity；
- 只有没有 active 或 archived Room 的 Project 才能从 Registry 注销，且注销不删除 Git worktree。

## 5. Room 与 Binding

每个 Room：

- 永久属于一个 Project；
- 拥有独立 append-only Event Log 和附件库；
- 恰好绑定一个 Claude Session 与一个 Codex Thread；
- 有自己的角色、路由、消息、审批和 Runtime 生命周期。

### 5.1 四种创建组合

Claude 与 Codex 可分别选择：

```text
new
existing(<vendor-session-or-thread-id>)
```

因此支持 new/new、new/existing、existing/new、existing/existing。

### 5.2 全局唯一 ownership

Binding Identity：

```text
(agent, vendor_session_id)
```

在整个 Service 内全局唯一。归档 Room 仍保留 ownership，避免同一 Vendor context 被两个 PairRoom Room 并发解释或写入。

### 5.3 Existing Binding

Existing Binding 在创建过程中必须：

- 通过当前官方协议精确恢复；
- 属于正确 Agent 类型；
- 未被其他 Room 占用。

恢复只带回 Vendor 原生 context。PairRoom 不读取、复制、搜索或展示绑定前 Transcript；Room View 会标出 transcript boundary，PairRoom 时间线从成功绑定后开始。

### 5.4 Deferred New Binding

空 Vendor Session/Thread 在没有首个 Turn 时不一定具备可持久化身份，因此 `new` 初始记为 deferred。

首个真实输入流程：

1. Adapter 创建新原生会话；
2. 官方 CLI 接受 PairRoom 输入；
3. Engine 追加 `service.room.binding.materialized`；
4. Registry 建立全局 ownership/checkpoint；
5. Turn 才继续按正常事件投影。

Event append、唯一性或 checkpoint 失败会中断该执行并 fail closed。若 Service 在首个输入前退出，重启时可以重新创建空会话，不会错误恢复尚未 materialize 的临时 ID。

## 6. 原子 Provisioning

创建 Room 在隐藏暂存目录中完成：

```text
validate Project
  -> validate/probe both bindings
  -> create staged Room store
  -> append initial lifecycle events
  -> claim binding ownership
  -> atomic rename/publish
  -> checkpoint Registry
```

任一 Existing ID 无法精确恢复、任一 identity 已占用、协议验证失败或初始 Store 提交失败时：

- 不出现可见 Room；
- 不写入半成品 Binding 索引；
- 不留下可激活的数据目录。

## 7. Room Runtime

每个激活 Room 创建独立的：

- Event Store 与 Attachment Store；
- Workspace Manager；
- Engine 与 Event Hub；
- Room HTTP/SSE Server；
- ClaudeAdapter 与 CodexAdapter；
- loopback listener 与认证 Token。

这些 Runtime 是 Service 进程内隔离的逻辑单元；官方 `claude`/`codex app-server` 是各自子进程。

### 7.1 激活

打开 Room 或调用 activate 时：

- 已 active：返回现有 URL；
- 有空闲 slot：启动 Runtime；
- 可回收 idle Runtime：先安全挂起最久未使用者；
- 所有 slot busy：进入 FIFO queue。

切换浏览器标签页不会 stop 或 interrupt Agent。

### 7.2 精确恢复

Binding materialize 后，Runtime 再次激活会以同一 durable Claude Session/Codex Thread ID 精确 resume，而不是创建新 Binding。

### 7.3 Idle suspend

Room 空闲超过 `--idle-timeout` 后释放 Agent 子进程和 Runtime slot。Room Event Log、附件、Binding 与消息历史不被删除。浏览器草稿属于页面状态，不应被当作 Runtime 的 durable 数据。

## 8. Runtime Capacity

`--runtime-limit` 范围为 1–128，默认 2。实际合理值取决于机器资源、Vendor 并发限制和仓库规模。

容量状态：

| Phase | 通常占用 slot | 说明 |
|---|---:|---|
| starting | 是 | 正在构造 Runtime/Listener/Adapters |
| active | 是 | 可用，可能 idle 或 busy |
| stopping | 是 | 尚未证明 cleanup 完成 |
| failed + retained runtime | 是 | cleanup uncertain，不能假装释放 |
| queued | 否 | 等待 FIFO |
| suspended | 否 | durable Room 保留，运行资源已释放 |

`occupies_capacity` 是 API 的明确字段，不应仅从 phase 名称猜测。

### 8.1 安全挂起

```text
POST /api/v1/rooms/{room-id}/suspend
```

行为：

- queued：从 FIFO 移除并回到 suspended；
- active + idle：drain/stop；
- active + busy：`409 Conflict`，不 interrupt Turn；
- starting/stopping：根据当前状态冲突；
- failed + retained runtime：`409 Conflict`，提示 cleanup uncertain；
- unknown Room：`404 Not Found`。

Suspend 与 rename/archive/binding completion 使用同一 per-Room 控制锁，防止并发修改。

## 9. Room 生命周期

当前 durable 生命周期：

```text
create -> rename* -> archive <-> restore
```

- Rename 只改展示名称，不改 Room ID/Binding；
- Archive 前等待活动 Turn 自然结束并挂起 Runtime；
- Archive 不删除 Event Log、附件或 Binding ownership；
- Restore 保留完整历史与原绑定；
- 当前没有永久 Room deletion；
- 空 Project 可以安全从 Registry 注销；任何 active 或 archived Room 都会阻止该操作。

生命周期操作需要避免 Room Engine 与控制面同时成为 Event Log writer，因此会先进入安全 Runtime 边界。

## 10. 数据布局与 Registry 重建

```text
<pairroom-root>/
├── service.lock
├── service-registry.json
└── rooms/
    └── <room-id>/
        ├── events.jsonl
        ├── metadata.json
        ├── attachments/
        └── runtime/
```

`events.jsonl` 是每个 Room 的事实源；`service-registry.json` 是可替换 checkpoint/index。

checkpoint 删除或损坏后，Service 扫描默认 `rooms/`，从 Event Logs 重建：

- Project；
- Room lifecycle；
- Binding ownership；
- archived/pending 状态。

显式导入的自定义 Legacy 路径不在默认扫描边界内，需要再次导入。导入只重建索引，不修改原 Room Event Log。

Registry 无法证明内存索引、已提交 Event 与 checkpoint 一致时，会阻止后续 mutation。

Project 注销与 Room provisioning 使用同一全局 mutation 串行化边界。若 provisioning 先提交，注销返回 `409 project_has_rooms`；若注销先提交，后续 provisioning 看到 Project 不存在。这样不会产生孤儿 Room，也不会出现重启后被 Event Log 意外“复活”的已注销 Project。

## 11. Legacy Room

### 11.1 默认目录发现

Service 启动时只扫描默认 PairRoom Room 根，不全盘搜索，也不移动、复制或重写旧 Event Log。

Event Log 中已有的 Session/Thread ID 会登记为 durable Binding。缺任一侧 ID 的 Room 标记为 pending，暂不能激活；导入本身不主动证明 Vendor 端当前一定可恢复。

### 11.2 补全 Binding

Management Shell 提供一次性 completion：

- existing：先精确验证，再原子补全；
- new：记录 deferred，首个真实输入时 materialize；
- 已存在的 durable Binding 不能被替换。

成功只追加一个 lifecycle 事件，不重写旧历史。

### 11.3 自定义旧目录

旧 `serve --data-dir /custom/path` 不会被自动扫描。用户在 Management Shell 显式输入绝对路径导入；导入建立可重建索引，不搬迁数据。

## 12. Service snapshot

`GET /api/v1/service` 返回：

```json
{
  "version": "1.0.0",
  "commit": "...",
  "build_date": "...",
  "data_root": "/absolute/path",
  "generated_at": "...",
  "projects": [],
  "rooms": [],
  "runtimes": [],
  "runtime_policy": {},
  "summary": {},
  "capabilities": {},
  "healthy": true,
  "diagnostic": ""
}
```

### 12.1 Runtime policy

```json
{
  "limit": 2,
  "idle_timeout_seconds": 900,
  "poll_interval_milliseconds": 500,
  "close_timeout_seconds": 10
}
```

这些是当前进程有效值，不是可热修改设置。修改前台 Service 需重启；修改 daemon 需完整 `install --force`。

### 12.2 Summary

服务端聚合 Project/Room、pending bindings、capacity used、active/busy/queued/failed Runtime 和 attention items。它是派生观测值，不是新的持久化事实源。

### 12.3 Capabilities

典型字段：

```json
{
  "legacy_import": true,
  "runtime_suspend": true,
  "runtime_policy_mutation": false,
  "project_refresh": true,
  "project_removal": true,
  "room_deletion": false,
  "server_path_browser": false
}
```

`false` 表示当前产品契约不提供该操作，不应伪装成“保存成功”或简单归因于用户权限。

## 13. Management API 摘要

Management API 接受直接 Bearer 或有效 browser session。直接 API 客户端发送：

```http
Authorization: Bearer <management-token>
```

浏览器自动发送 HttpOnly Cookie；Session 认证的 mutation 还必须提供：

```http
X-PairRoom-CSRF: <session-csrf-token>
```

主要端点：

```text
POST   /api/v1/session
GET    /api/v1/session
DELETE /api/v1/session
GET    /api/v1/service
POST   /api/v1/projects
POST   /api/v1/projects/{project}/refresh
DELETE /api/v1/projects/{project}
POST   /api/v1/projects/{project}/rooms
POST   /api/v1/rooms/{room}/activate
POST   /api/v1/rooms/{room}/suspend
POST   /api/v1/rooms/{room}/bindings
PATCH  /api/v1/rooms/{room}
POST   /api/v1/rooms/{room}/archive
POST   /api/v1/rooms/{room}/restore
POST   /api/v1/import
```

- query token 被拒绝；
- browser session 的 mutation 需要 CSRF，直接 Bearer 客户端不需要该 Header；
- mutation 使用结构化错误与状态码；
- Project refresh 原子持久化当前可用性，不改变 canonical identity；
- Project DELETE 必须提交精确匹配 path ID 的 `confirm_project_id`，且只允许没有任何 Room 的 Project；
- 有 active/archived Room 时返回 `409 Conflict`、`code: project_has_rooms` 和有界 Room ID 诊断；
- busy/unsafe suspend 使用 `409 Conflict`；
- Room 激活结果包含独立 Room URL；
- Management API 不提供 Room transcript 或附件内容。

Room 的消息、附件、SSE 和浏览器 session 契约见 [Room 协议](PROTOCOL.md)。

## 14. 关闭与锁

收到 SIGINT/SIGTERM：

1. Management Server 停止接收新 mutation；
2. 等待 in-flight provisioning/lifecycle handler；
3. Runtime Manager 等待活动 Turn 并关闭各 Room；
4. Engine/Store 关闭；
5. 释放 `service.lock`。

一个 data root 只能有一个 Service。崩溃残留 lock 不自动判 stale；确认旧进程已退出后才使用：

```bash
pairroom service --recover-stale-lock
```

## 15. 当前边界

当前 Service 支持一个本机用户管理多个 Project/Room，但每个 Room 仍固定为：

```text
one canonical Git worktree
one human operator
one Claude participant
one Codex participant
one Driver + one Reviewer by default
```

没有多人身份、远程 worker、云同步、托管 TLS、Project worktree/Room 永久删除、Runtime policy 热修改或额外 Vendor 插件市场。空 Project 的 Registry 注销不属于数据删除。

Management Shell 的页面级行为、可访问性和测试契约见 [MANAGEMENT_SHELL.md](MANAGEMENT_SHELL.md)。cc-connect 的体验调研与取舍记录见 [CC_CONNECT_UX_RESEARCH.md](CC_CONNECT_UX_RESEARCH.md)。
