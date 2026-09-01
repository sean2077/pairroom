# PairRoom 核心概念

> [文档首页](README.md) · [快速上手](GETTING_STARTED.md) · [Multi-Room Service](MULTI_ROOM_SERVICE.md) · [架构](ARCHITECTURE.md)

PairRoom 的 UI 同时出现 Project、Room、Binding、Runtime、Turn、Delivery 和 Processing。它们分别解决“代码在哪里”“讨论属于谁”“供应商上下文是谁”“进程是否正在运行”“一次模型工作是什么”“输入是否送达”“工作是否完成”这些不同问题。

## 一张心智模型图

```text
PairRoom Service                     durable control plane
└── Project                          one canonical Git worktree
    ├── Room A                       durable collaboration history
    │   ├── Claude Binding           one vendor Session identity
    │   ├── Codex Binding            one vendor Thread identity
    │   └── Room Runtime             ephemeral active processes/web server
    │       ├── Claude Adapter ─── official claude
    │       └── Codex Adapter  ─── official codex app-server
    └── Room B
        └── ...
```

Service、Project、Room 和 Binding 是长期控制状态；Runtime 与 Vendor 进程可以因容量、空闲、重启而消失和恢复。

## Project

Project 代表一个 **canonical Git worktree root**。

登记时 Service：

1. 要求绝对路径；
2. 检查目录可访问；
3. 解析符号链接；
4. 用 Git 定位 worktree root；
5. 再次 canonicalize 并去重。

Project 不是模糊的“项目名称”，也不是当前进程工作目录。一个 worktree 的根目录、子目录和等价 symlink 不会创建多个 Project。

当前没有 Project 删除操作。路径暂时不可用时，Project 会保留并显示诊断；修复路径后可以继续使用。

## Room

Room 是一个 Project 内的持久化协作单元。它拥有：

- 一条 PairRoom 公共时间线；
- append-only Event Log；
- 附件与 Room metadata；
- 一个 Claude Binding；
- 一个 Codex Binding；
- 角色、路由、消息状态、审批投影和 Turn 摘要。

Room 永久属于创建时的 Project，不能跨 Project 移动。多个 Room 可以针对同一仓库承载不同任务、上下文或审查周期。

Room 生命周期：

```text
create → active ⇄ archived
```

归档不会删除 Event Log、附件或 Binding；恢复后继续使用同一历史和身份。当前没有永久删除 Room 的操作。

## Binding

Binding 是 Room 与供应商原生会话身份的持久关系：

```text
Claude Binding = Claude Session ID
Codex Binding  = Codex Thread ID
```

### `existing`

创建 Room 时提供已有 ID。Provisioning 必须精确恢复该 Session/Thread 并验证可用；失败时不发布半成品 Room。

恢复的是供应商内部 context，不是 PairRoom Transcript。PairRoom 不读取、导入或显示绑定前的 Vendor Transcript。

### `new`

空 Vendor 会话通常在没有首个用户 Turn 时还没有可持久化身份，因此 `new` 先记录为 deferred Binding。第一个输入被官方 CLI 接受后，PairRoom 才将 Session/Thread ID materialize 到 Room Event Log 与 Registry。

如果 Event append、唯一性或 Registry checkpoint 失败，执行会 fail closed，而不是继续运行一个无法可靠归属的会话。

### 唯一性

同一个 `(agent, vendor_session_id)` 在整个 Service 内只能属于一个 Room，归档 Room 也继续持有它。这样可以防止两个 Room 同时恢复和写入同一个供应商上下文。

## Runtime

Room Runtime 是 Room 激活时创建的一组临时对象和进程：

- Event/Attachment Store 句柄；
- Workspace Manager；
- Room Engine；
- Room REST/SSE Web server；
- Claude Adapter/进程；
- Codex Adapter/进程。

Runtime 可被挂起并按相同 Binding 恢复；挂起不会删除 Room 历史。

常见 phase：

| Phase | 含义 | 通常占用容量 |
|---|---|---|
| `suspended` | 未运行 | 否 |
| `queued` | 等待全局容量 | 否 |
| `starting` | 正在创建 | 是 |
| `active` | 可用或正在工作 | 是 |
| `stopping` | 正在安全关闭 | 是 |
| `failed` | 启动/清理失败 | 视是否仍保留实例而定 |

`occupies_capacity` 才是容量事实；不要仅凭 phase 名称推断 slot 是否释放。

## Runtime 容量

`pairroom service` 有全局 `--runtime-limit`。激活新 Room 时：

1. 有空 slot：直接启动；
2. 容量满但存在最久未使用的 idle Runtime：先安全挂起它；
3. 所有 slot 都在忙：新 Room 进入 FIFO 队列；
4. 活动 Turn 不会为了释放容量而被中断。

超过 `--idle-timeout` 的空闲 Runtime 会被挂起。切换浏览器页面不会自动停止 Room。

## Management Shell 与 Room View

它们是两个不同的界面和安全边界。

### Management Shell

负责：

- 登记 Project；
- 创建、打开、改名、归档和恢复 Room；
- 补全 Legacy Binding；
- 查看 Service 健康、容量、队列和 Runtime；
- 提供只读 Runtime/daemon/Service 运维信息。

Management Token 是整个 Service 的高权限控制面凭据，不是某个 Room 的局部凭据。浏览器只在当前页面内存中持有它直到完成一次 Bearer bootstrap，随后使用 Service-scoped HttpOnly Session Cookie，并把 CSRF Token 保留在内存；CLI/API 客户端仍可直接使用 Bearer Header。Management Session 与任一 Room Session 都不能跨作用域复用。

### Room View

负责：

- 公共时间线、搜索、引用与线程；
- 消息目标、路由与意图；
- 图片、附件与导出；
- Agent 生命周期、角色、审批；
- Work Inspector、Git status/diff 和重试。

每个激活 Room 有独立的 loopback URL 和 token/session 边界。一个 Room 的凭据、SSE cursor、附件 ID 或草稿不能用于另一个 Room。

## Message、Delivery、Processing 与 Turn

它们不是同一个概念。

### Message

用户或 Agent 在 PairRoom 公共时间线中的持久记录。用户消息只选择一个 Agent 或一个可解析为单一 Agent 的角色目标；正文 mention、显式接收者和 `target_role` 优先于回复推断。仅提供 `reply_to` 时，回复 Agent 消息会投递回该 Agent；没有目标事实时，正常 Driver/Reviewer 配置只投递给唯一的当前 Driver。需要两侧参与时，应使用自然语言阶段序列或在当前 Agent 的结果中显式交棒，而不是同时启动两个 Runtime。

Agent 消息还可持久化一个有界 `handoff`：完整 `text` 服务于人类时间线，`handoff` 只携带下一 Agent 完成其轮次所需的目标、改动范围、证据、风险和明确问题，避免把长报告重复注入另一侧原生上下文。

### Delivery

描述某个目标输入是否进入对应 Harness：

```text
pending → started | injected | queued → failed | skipped
```

例如 `started` 表示启动了新原生 Turn，`injected` 表示注入正在运行的 Codex Turn，`queued` 表示等待安全边界。

### Processing

描述 Harness 是否完成该目标：

```text
waiting → working → completed
                 ├→ cancelled
                 ├→ failed
                 └→ superseded
```

“消息成功持久化”不等于“Agent 已开始”，“已进入 Harness”也不等于“工作完成”。PairRoom 分开记录是为了避免把丢失、排队、取消和供应商失败混在一起。

### Turn

Turn 是供应商 Runtime 的一次工作单元。多个发给同一 Agent 的 PairRoom 输入可能被关联到同一个原生 Turn；同一 Room 任意时刻只允许一个 Agent 拥有 active Turn。跨 Agent 输入与 `next_turn` 输入进入 Room 级 FIFO 队列，只有可靠的 `turn.completed`、已确认的进程退出，或显式 cancel/stop 边界才会释放 owner。普通 Runtime `error` 只是诊断，不能启动下一位 Agent。

Room FIFO 是进程内调度状态。服务重启不会自动重放排队消息；未提交项会 fail-closed 地结算为 skipped/cancelled，用户确认后通过 Retry 创建新的可审计尝试。

Inspector 的工具、命令、计划、Diff、用量和最终结果按 Turn 投影。PairRoom 保存有界摘要，不复制供应商完整内部 Transcript 或推理状态。

## 消息意图

对正在工作的目标，新输入可表达：

- **append/steer**：追加到当前讨论；发给同一 Agent 时，即使没有 `reply_to`，也会阻止该 Agent 的较旧结果继续自动接力；
- **next turn**：作为独立任务等待下一个安全 Turn；未显式回复旧消息时，不会使其他讨论的 Agent 结果过期；
- **supersede**：明确取代旧指令，并收口受影响状态。

取消和重试按目标执行。仍在 Room FIFO 的消息可单独取消且不会 Interrupt Runtime；已经进入原生 Turn 的输入受供应商取消粒度限制，可能连同该 Turn 内其他已接受输入一起取消，但不会删除 Room FIFO 中尚未提交的后续项。重试创建新消息并引用原消息，不改写历史。

## 单轮次接力

PairRoom 只有一个运行策略：`turns`。

```text
user → active Agent → optional HANDOFF + NEXT → peer → … → DONE/user
```

规则：

- 同一时刻只有一个 Agent Turn owner；另一 Agent 的输入在 Room 队列等待；
- 发给当前 owner 的 `append`/`supersede` 可作为 steering，`next_turn` 即使目标相同也等待下一安全边界；
- 普通 `@peer`、`@claude`、`@codex` 文本不会从 Agent 输出中启动新 Turn；
- 交棒必须同时包含可用的 `[PAIRROOM:HANDOFF]...[/PAIRROOM:HANDOFF]` 与 `[PAIRROOM:NEXT]`；
- `[PAIRROOM:DONE]` 返回用户，`[PAIRROOM:WAIT]` 等待用户决定，`[PAIRROOM:BLOCKED]` 表示未解决阻塞；
- 新用户输入、冲突控制标记、缺失 handoff 或 `--max-hops` 上限都会 fail closed，不继续旧接力。

路由模式只接受 `turns`；`manual`、`mentions`、`roundtable` 属于无效值，不会迁移或规范化。Agent 契约只识别 `NEXT`、`DONE`、`WAIT` 和 `BLOCKED`；旧协议的 `CONTINUE`、`IMPLEMENTED`、`REVIEW_CHANGES` 与 `REVIEW_APPROVED` 不再被识别。

## 角色与 Workspace

角色决定协作意图，Workspace 与 Vendor policy 才决定实际边界。

### Driver

- 使用 live working tree；
- 使用用户配置的 Claude permission mode 或 Codex sandbox；
- 通常是唯一写入者。

### Reviewer

- 使用独立 Git snapshot；
- snapshot 包含 HEAD、dirty tracked patch 和 untracked regular files；
- 不安全 symlink、无法应用的 patch 或不可读 HEAD 会明确失败；
- Claude 使用 plan + write-tool deny；
- Codex 使用 readOnly sandbox；
- POSIX 上额外移除写位，Windows 明确报告较弱文件权限边界。

Reviewer snapshot 用于读取和审查，不是并行实现分支，也不是容器级隔离。Room 激活时会建立初始边界；当 Reviewer 处于安全空闲状态并即将开始新的审查 Turn 时，PairRoom 会重新生成快照，并在捕获期间序列化两侧的新提交，因此 Driver 刚完成的 dirty/untracked 变化会以一致视图进入本轮审查。正在执行或被 steer 的同一个 Reviewer Turn 不会中途更换文件系统视图。

## Event Log 与 Registry

### Room Event Log

`events.jsonl` 是 Room 的事实源。领域变化先持久化再发布给 SSE；启动时通过 replay 恢复 snapshot。中间损坏不会被静默跳过。

### Service Registry

`service-registry.json` 是 Project/Room/Binding 的可重建 checkpoint，不是 Room 历史事实源。默认 `rooms/` 下的 Event Logs 可用于重建；显式导入的自定义 Legacy 目录超出默认扫描边界，Registry 丢失后需要再次显式导入。

## 常见误解

| 误解 | 正确理解 |
|---|---|
| 打开两个模型窗口就是 PairRoom | PairRoom 还提供共同历史、路由、身份、状态、审批和恢复 |
| Room 就是 Runtime | Room 持久存在；Runtime 可被挂起和重建 |
| Existing Binding 会导入旧聊天 | 只恢复 Vendor context；PairRoom 时间线从绑定后开始 |
| Delivery completed 代表 Agent 做完了 | Delivery 与 Processing 分开 |
| Reviewer 一定无法写磁盘 | 有多层约束，但不是 OS/container 绝对隔离 |
| Registry 是唯一数据源 | 每个 Room 的 Event Log 才是 Room 事实源 |
| 关闭浏览器会停止 Turn | 浏览器不是 Runtime owner；活动 Turn 继续 |
