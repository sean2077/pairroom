# PairRoom Management Shell

> [文档首页](README.md) · [快速上手](GETTING_STARTED.md) · [Multi-Room Service](MULTI_ROOM_SERVICE.md) · [运维手册](OPERATIONS.md) · [排障](TROUBLESHOOTING.md)

Management Shell 是 `pairroom service` 的本地控制面。它管理 Project、Room 生命周期与 Runtime 容量；实际对话、附件、审批和 Inspector 位于每个 Room 自己的 Room View。

它不读取或导入 Vendor 原生 Transcript，也不把多个 Room 的消息合并到管理页面。

## 1. 打开与认证

Service 启动时输出：

```text
PairRoom Service <version>
  management: http://127.0.0.1:7332/#token=...
  data root:  ...
```

Shell 的认证流程：

1. 从 URL fragment 读取 Management Token；
2. 立即从地址栏移除 fragment；
3. 用 `Authorization: Bearer ...` 调用 `POST /api/v1/session`；
4. 浏览器收到 Service-scoped `HttpOnly`、`SameSite=Strict` Session Cookie 和内存中的 CSRF Token；
5. 后续管理请求使用 Cookie，mutation 额外发送 `X-PairRoom-CSRF`。

bootstrap Token、Session ID 和 CSRF 不写入 `localStorage` 或 `sessionStorage`。刷新时页面会用 Cookie 调用 `GET /api/v1/session` 恢复 CSRF；Service 重启、会话过期、注销或新浏览器上下文需要重新打开完整启动 URL。CLI/API 客户端仍可直接使用 Bearer Header。这与 Room View 使用不同 listener、Token 和 Cookie 作用域的 browser session 链路不同。

## 2. 页面路由

Shell 使用 hash 路由，不要求服务端 route fallback：

| 路由 | 用途 |
|---|---|
| `#/overview` | 健康、容量、attention、活动 Runtime、Project 摘要 |
| `#/projects` | 搜索/过滤 Project 与 Room、登记 Project、导入 Legacy Room |
| `#/projects/<project-id>` | Project 详情、路径复检、安全注销、创建/打开/改名/归档/恢复 Room、补全 Binding |
| `#/runtimes` | phase、busy、capacity、queue、last used、错误、安全挂起 |
| `#/settings/interface` | 当前标签页 theme、density、refresh、打开 Room 行为 |
| `#/settings/runtime` | 有效 Runtime policy 和等价命令 |
| `#/settings/daemon` | status/logs/restart/reinstall/stale-lock 指引 |
| `#/settings/service` | build、data root、健康、脱敏 diagnostics export |
| `#/settings/boundaries` | capabilities 与不可用操作的原因 |

未知/缺失路由回到安全默认页面；路由本身不携带认证 Token。

## 3. 首次工作流

### 3.1 登记 Project

在 Projects 选择 Register Project，输入 Git worktree 的绝对路径。

服务端负责：

```text
absolute check
  -> directory/access check
  -> symlink resolution
  -> git worktree root
  -> canonicalize
  -> deduplicate
```

页面不提供服务器路径浏览器，也不会自动扫描目录。提交失败时保留表单输入并显示结构化错误。

### 3.2 创建 Room

在 Project detail 选择 Create Room，填写：

- Room name；
- Claude Binding：new 或 existing ID；
- Codex Binding：new 或 existing ID。

创建成功后回到 Project detail 并突出新 Room。Existing Binding 会在可见 Room 发布前精确验证；New Binding 可能在首个真实输入时 materialize。

### 3.3 打开 Room

Open Room 会请求激活 Runtime：

- 已 active：直接打开现有 URL；
- 有容量：启动后打开；
- 容量满且全部 busy：进入 queue，并在 Runtimes 显示位置；
- Project/Binding/Store 不可用：显示失败原因，不打开伪 Room。

可选择当前标签页或新标签页打开。Room 使用独立 listener 和 Token。

## 4. Overview

Overview 用于回答四个问题：

1. Service 是否健康；
2. 管理了多少 Project/Room；
3. Runtime capacity 是否紧张；
4. 是否有 unavailable Project、pending Binding 或 failed Runtime 需要处理。

### 4.1 Summary

服务端聚合：

- Projects / unavailable Projects；
- Rooms / active / archived；
- pending bindings；
- capacity used；
- active / busy / queued / failed Runtime；
- attention items。

`attention_items` 是派生提示，不是新的 Event Log 事实。Queued Runtime 单独显示容量等待，不默认当作阻断故障。

### 4.2 健康与 diagnostic

`healthy` 表示 Service snapshot 能否证明关键 Registry/Runtime 状态可用。`diagnostic` 应展示为可复制、可换行的受控文本；不要把其中路径或错误原样贴到公开 Issue。

## 5. Projects 页面

支持：

- 按 Project/Room 名称或路径搜索；
- 按 available/unavailable 与 archived 状态筛选；
- 登记 Project；
- 显式导入 Legacy Room；
- 重新检查 canonical path 的当前可用性；
- 导航到 Project detail，并在 Project 无任何 Room 时安全注销登记。

Project unavailable 不会删除其 Room 或 Binding。页面应解释不可用路径，并允许用户修复文件系统后刷新，而不是提供危险的自动迁移。

## 6. Project detail

展示 canonical root、Project 状态和所有 Room。

### 6.1 Rename

受控 Dialog 修改 display name。Room ID、Event Log 目录和 Binding 不变。若 Runtime busy，生命周期操作等待安全边界或返回明确冲突。

### 6.2 Archive / Restore

Archive Dialog 必须明确：

- 不删除 Event Log；
- 不删除附件；
- 不释放 Binding ownership；
- 会先等待 Turn 并挂起 Runtime。

Restore 恢复可见和可激活状态，并沿用原历史与 Binding。

### 6.3 Binding completion

Legacy Room 缺少一侧身份时显示 pending。Completion Dialog 只允许为缺失侧选择 existing 或 deferred new；已经 durable 的 Binding 不可替换。

### 6.4 Project refresh / unregister

`POST /api/v1/projects/{project-id}/refresh` 重新解析已登记 canonical root，并把 available/diagnostic 投影原子写回 Registry checkpoint。路径恢复后可重新变为 available；路径现在指向不同 canonical identity 时只标记 unavailable，不会静默迁移 Project 或改写 Room ownership。

`DELETE /api/v1/projects/{project-id}` 只注销**没有任何 Room**的 Project。请求体必须逐字确认完整 Project ID：

```json
{
  "confirm_project_id": "project-..."
}
```

安全边界：

- active 与 archived Room 都会阻止注销并返回 `409 project_has_rooms`；
- UI 在已知有 Room 时禁用按钮，后端仍在事务边界重新检查，覆盖并发创建 Room 的竞争；
- 成功只从 Service Registry 移除登记，不删除 Git worktree、Room 目录、Event Log、附件或 Vendor Session/Thread；
- 删除 checkpoint 持久化失败时回滚内存索引；若 checkpoint 已替换但目录同步失败，Registry fail closed；
- 注销后可再次登记同一 worktree。

注销 Dialog 要求输入完整 Project ID，不能用名称、路径尾段或原生 `window.confirm` 代替。

## 7. Runtimes 页面

每行/卡片展示：

- Room/Project；
- phase；
- busy；
- `occupies_capacity`；
- queue position；
- last used；
- Room URL 是否可用；
- error 或 cleanup 状态；
- 可用时的 Suspend 操作。

### 7.1 不要只看 phase 猜容量

- starting、active、stopping 通常占用；
- retained failed Runtime 仍占用；
- queued、suspended 不占用。

UI 以服务端 `occupies_capacity` 为准。

### 7.2 安全挂起

`POST /api/v1/rooms/{room-id}/suspend`：

| 状态 | 结果 |
|---|---|
| queued | 从 FIFO 取消，回到 suspended |
| active + idle | drain 并关闭 |
| active + busy | `409 Conflict`，不 interrupt |
| starting/stopping | 冲突或等待当前转换 |
| failed + retained | `409 Conflict`，cleanup uncertain |
| unknown | `404 Not Found` |

Shell 不提供“强制释放 slot”按钮，因为无法证明 cleanup 的实例不能被安全遗忘。

## 8. Settings

### 8.1 Interface

仅当前标签页内存：

- system/light/dark；
- comfortable/compact；
- 5/10/30/60 秒 refresh 或关闭；
- include archived；
- Room 在当前/新标签页打开。

这些设置刷新后重置是设计行为。

### 8.2 Runtime

展示 read-only effective policy：

```json
{
  "limit": 2,
  "idle_timeout_seconds": 900,
  "poll_interval_milliseconds": 500,
  "close_timeout_seconds": 10
}
```

并给出等价命令：

```bash
pairroom service --runtime-limit 2 --idle-timeout 15m
```

当前没有 Runtime policy mutation API，页面不能显示“保存成功”。前台需重启，daemon 需完整重装定义。

### 8.3 Daemon

只读指引：

```bash
pairroom daemon open
pairroom daemon status
pairroom daemon logs -f
pairroom daemon restart
```

`daemon open` 从受保护的当前/轮转日志解析 Management URL，只接受包含 bootstrap token 的数值 loopback HTTP 地址，并通过当前 Service 的 `GET /api/v1/service` 验证后才打开默认浏览器；它不会把 token 复制到 daemon metadata。

修改策略：

```bash
pairroom daemon install --force -- \
  --data-root /absolute/path/to/pairroom-data \
  --runtime-limit 2 \
  --idle-timeout 15m
```

`restart` 不接受新参数。重装必须保留原有完整 listen/token/proxy/log/Agent/routing 配置。

Stale lock 仅在确认旧进程退出后：

```bash
pairroom daemon start --recover-stale-lock
```

Shell 不提供 stop/restart 自身按钮，避免控制面在回答前终止自己。

### 8.4 Service

显示：

- version/commit/build date；
- data root；
- generated at；
- healthy/diagnostic；
- Runtime policy；
- Registry 与能力摘要。

可导出脱敏 JSON snapshot。导出移除 Management Token，并对 Runtime URL 脱敏；不包含 Room Event Log、消息、附件或 Vendor Transcript。

### 8.5 Boundaries

展示 capabilities：

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

`false` 是产品契约，不是需要隐藏的错误。页面应解释安全/一致性原因。

## 9. 自动刷新与连接状态

- 默认每 10 秒读取 `GET /api/v1/service`；
- 允许 5/10/30/60 秒或关闭；
- 页面隐藏时暂停；
- 同一 snapshot 请求不重入；
- mutation 成功后立即刷新；
- 网络失败保留最后可用 snapshot，并显示连接横幅；
- Retry 由用户显式触发；
- 恢复连接后不能把旧错误状态悄悄当作最新事实。

如果刷新导致 401，通常是 Service 重启、Session 过期、注销或浏览器已丢失 Cookie；运行 `pairroom daemon open`（已安装 daemon）或重新打开 Service 打印的完整 URL，而不是把 Token 放到 query string。

## 10. Service snapshot contract

```json
{
  "version": "1.0.0",
  "commit": "...",
  "build_date": "...",
  "data_root": "/absolute/path",
  "generated_at": "2026-08-16T00:00:00Z",
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

Shell 应把缺失/新增字段当作版本化 API contract 处理，不能通过 DOM 文案猜后端状态。

## 11. Dialog 与错误处理

有副作用的操作统一使用语义化 Dialog：

- 有 label、description 与可见 validation；
- 打开后聚焦首个可操作控件；
- Escape 可关闭非进行中操作；
- 提交期间禁用重复提交；
- 服务端错误保留用户输入；
- destructive/lifecycle 操作明确说明“不删除什么”；
- 高风险注销要求输入完整 durable ID，并在输入精确匹配前禁用提交；
- 不使用 native prompt/confirm；
- 成功通过页面状态与 toast 双重反馈。

HTTP status 只是传输层线索；页面应展示服务端安全原因，例如 busy、pending binding、project unavailable、cleanup uncertain。

## 12. 可访问性与响应式

- 使用语义化 `nav`、`main`、`dialog`、form label 和 button；
- 状态同时提供文本，不只依赖颜色；
- `prefers-reduced-motion` 关闭非必要动画；
- 移动端侧栏变为抽屉；
- Runtime 表格在窄屏变为带 `data-label` 的卡片；
- 点击目标满足触摸尺寸；
- canonical path、错误和 Session ID 可换行；
- viewport 不应产生全局横向滚动；
- focus 在 Dialog 打开/关闭后保持可预测。

## 13. 浏览器状态边界

Shell 禁止：

- Web Storage 保存 bootstrap Token、Session ID 或 CSRF；
- 把 Room 的 Cookie、CSRF 或 Token 当作 Management auth；
- query-string token；
- 自动扫描服务端路径；
- 浏览器端捏造 Runtime policy；
- 为不支持的 capability 展示可执行按钮；
- 在页面内永久删除 Room、Project worktree、Event Log、附件或 Vendor context；空 Project 的 Registry 注销是单独、受限且显式确认的操作。

## 14. 回归验证

Go tests 覆盖：

- policy/capacity observability；
- snapshot summary/policy/capabilities；
- busy/cleanup-uncertain suspend 冲突；
- queued cancel；
- lifecycle 控制并发；
- 空 Project 注销跨重启持久化且不触碰 worktree；
- active/archived Room 阻止注销，并返回结构化冲突；
- Project 注销与 Room provisioning 串行化；
- path refresh 的 unavailable/recovery 持久化；
- assets 包含路由/daemon/Project 管理边界；
- 禁止 Web Storage、`window.prompt`、`window.confirm`。

`tools/visual_smoke.py` 使用 Chromium、真实静态 assets 和模拟 Management API 数据验证 desktop/mobile 路由、Dialog、console/page error 与横向溢出。它不启动真实 Claude/Codex，不能当作 Vendor E2E。

## 15. 常见问题

### 刷新后全部请求 401

Management Session 只在当前 Service 进程内有效，并按请求滑动续期；Service 重启、会话过期、注销或 Cookie 被清理后，重新打开完整启动 URL。已安装 daemon 先运行 `pairroom daemon open`。

### Open Room 一直排队

到 Runtimes 查看 capacity、busy 和 queue position。等待 Turn 完成或安全挂起其他 idle Runtime；不要强杀 Agent。

### Project 显示 unavailable

检查 canonical worktree 是否移动、卸载或权限变化。恢复原路径后选择“重新检查”；该操作只更新可用性诊断，不迁移 Project identity。若 worktree 已永久迁移，需要登记新的 canonical worktree，现有 Room 不会自动改属。

### 看不到注销按钮，或按钮不可用

Project 注销只在 `project_removal` capability 为 true 且 Project 不含任何 Room 时可执行。active 和 archived Room 都属于 durable 数据，会阻止注销；Archive 是可逆生命周期，不是删除。当前仍不支持永久 Room deletion，也不删除 Project worktree。

更多症状见 [排障手册](TROUBLESHOOTING.md)。
