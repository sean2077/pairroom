# PairRoom Management Shell

Management Shell 是 `pairroom service` 的多 Project / 多 Room 本地控制面。它管理 Project、Room 生命周期和 Runtime 容量，但不替代 Room View，也不读取供应商原生 Transcript。

## 路由

Management Shell 使用 URL hash 进行无服务端路由：

| 路由 | 用途 |
|---|---|
| `#/overview` | Service 健康、容量、待处理项、活动 Runtime、Project 摘要 |
| `#/projects` | 搜索和过滤 Project/Room，登记 Project、导入 Legacy Room |
| `#/projects/<project-id>` | Project 详情、Room 创建、打开、改名、归档、恢复、补全 Binding |
| `#/runtimes` | Runtime phase、busy、容量占用、FIFO 队列、最近使用、错误和安全挂起 |
| `#/settings/interface` | 主题、密度、自动刷新和打开 Room 行为 |
| `#/settings/runtime` | 当前生效 Runtime policy 与启动参数指导 |
| `#/settings/daemon` | daemon status/logs/restart/reinstall/stale-lock 运维指导 |
| `#/settings/service` | 版本、数据根、健康状态和脱敏诊断导出 |
| `#/settings/boundaries` | Service capabilities 和不可用操作的安全原因 |

## 浏览器状态

以下纯界面偏好只保存在当前标签页内存中：

- theme；
- density；
- refresh interval；
- include archived；
- Project filter；
- search query；
- 当前路由。

Management Bearer Token 从 URL fragment 读取后立即从地址栏移除，只保留在当前页面内存。Management Shell 不使用 `localStorage` 或 `sessionStorage`，刷新后需要由完整启动 URL 重新 bootstrap。

## 自动刷新

- 默认每 10 秒读取一次 `GET /api/v1/service`；
- 允许在当前标签页切换 5/10/30/60 秒或关闭；
- 页面隐藏时暂停定时刷新；
- 不允许同一个 snapshot 请求重入；
- 网络失败时保留最后一份可用数据，显示连接横幅，并允许手动 Retry；
- 用户完成 mutation 后立即刷新 snapshot。

## Service snapshot

Management Shell 使用以下顶层字段：

```json
{
  "version": "dev",
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

### Runtime policy

```json
{
  "limit": 2,
  "idle_timeout_seconds": 900,
  "poll_interval_milliseconds": 500,
  "close_timeout_seconds": 10
}
```

这些值是当前进程的有效配置，不是可热修改的设置。UI 必须把它们表现为 read-only effective values。

### Summary

服务端聚合：

- Projects、unavailable Projects；
- Rooms、active Rooms、archived Rooms；
- pending bindings；
- capacity used、active/busy/queued/failed runtimes；
- attention items。

`attention_items` 是需要用户查看的聚合提示，不是新的持久化事实。当前由 unavailable Projects、pending bindings 和 failed runtimes 组成；queued runtimes 单独显示为容量等待状态，不计入阻断项。

### Capabilities

```json
{
  "legacy_import": true,
  "runtime_suspend": true,
  "runtime_policy_mutation": false,
  "project_removal": false,
  "room_deletion": false,
  "server_path_browser": false
}
```

Web Shell 根据 capability 决定是否展示可执行按钮。`false` 不应被解释为权限不足，而是当前产品契约不支持。

## Runtime 状态

`RuntimeStatus` 增加 `occupies_capacity`：

- `starting`、`active`、`stopping` 占用容量；
- cleanup 未能证明完成且仍持有 runtime 的 `failed` 状态占用容量；
- `queued`、`suspended` 不占用容量；
- 一个状态出现在列表中不等于它正在消耗 Runtime slot。

## 安全挂起

端点：

```text
POST /api/v1/rooms/{room-id}/suspend
```

行为：

- queued：从 FIFO 队列移除并回到 suspended；
- active + idle：进入 draining/stopping 并安全关闭；
- active + busy：返回 `409 Conflict`，不 interrupt Turn；
- starting/stopping：按 Runtime Manager 当前状态返回冲突或不可安全挂起；
- failed + retained runtime：返回 `409 Conflict` 和 cleanup uncertain，不释放容量也不假装成功；
- unknown Room：返回 `404 Not Found`。

Management handler 使用和 Room 生命周期操作相同的 per-Room mutex，避免 suspend 与 rename/archive/binding completion 并发修改同一 Room 控制状态。

## Project 与 Room 操作

### Register Project

只接受绝对路径。服务端负责：

1. 校验目录存在和可访问；
2. 解析符号链接；
3. 解析 Git worktree root；
4. canonicalize；
5. 去重。

页面不提供服务器目录浏览器。

### Create Room

Dialog 同时收集：

- Room name；
- Claude Binding：new 或 existing ID；
- Codex Binding：new 或 existing ID。

提交失败时保留表单内容并在 Dialog 中显示服务端错误。创建成功后导航至 Project 详情并突出新 Room。

### Lifecycle

- Rename、archive、restore 使用受控 Dialog；
- archive 前明确解释不会删除 Event Log、附件或 Binding；
- 没有永久删除操作；
- pending legacy bindings 使用专门的 completion Dialog，不允许替换已持久化的 Binding。

## Settings 设计边界

### Interface

仅作用于当前页面，不写入磁盘：

- system/light/dark；
- comfortable/compact；
- auto refresh；
- Room 在当前或新标签页打开。

### Runtime

显示 effective policy，并提供等价命令示例：

```bash
pairroom service --runtime-limit 2 --idle-timeout 15m
```

页面不调用不存在的 Runtime policy mutation API。

### Daemon

只读运维指引：

```bash
pairroom daemon status
pairroom daemon logs -f
pairroom daemon restart
```

修改已安装定义时，必须重新提供完整 Service 参数：

```bash
pairroom daemon install --force -- --runtime-limit 2 --idle-timeout 15m
```

`daemon restart` 只重启已安装定义，不接受新的 Runtime policy。使用 `--force` 重装时还必须保留原有 `--data-root`、`--listen`、`--token`、代理、日志、Agent 和路由参数；页面的示例不是可盲目覆盖生产定义的完整备份。

崩溃遗留 stale lock 只在确认旧进程已经退出后处理：

```bash
pairroom daemon start --recover-stale-lock
```

Management Shell 不提供 stop/restart 自身的按钮。

### Service diagnostics

页面可导出一份 JSON 快照，包含：

- build metadata；
- Project/Room/Runtime 的结构化状态；
- Runtime policy、summary、capabilities；
- registry diagnostic。

导出前会移除 Management token，并对 Runtime URL 进行脱敏。它不是 `pairroom diagnostics` Room 数据包的替代品，也不包含 Event Log 内容、消息文本、图片或 vendor transcript。

## 可访问性与响应式

- 使用语义化 `nav`、`main`、`dialog`、form label 和 button；
- Dialog 支持 Escape 关闭，并在打开后聚焦首个可操作控件；
- 状态不只依赖颜色，均有文字标签；
- `prefers-reduced-motion` 下关闭非必要动画；
- 移动端侧栏变为抽屉；
- Runtime 表格在窄屏变为带 `data-label` 的卡片行；
- 点击目标满足触摸尺寸要求；
- 长 canonical path、错误和 session ID 可换行，不扩大 viewport。

## 回归测试

Go tests 覆盖：

- policy 和 capacity observability；
- snapshot summary/policy/capabilities；
- busy Runtime 不可挂起，drain-aborted/cleanup-uncertain 控制状态同样保持 `409 Conflict`；
- queued Runtime 可安全取消；
- Management assets 必须包含新路由和 daemon 边界；
- assets 禁止 Web Storage、`window.prompt` 和 `window.confirm`。

`tools/visual_smoke.py` 使用 Chromium 和真实静态 assets、模拟 Management API 数据完成：

- desktop Overview/Projects/Project detail/Runtimes/Settings；
- Project 搜索和 Dialog；
- mobile navigation、Overview 和 Runtime 卡片；
- console/page error；
- viewport horizontal overflow。

该 smoke test 不启动真实 Claude Code 或 Codex，因此不能替代 vendor runtime E2E。
