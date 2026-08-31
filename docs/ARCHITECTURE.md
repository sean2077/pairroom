# PairRoom 架构设计

> [文档首页](README.md) · [核心概念](CONCEPTS.md) · [Multi-Room Service](MULTI_ROOM_SERVICE.md) · [Room 协议](PROTOCOL.md) · [安全策略](../SECURITY.md)

本文描述当前 `main` 的实现架构。重点不是某个页面长什么样，而是：进程如何组织、状态由谁拥有、数据何时落盘、两个官方 Harness 如何接入，以及系统在失败时选择什么边界。

## 1. 架构目标

PairRoom 的核心约束是：

> 保留官方 Claude Code 和 Codex 的完整 Harness，只实现两者之上的本地三方协作、介入、审批、可观察性与持久化层。

PairRoom 不实现：

- 模型调用网关或账号系统；
- 通用 Agent loop、工具执行器或上下文压缩；
- 通过 ANSI 终端文本猜测结构化状态；
- 供应商 Session/Thread 数据库的复制品；
- 多人身份、团队 RBAC、云同步或公网托管。

这条边界决定了系统必须同时满足四件事：

1. **原生性**：只通过官方 CLI 的结构化接口工作；
2. **可见性**：把公共结论、过程事件和审批投影到同一房间；
3. **可恢复性**：PairRoom 自己拥有的事实必须先持久化再发布；
4. **诚实性**：无法证明安全或一致时 fail closed，而不是伪装成功。

## 2. 三种运行拓扑

### 2.1 `pairroom service`：多 Project / 多 Room 控制面

```text
┌────────────────────────────── Browser ──────────────────────────────┐
│ Management Shell                         Room View                   │
│ Project · Room · Runtime lifecycle       Timeline · Inspector       │
└───────────────┬──────────────────────────────┬───────────────────────┘
                │ Management REST             │ Room REST + SSE
┌───────────────▼──────────────────────────────▼───────────────────────┐
│ PairRoom Service process                                             │
│                                                                      │
│ Management Server ─ Registry ─ Provisioner ─ Runtime Manager         │
│                                             │                        │
│                   ┌─────────────────────────┼────────────────────┐   │
│                   │ logical Room Runtime A  │ Room Runtime B      │   │
│                   │ Engine/Store/Web/Hub    │ Engine/Store/Web    │   │
│                   └──────────┬───────┬──────┴─────────┬───────┬───┘   │
└──────────────────────────────┼───────┼────────────────┼───────┼───────┘
                               │       │                │       │
                         claude child  codex child  claude child codex child
```

Service 是一个本地进程。每个 Room Runtime 是该进程内的独立逻辑运行单元，拥有自己的 Engine、Store、附件库、Room HTTP/SSE listener、认证 Token 和两侧 Adapter；官方 `claude` 与 `codex app-server` 则是各 Runtime 启动的子进程。

Runtime 按需激活，并受全局容量限制。Management Shell 不承载聊天协议；打开 Room 时浏览器进入该 Room 自己的 listener 和认证域。

### 2.2 `pairroom daemon`：操作系统托管的同一 Service

`pairroom daemon` 不是第二种服务实现。它负责把 `pairroom service` 安装到：

- Linux systemd；
- macOS launchd；
- Windows 当前用户 Task Scheduler。

安装定义固定二进制路径、工作目录、日志、环境和完整 Service 参数。生命周期仍由同一个 Service/Runtime Manager 实现，因此容量、数据、认证和关闭顺序不发生分叉。

### 2.3 `pairroom serve`：兼容的单 Room 快捷入口

```text
Browser ── Room REST/SSE ── Room Engine/Store ── ClaudeAdapter/CodexAdapter
```

`serve` 跳过 Registry、Provisioner、Management Server 和全局 Runtime Manager，直接为一个仓库与一个 Room 数据目录启动运行时。它适合一次性使用、调试和旧工作流兼容，但不是多 Room 管理入口。

## 3. 控制面与房间面的边界

| 层 | 拥有的职责 | 明确不拥有 |
|---|---|---|
| Management Server | Project 登记、Room provisioning/lifecycle、Runtime 激活/挂起、Service snapshot | Room 消息正文、附件读取、Room SSE、供应商 Transcript |
| Registry | Project/Room/Binding 索引与生命周期 checkpoint | Room Event Log 的替代事实源 |
| Runtime Manager | Runtime 容量、FIFO 队列、idle 回收、关闭顺序 | 活动 Turn 的强制抢占 |
| Room Runtime | Engine、Store、附件、工作区、Room HTTP/SSE、Adapter | 其他 Room 的状态或 Token |
| Vendor Harness | 原生会话、模型上下文、工具、Skills/MCP/Hooks、供应商凭据 | PairRoom 公共时间线与 Event Log |

这层分离带来两个重要结果：

- Management Token 是 Service 范围的高权限控制面凭据；Room Token、Session/CSRF、SSE cursor 和附件 ID 则严格按 Room 隔离，不能跨 Room 复用；
- Registry 丢失时可以从默认 Room Event Logs 重建索引，但不能反过来用 Registry 还原 Room transcript。

## 4. Service 组件

### 4.1 Management Server

Management Server 提供内嵌静态 Shell 与 `/api/v1` 管理端点，负责：

- 返回 Project、Room、Runtime、policy、summary 和 capabilities；
- 登记 canonical Git Project；
- 创建、改名、归档、恢复 Room；
- 补全 Legacy Binding；
- 请求 Runtime 激活或安全挂起；
- 显式导入自定义 Legacy Room；
- 在 mutation 上使用 per-Room 串行边界，避免 lifecycle 与 suspend 并发写同一控制状态。

它接受直接 Bearer Header，或由 Bearer bootstrap 建立的 Service-scoped browser session；Session mutation 还要求内存 CSRF。它不把 query token 当作凭据，也不提供服务器路径浏览器。

### 4.2 Registry

Registry 管理三类索引：

```text
canonical Project root  -> Project
Room ID                 -> Room metadata/lifecycle
(agent, vendor ID)      -> owning Room
```

`service-registry.json` 是可替换 checkpoint，而不是最深层事实源。默认 `rooms/` 下的 Event Logs 包含足够的 Service lifecycle 与 Binding 事件，可用于重建索引。自定义路径导入的 Legacy Room 不在默认扫描边界内，checkpoint 丢失后需要再次显式导入。

Registry 的一致性原则是：

1. Room Event 先提交；
2. 再更新内存索引和 checkpoint；
3. 如果无法证明内存、Event Log 与 checkpoint 一致，阻止后续 mutation。

### 4.3 Project canonicalization

登记 Project 时，服务端按固定顺序处理：

```text
absolute input
  -> directory/access check
  -> symlink resolution
  -> git rev-parse --show-toplevel
  -> canonical worktree root
  -> stable Project identity + deduplication
```

因此同一 Git worktree 的根目录、子目录和符号链接只会得到一个 Project。不同 worktree 即使来自同一仓库，也作为不同 Project 管理。

### 4.4 Provisioner

Room provisioning 同时建立：

- 一个 Project 归属；
- 一个 Claude Session Binding；
- 一个 Codex Thread Binding；
- 初始 Room Event Log 与 metadata；
- 全局 Binding ownership。

创建过程在隐藏暂存目录完成，全部校验成功后原子发布。`existing` Binding 必须能被对应官方协议精确恢复且未被其他 Room 占用；任何一步失败都不应留下可见半成品 Room。

`new` Binding 是 deferred 的：空的原生 Session/Thread 在尚无用户 Turn 时未必具备可持久化身份。首个真实输入被官方 CLI 接受后，Engine 才追加 materialization 事件并建立全局 ownership。提交或唯一性检查失败会中断该执行，而不是让一个身份同时属于多个 Room。

### 4.5 Runtime Manager

Runtime Manager 维护：

- `--runtime-limit` 全局容量；
- starting/active/stopping/queued/suspended/failed phase；
- `busy`、`occupies_capacity`、queue position、last-used 和错误；
- idle Runtime 的 LRU 式回收；
- 容量不足时的 FIFO 激活队列；
- Service 关闭时的有界排空。

容量不足时先尝试挂起最久未使用且 idle 的 Runtime。若所有 slot 都在执行 Turn，新 Room 进入 FIFO 队列；活动 Turn 不会仅为腾出容量而被 interrupt。

`occupies_capacity` 是独立事实：

- starting、active、stopping 占用；
- cleanup 未被证明完成且仍持有实例的 failed 状态占用；
- queued 与 suspended 不占用。

安全挂起只允许 queued cancel 或 active+idle drain。busy、starting/stopping 冲突以及 cleanup-uncertain failed 会返回冲突，不伪造容量释放。

## 5. Room Runtime 组件

### 5.1 Room Engine

Room Engine 是领域状态机，负责：

- 消息、线程、引用、目标和重试；
- Manual/Mentions/Roundtable 路由；
- 用户新指令对旧自动接力的优先级；
- Delivery 与 Processing 双生命周期；
- Runtime correlation 和 final response 投影；
- 角色、工作区和审批生命周期；
- 崩溃恢复时瞬态状态收口；
- 领域事件先落盘、后广播。

Engine 不推理模型内容，也不解析终端绘制结果。它只消费 Adapter 产生的结构化 RuntimeEvent。

### 5.2 Event Store

每个 Room 使用 append-only JSONL：

```text
<room-data>/
├── events.jsonl
└── metadata.json
```

关键性质：

- `seq` 单调递增；
- durable event 在 SSE 发布前写入并同步；
- 启动时通过 replay 恢复 snapshot；
- 只修复损坏的最后半行，不静默跳过中间损坏；
- metadata 记录 Store schema；
- 高于当前二进制支持的未来 schema 会被拒绝。

当前 Store schema 为 `8`。schema 描述的是 PairRoom Event 投影，不是 Claude/Codex 原生会话格式。

### 5.3 Event Hub 与 SSE

Hub 把 Engine 事件投影到 Room SSE：

- durable event 带单调 `seq`，可用于 replay/cursor；
- 瞬态 token delta 或心跳不推进 durable replay cursor；
- 浏览器发现序列缺口时重新读取 snapshot，而不是猜测缺失状态；
- 长房间初始只加载最新窗口，历史通过 cursor 分页读取，底层 transcript 仍完整保留。

### 5.4 Attachment Store

```text
<room-data>/attachments/
├── att-<opaque-id>.json
└── att-<opaque-id>.<ext>
```

只接受 PNG、JPEG、GIF、WebP。每个附件记录安全文件名、媒体类型、字节数、SHA-256、宽高、来源和创建时间。

边界规则：

- 本机绝对路径不进入 Message/Event/API/export；
- 不信任扩展名，校验真实内容签名；
- 每次跨浏览器或 Agent 边界前复核大小、普通文件、非 symlink、维度和哈希；
- Agent 生成图片只允许从当前仓库 canonical 边界内导入；
- 远程 Markdown 图片不会自动抓取；
- 已被 durable message 引用的附件不能通过删除端点移除。

### 5.5 Workspace Manager

Driver 默认使用 live working tree。Reviewer 使用独立 Git snapshot：

1. 以当前 HEAD 创建 detached worktree；
2. 应用完整 `git diff HEAD`，覆盖 staged 与 unstaged tracked 变化；
3. 复制 untracked regular files；
4. 拒绝不安全 symlink 与越界目标；
5. 记录 source HEAD、patch/untracked digest 和 dirty 状态；
6. POSIX 上移除写位，并叠加供应商原生只读策略。

Reviewer snapshot 不是容器、VM 或只读 mount。它用于默认的“一个实现者、一个审查者”工作流，不是第二个并行实现分支。

## 6. Adapter 边界

### 6.1 统一 Adapter 契约

Room Engine 只依赖统一概念：

```text
start / stop / interrupt
submit / steer
role policy
runtime events
approval requests
session/thread identity
```

供应商协议差异被限制在 Adapter 内。MockAdapter 也实现同一契约，用于确定性产品和恢复测试，但不能证明真实供应商行为。

### 6.2 Agent 协作契约投影

`internal/protocol` 是固定协作规则的单一来源，`pairroom protocol` 将同一份版本化契约投射为文本或 JSON。可计数、可校验、可持久化的机械行为继续由 Room Engine、Adapter、角色沙箱和路由器执行，而不是依赖长 prompt 反复提醒。

原生 Harness 只接收一个紧凑 bootstrap：

- ClaudeAdapter 优先使用 Claude Code 的原生 append-system-prompt 能力；兼容路径最多在首个输入前投射一次；
- CodexAdapter 在 `thread/start` 和 `thread/resume` 的 `developerInstructions` 中投射 bootstrap；`turn/start` 与 `turn/steer` 不再内联完整规则；
- bootstrap 不包含 Room 名称或 repository 绝对路径，只声明 Human/Harness authority、角色与路由判断入口、控制 marker 和 `pairroom-protocol/v3` 查询命令；
- 每轮 `[PairRoom message]` envelope 只携带 message/thread/hop、from/to、role、routing、intent、附件元数据和正文等动态事实。

`internal/prompt` 的 byte-budget 测试是发布门：固定 bootstrap 或逐轮 envelope overhead 超预算会直接使测试失败，防止规则再次无界增长。

### 6.3 ClaudeAdapter

ClaudeAdapter 启动官方 `claude`，使用：

```text
-p
--input-format stream-json
--output-format stream-json
--permission-prompt-tool stdio
```

职责包括：

- 长驻双向 stream-json；
- 原生 control initialize 握手；
- Session 创建、持久化与精确 resume；
- 用户输入队列和 message correlation；
- 文本、工具、Hook、子 Agent、结果事件投影；
- base64 image content blocks；
- `can_use_tool`、`AskUserQuestion` 与 control response；
- 进程退出时收口 waiter、审批和输入状态。

Reviewer 默认策略：

```text
permission mode = plan
write-capable tools = denied/disallowed
```

未知 control request 直接返回协议错误。

### 6.4 CodexAdapter

CodexAdapter 启动：

```text
codex app-server
```

职责包括：

- initialize；
- thread start/resume；
- turn start/steer/interrupt；
- `clientUserMessageId` correlation；
- `localImage` 输入；
- item/plan/diff/usage/command 等结构化事件；
- command/file/additional-permission approval；
- overload 的有界重试；
- 未知 server request fail closed。

Reviewer 每个 Turn 使用原生 read-only sandbox 请求。

### 6.5 原生事实不被复制

PairRoom 持久化 Vendor Session/Thread ID 和自己的事件投影，但不导入绑定前供应商 Transcript。Existing Binding 恢复的是原生 context；Room 时间线只从 Binding 成为 PairRoom Room 的一部分后开始。

## 7. 浏览器与认证架构

所有内置 listener 只接受数字 loopback 地址，如 `127.0.0.1` 或 `::1`。Management 与 Room 使用不同浏览器链路。

### 7.1 Management Shell

```text
launch URL fragment
  -> JavaScript reads token
  -> fragment removed from address bar
  -> POST /api/v1/session with Bearer
  -> HttpOnly + SameSite=Strict service session cookie
  -> in-memory CSRF token for mutations
  -> Management REST uses browser session
```

- bootstrap Token、Session ID 和 CSRF 不写 `localStorage`/`sessionStorage`；
- 刷新可通过仍有效的 Cookie 恢复 CSRF；Service 重启、会话过期或显式注销后需要重新打开完整启动 URL；
- CLI/API 客户端可直接使用 Service Bearer Header；
- query token 不授权 API。

### 7.2 Room View

启用 Token 认证时：

```text
launch URL fragment
  -> bootstrap endpoint
  -> HttpOnly + SameSite=Strict session cookie
  -> in-memory CSRF token for mutations
  -> REST/SSE use browser session
```

长期 Token 不进入 Web Storage。CLI/API 客户端仍可直接使用 Authorization Header。`serve` 未配置 Token 时仍依赖 numeric loopback、Host 和 same-origin 防护。

### 7.3 Web 安全层

两个 Web 面共同执行 query-token 拒绝、same-origin、CSP、no-referrer、nosniff 和 frame-ancestor 防护。除此之外：

- Management browser session 的 mutation 要求 CSRF，直接 Bearer 客户端仍受 same-origin mutation 检查；
- Room Server 还执行 Host 检查、Session mutation CSRF、固定窗口限流和认证附件 Blob 响应；
- Management Session 与每个 Room Session 使用独立 Token、Cookie 作用域/origin 和 CSRF，不能跨面复用。

远程使用只通过 SSH 本地端口转发到服务端 loopback listener；PairRoom 不内建 TLS 或公网监听。

## 8. 状态事实源

| 信息 | 唯一事实源 |
|---|---|
| Project/Room/Binding 生命周期 | Room Event Logs；Registry 是 checkpoint/index |
| Room 消息、角色、路由、审批投影 | PairRoom Room Event Log |
| 附件元数据引用 | PairRoom Room Event Log |
| 附件二进制 | Room Attachment Store |
| Runtime phase/capacity | 当前 Service Runtime Manager；快照为派生视图 |
| Claude 原生上下文 | Claude Code |
| Codex 原生上下文 | Codex App Server |
| Git 文件内容 | Project worktree / Reviewer snapshot |
| Browser UI | snapshot + SSE + 非关键本地界面状态 |

任何文档或 UI 都不应把派生 snapshot 描述为比上述事实源更权威。

## 9. 关键数据流

### 9.1 创建 Room

```text
Management form
  -> canonical Project lookup
  -> validate requested bindings
  -> hidden staging directory
  -> probe/resume existing identities
  -> write initial Room events
  -> claim global identities
  -> atomic publish
  -> Registry checkpoint
```

### 9.2 打开 Room

```text
POST activate
  -> existing active URL, or request capacity
  -> evict idle runtime / enqueue FIFO
  -> open Room Store and Workspace Manager
  -> create Engine + Adapters + Room Server
  -> bind independent loopback listener/token
  -> return Room URL
```

打开页面本身不等于启动真实模型 Turn；是否自动启动 Agent 由 runtime 配置决定。

### 9.3 富媒体消息

```text
Browser upload
  -> validate/hash/durable attachment ID
  -> POST message with attachment IDs
  -> Engine resolves canonical metadata
  -> Message event fsync
  -> per-target Delivery state
  -> Claude base64 block / Codex localImage
  -> Runtime events + final response
  -> Processing terminal state
```

### 9.4 Agent 生成图片

```text
Agent writes repository image
  -> final Markdown references relative path
  -> canonical path + symlink boundary check
  -> raster validation and immutable import
  -> safe attachment metadata in final message
```

### 9.5 审批

```text
Vendor request
  -> Adapter policy guard
  -> durable ApprovalRequested
  -> browser decision
  -> durable decision
  -> vendor-native response
```

连接中断、Runtime 停止、角色变化或重启会使无法安全复用的未决审批过期。

## 10. 并发与一致性

- Registry/lifecycle mutation 通过控制面锁和 per-Room mutex 串行化；
- 每个 Room Event Store 只有一个写者；
- Engine 用互斥锁保护 snapshot 与 Adapter map；
- Claude stdin 与输入队列串行；
- Codex RPC 使用唯一 ID 与 waiter map；
- durable event 先 append+sync，后发布；
- role 先应用 Adapter policy，成功后再写 Room state；
- Room rename/binding completion 会等待活动 Turn 自然结束并挂起 Runtime，避免控制面与 Engine 双写；Room archive 会先 interrupt 活动 Turn 再挂起 Runtime，因此归档无需先在 Room 内手动停止 Agent；
- shutdown 先停止 Management mutation，再排空 Runtime，最后释放 Service lock。

## 11. 失败与恢复边界

| 失败 | 设计行为 |
|---|---|
| Provisioning 任一步失败 | 暂存目录不发布，不留下可见 Room |
| Binding 重复或无法精确恢复 | 拒绝创建/补全 |
| Registry checkpoint 损坏 | 默认 Room 可由 Event Logs 重建；自定义导入需重做索引 |
| 中间 Event Log 损坏 | 拒绝启动；只允许修复末尾半行 |
| Runtime cleanup 不确定 | failed 且继续占用 capacity，不假装释放 |
| 浏览器 SSE 缺序 | 重取 snapshot |
| Vendor 进程退出 | 收口 waiter、审批与 Processing，不保留“幽灵 working” |
| Reviewer snapshot 不安全 | 拒绝角色/运行，而不是回退到可写 live tree |
| 高版本 Store schema | 旧二进制拒绝打开 |
| 崩溃遗留 service.lock | 仅在人工确认旧进程退出后显式恢复 |

## 12. 依赖与部署边界

Go module 和内嵌 Web UI 不依赖第三方运行时框架。正常运行依赖的外部程序为：

```text
git
claude
codex
browser
```

开发验证另需 Go、Node.js、Python、shell 与归档工具。详见 [开发者指南](DEVELOPMENT.md)。

## 13. 当前产品边界

当前架构支持：

```text
one local Service
multiple canonical Git Projects
multiple durable Rooms per Project
bounded concurrently active Room Runtimes
one human + one Claude + one Codex per Room
one Driver + one Reviewer by default
```

仍不支持：

- 多用户身份与并发协同编辑；
- 远程 worker、云同步、托管 TLS 或公网 API；
- 一个 Room 绑定多个 Claude/Codex participant；
- Room 永久删除、Project 删除或运行时策略热修改；
- Reviewer 的容器/VM 级强隔离；
- 额外 Agent vendor 的稳定公共插件协议。
