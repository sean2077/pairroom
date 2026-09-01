# PairRoom 核心概念

PairRoom 同时出现 Project、Room、Binding、Runtime、Message、Delivery、Processing 与 Turn。它们分别回答“代码在哪里”“历史属于谁”“供应商上下文是谁”“进程是否在运行”“输入是否送达”“工作是否完成”。

## 心智模型

```text
PairRoom Service
└── Project                         canonical Git worktree
    ├── Room A                      durable collaboration boundary
    │   ├── Claude Binding          vendor Session identity
    │   ├── Codex Binding           vendor Thread identity
    │   └── Room Runtime            ephemeral processes + Room server
    └── Room B
```

Project、Room、Binding 和 Event Log 是长期控制状态；native process、active owner、Room FIFO 与 transient telemetry 是进程状态。

## Project

Project 是 canonical Git worktree 的注册记录。根目录、子目录与等价 symlink 会归一到同一 Project；两个独立 worktree 即使来自同一仓库，也属于不同 Project。

Project path 暂时不可用时保留注册和 Room。只有没有任何 active/archived Room 的 Project 才能注销；注销不删除 Git worktree。

## Room

Room 是一个 Project 内的 durable 协作边界，拥有：

- 公共 Timeline 与 append-only Event Log；
- 附件和 metadata；
- 一个 Claude Binding 与一个 Codex Binding；
- 角色、Workflow、Approval、Delivery/Processing 和 Turn projection。

生命周期：

```text
active <-> archived -> permanent removal
```

Archive 保留历史与 Binding ownership；Restore 不自动启动 Runtime；永久删除只处理 PairRoom 管理的数据。

## Binding

Binding 关联 Room 与 Vendor context：Claude Session 或 Codex Thread。

- `existing`：Room 创建前精确 resume/验证；失败时不静默创建新 context；
- `new`：先创建 pending Binding，首个 native input 被接受后 materialize ID；
- Codex 的 `thread/start` 可能先返回只存在于 app-server 内存中的候选 ID；只有首个 Turn 被接受并产生 durable rollout 后，这个 Thread 才能作为 Binding materialize。若进程在此前退出，候选 ID 会被丢弃，下次激活创建新 Thread；
- ownership：同一个 `(agent, vendor_session_id)` 在 Service 中只能属于一个 Room。

Existing Binding 只从 PairRoom 的绑定时刻开始建立公共 Timeline，不导入此前 Vendor Transcript。

## Runtime

Room Runtime 是可回收的执行容器，包括 Room Engine、Room HTTP/SSE server、Adapter、native process、active owner、FIFO 和 connection-local request。

Room 可以存在而 Runtime 未激活。Runtime capacity、idle suspend 或 Service 重启不删除 Room 历史。busy Turn 不因容量回收被抢占；重启不会自动重放进程内 FIFO。

## Message、Delivery 与 Processing

**Message** 是 append-only Timeline 记录。Reply、Retry、Intent、Workflow 和附件都是关联字段，不修改旧 Message。

**Delivery** 描述 transport：

```text
pending | started | injected | queued | failed | skipped
```

**Processing** 描述执行：

```text
waiting | working | completed | cancelled | failed | superseded
```

Delivery `started` 只表示 Runtime 接受，不表示任务完成。每个目标独立记录状态、detail、Turn ID 和更新时间。

## Native Turn

Turn 是官方 Runtime 的完整执行边界，可以包含多次工具调用和多个 steering input。一个 Room 同时最多一个 active Turn owner。

可靠 terminal boundary 包括：

- native completion；
- 明确的 native cancel/abort；
- 已确认的进程退出边界。

普通 diagnostic `RuntimeError` 可能出现在 Turn 中途，不会自行释放 owner 或启动 peer。

## FIFO 与 Intent

- 当前 owner 的 `append` 可 steering 同一 Turn；
- 跨 Agent 或 `next_turn` 输入进入 Room FIFO；
- Scheduler reserve 后、native submit 前会再次检查取消；
- FIFO 是 process-memory state，重启后 fail closed，不自动 replay。

这提供确定性接力，但不是固定的 A/B/A/B：同一 Agent 可以连续拥有多个显式排队的 native Turn。

## Handoff 与 hop

Agent 自动交棒必须同时给出有界 `HANDOFF` 和 `NEXT`。完整回答留给用户，handoff 只包含下一 Agent 改变决策所需的目标、范围、证据、风险和请求。

`max_agent_hops` 限制一条自动接力链的 Turn 数；用户新输入会使 stale 自动链 fail closed。

## 角色与 Workspace

- **Driver**：使用 live worktree，可按权限执行和修改；
- **Reviewer**：使用独立 snapshot 与 Vendor 只读 policy；
- **Peer**：不强制 Driver/Reviewer ceremony 的普通角色。

角色是 Workspace 与 policy 边界，不只是 UI 标签。角色切换等待安全 Turn 边界。

## Workflow 与批准

自然语言 Workflow 是显式 stage sequence，而不是新的自由路由模式。plan/review/audit 默认只读，execute 使用 Driver，discuss 保持当前角色。

执行批准绑定当前计划 revision。计划修改后旧批准失效；`WAIT` / `BLOCKED` 把控制权交回用户。

## Event Log 与 Registry

Room Event Log 是 Room 历史事实源。`service-registry.json` 是可重建 checkpoint，用于保留 Project 注册、加速发现和检查 Binding ownership；它不能替代各 Room Event Log。

详细数据边界见 [Storage](STORAGE.md)，机械协作合同见 [Protocol](PROTOCOL.md)。
