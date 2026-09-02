# Core concepts

## Project、Room 与 Binding

**Project** 是一个本地 Git 仓库在 Management Service 中的注册记录。它保存仓库位置以及可创建的 Room；它不是仓库副本，也不拥有用户源码。

**Room** 是协作的持久化边界，包含参与者角色、消息、Turn 摘要、审批、Workflow、Binding 和 Event Log。Room 可以跨多次进程启动继续使用，但 native 进程本身不会随 Event Log 一起恢复。

**Binding** 把 Room 中的 Claude / Codex participant 绑定到供应商原生 session/thread。Binding 由 Service 管理，不能被另一个活跃 Room 随意复用。

## Message 与 native Turn

Message 是 PairRoom 的可审计输入或输出；native Turn 是 Claude Code 或 Codex 实际执行的一次完整回合。两者不是一一对应：

- 一个 Turn 可以接收多条 steering message；
- 一条排队 message 可能在以后启动新 Turn；
- transport delivery 成功不代表 Turn 已完成；
- 普通 diagnostic error 不一定代表 Turn 已终止。

PairRoom 只在可靠边界释放 owner，例如 native `turn/completed`、确认的进程退出或明确的取消 / abort 终止事件。

## Deterministic relay

Room 采用唯一的 `turns` policy：

```text
idle
  -> reserve FIFO item
  -> submit to Agent A
  -> Agent A owns the native Turn
  -> reliable terminal boundary
  -> release owner
  -> reserve next FIFO item
```

不变量：

1. 一个 Room 同一时刻最多一个 active native Turn owner；
2. 跨 Agent 输入只能在当前 Turn 结束后提交；
3. FIFO item 在提交前再次检查取消状态，避免 ghost Turn；
4. generic runtime error 只更新诊断，不自行交棒；
5. 用户的新指令优先于旧的自动接力。

## Message intent

- `append`：优先 steering 当前目标 Agent 的 active Turn；不安全时进入后续边界；
- `next_turn`：明确要求新 native Turn，即使目标仍是当前 Agent；
- `supersede`：中断或替代目标 Agent 的在途输入，实际影响范围受 native harness 的中断语义约束。

## Handoff 与控制标记

Agent 输出中明确的 `@claude`、`@codex` 或 `@peer` 是交给对应 peer 的路由指令；`@human` 或 `@user` 表示需要用户决策并停止自动接力。没有明确 peer 地址时，隐式自动交棒需要：

```text
[PAIRROOM:HANDOFF]
Goal / Scope / Evidence / Risks / Exact ask
[/PAIRROOM:HANDOFF]
[PAIRROOM:NEXT]
```

收敛标记：

- `NEXT`：在没有明确 peer 地址时，携带有效 handoff 后交给 peer；明确 `@peer` 地址本身即可请求 peer Turn；
- `DONE`：没有直接 peer 地址时当前链完成并返回用户；直接 peer 地址仍表示交棒；
- `WAIT`：需要用户决定；
- `BLOCKED`：存在尚未解决的外部阻塞。

最大 hop 数限制无界往返，但它不是机械的 A/B 轮换次数。

## Role 与 Workspace

- **Driver**：在 live workspace 中实现；
- **Reviewer**：在隔离的 reviewer snapshot 中检查，默认不修改 Driver 的 live tree；
- **Peer**：没有 Driver / Reviewer 特权的普通参与者。

角色是运行时权限和 workspace boundary，不只是 prompt 标签。切换角色需要在安全 Turn 边界进行。

## Workflow 与 Approval

自然语言中的 actor/action sequence 可以编译为 Workflow stages，例如 plan、review、execute、audit。Workflow 仍复用同一个 FIFO 和 single-owner scheduler。

Approval 绑定到明确的 Agent、native request 和 plan revision。进程重启后供应商 request ID 不再可靠，因此 pending approval 会失效，而不是自动重放。

## Restart 与 fail-closed

Event Log、Binding 和历史消息是 durable state；native process、当前 owner 和 Room FIFO 是 process state。重启时：

- 在途 / 排队输入会被标记为 skipped、cancelled 或 failed；
- pending approval 会过期；
- 不自动重放可能已有副作用的输入；
- 用户检查仓库状态后显式 Retry。

这是一项防止重复执行的安全选择，不是持久队列承诺。
