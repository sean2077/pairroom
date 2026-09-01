# PairRoom 日常使用

本页说明 Management Shell 与 Room View 中“怎么做”。概念解释见 [Concepts](CONCEPTS.md)，HTTP 合同见 [API Reference](API_REFERENCE.md)。

## 打开与认证

`pairroom service` 启动后打印 Management URL；`pairroom daemon open` 会验证当前 daemon 的 loopback URL 后打开。浏览器 URL fragment 中的 bootstrap token 用于换取 HttpOnly Session，不应复制到日志或 Issue。

Management Shell 与每个 Room View 是独立认证作用域。某个页面已登录，不代表另一个 Room 或 Management API 已授权。

## Management Shell

### Overview

先检查：

- Project / Room 数量和 unavailable Project；
- pending Binding；
- Runtime capacity、busy/queued/failed；
- pending Room deletion cleanup；
- daemon / service diagnostics。

Overview 是当前 projection，不是历史审计记录；Room 历史以 Event Log 为准。

### 登记与刷新 Project

登记时使用 Git worktree 的绝对路径。根目录、子目录与等价 symlink 会归一到 canonical worktree；两个独立 worktree 是两个 Project。

Project path 暂时不可用时保留记录和 Room。恢复挂载后使用 Refresh。只有没有 active/archived Room 的 Project 才能注销，且需要精确输入 Project ID；注销不删除仓库。

### 创建 Room 与 Binding

每个 Room 必须有一个 Claude Binding 和一个 Codex Binding：

- `new`：先占位，首次真实 native Turn 接受后 materialize ID；
- `existing`：创建前精确验证已有 ID。

同一 `(agent, vendor_session_id)` 在整个 Service 中只能归属一个 Room。归档不会释放 ownership；永久删除 Room 后才释放。

若旧 Room 缺少 Binding，可在 Room 未运行时完成 Binding。Service 会先等待并挂起 Runtime，避免控制面与 Engine 同时写 Event Log。

### 激活、挂起与容量

Open Room 会请求 Runtime activation。容量不足时状态为 queued；空闲 Runtime 可被回收，但 busy native Turn 不会被容量策略抢占。

Suspend 只在安全边界完成。若 Room 正在执行，普通 suspend 返回冲突；Archive 则按产品语义主动 interrupt 当前 Turn、等待收口、挂起 Runtime，再写入 archive lifecycle event。

### Rename、Archive、Restore、Delete

- **Rename**：等待安全 Turn 边界，不中断 active work；
- **Archive**：默认停止 active Turn，保留 Event Log、附件和 Binding ownership；
- **Restore**：把 archived Room 恢复为 active lifecycle，不自动启动 Runtime；
- **Permanent delete**：只允许 archived Room，并要求显式不可逆确认；删除 PairRoom 数据和 Binding ownership，不删除 Git worktree、Vendor Session/Thread 或外部导入目录；
- **Batch archive/delete**：逐 Room 返回结果；重复 ID 会去重，单项失败不会伪装为整批成功。

若永久删除的物理清理在 durable commit 后失败，Room 不会复活；Maintenance 区会显示 pending cleanup，可显式重试。

### Legacy import

Import 只登记现有 Room 数据目录，不搬移、复制或重写 `events.jsonl`。导入路径仍参与 Project canonicalization、Room ID 和 Binding ownership 去重。

## Room View

### Timeline 与 Inspector

Timeline 面向人类阅读，Inspector 面向执行事实：

- Message：不可变的对话记录；
- Delivery：输入是否被 transport 接受；
- Processing：该输入实际等待、执行、完成、取消或失败；
- Turn：native Runtime 的执行边界；
- Activity：工具、命令、计划、diff、usage 和诊断 telemetry。

高频 transient telemetry 会批量刷新；最终 Message、Processing 与 Turn projection 不依赖浏览器 DOM。

### 发送目标和 Intent

一条用户输入只能有一个 Agent/角色目标。UI 不提供 `@All`；后端也拒绝多个 recipient。

- `append`：目标是当前 owner 时尝试 steering；
- `next_turn`：无论是否同一 Agent，都排到下一 native Turn；
- `supersede`：取代旧指令，并在 native 能力允许时中断。

目标为另一 Agent 时进入 Room FIFO，直到当前 owner 到达可靠 terminal boundary。

### 自然语言 Workflow

至少两个显式 `Claude/Codex + action` 组合才会编译为 Workflow，例如：

```text
Claude 规划，Codex 审查，等我批准后 Codex 执行，Claude 验收。
```

支持 plan、review、execute、audit、discuss 及对应中文动作词。plan/review/audit 使用只读边界，execute 使用 live Driver workspace。进入执行前需要的批准绑定当前计划 revision；修改计划后必须重新批准。

单句“Codex review this”仍是普通单目标 Turn，不会凭空创建多阶段流程。

### Handoff 与收敛

Agent 之间普通 `@peer` 不触发机械投递。自动交棒需要：

```text
[PAIRROOM:HANDOFF]
Goal: ...
Scope: ...
Evidence: ...
Risks: ...
Exact ask: ...
[/PAIRROOM:HANDOFF]
[PAIRROOM:NEXT]
```

`DONE` 返回用户，`WAIT` 等待用户决定，`BLOCKED` 表示未解决阻塞。缺失 handoff、冲突标记、用户新指令或 hop 上限都会停止旧接力。

### 角色与 Workspace

Driver 使用 live worktree。Reviewer 在独立 snapshot 中运行，并使用 Vendor 只读 policy。角色切换需要安全边界；Reviewer 只读是 defense in depth，不是容器级隔离。

审查应独立验证文件、测试和 Git diff，不把 peer 的自述当成完成证据。

### 图片与附件

Room 支持经内容签名、大小和尺寸校验的 PNG/JPEG/GIF/WebP。每张最多 `5 MiB`、每条 Message 最多 `8` 张且合计最多 `20 MiB`；单边不超过 `8000 px`，总像素不超过 `64,000,000`。上传后未被 Message 引用的附件可以删除；进入 durable Transcript 后不可从 UI 单独删除。

Agent Markdown 引用的本地图片只有在 canonical path 位于仓库内且无 symlink escape 时才会导入。绝对本机路径不会进入公开 Message/Event/API。

### Retry、Cancel、Interrupt、Stop

- Retry 创建新 Message，并保留 `retry_of`；
- 取消仍在 FIFO 的消息只删除该项，不中断 Runtime；
- 取消已 reserve 但未 submit 的项会阻止 native submission；
- 已被 Runtime 接受的输入可能需要 interrupt 当前 Agent Turn；
- Interrupt 针对当前 Turn；Stop 终止该 Agent Runtime；Restart 重新启动并按严格 Binding 恢复。

取消或进程退出后，先检查工作区是否已有副作用，再决定 Retry。

### Export

Room 可导出 Markdown 或 JSON Transcript。普通 JSON export 默认不含 Inspector event tail；需要 forensic event 时显式请求 `include_events=1`。Export 不是可恢复备份，备份请使用 `pairroom backup`。

## 浏览器状态边界

浏览器保存的是导航、筛选、展开状态等 presentation state。Token 不进入 Web Storage；页面刷新后 durable state 来自服务端 snapshot + Event Log，transient Activity 可能不会重放。
