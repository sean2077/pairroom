# PairRoom Agent 与 Room 协作协议

本文定义 Agent 输入输出和 Room lifecycle 的机械合同。调度、权限、持久化和取消必须由代码执行，不能只依赖模型自律。机器可读合同由：

```bash
pairroom protocol --json
```

输出。

## Actor 与角色

Actor 包括 `user`、`system`、`claude`、`codex`。Participant role 为 `driver`、`reviewer`、`peer`；角色决定 workspace/policy 边界，不只是提示词标签。

## Message

Message 是 append-only Timeline 记录，包含 ID、sequence、from/to、text、reply/retry、intent、thread、hop、Turn/Workflow correlation、Delivery/Processing 和 attachment metadata。

用户输入必须解析为一个 Agent/角色目标。`@All` 不启动两个 Runtime；多个 recipient 被拒绝。Agent 输出中的普通 `@claude`、`@codex`、`@peer` 只保留为可读文本。

## Intent

- `append`：尝试 steering active target Turn；
- `next_turn`：在下一 native Turn 执行；
- `supersede`：取代旧指令并按 native 能力中断。

Intent 不绕过 single-owner scheduler。

## Delivery 与 Processing

Delivery 是 transport-level：

```text
pending | started | injected | queued | failed | skipped
```

Processing 是 execution-level：

```text
waiting | working | completed | cancelled | failed | superseded
```

每个目标独立记录 detail、Turn ID 和更新时间。Retry 创建新 Message 并保留 `retry_of`；旧 Message 不被改写。

## Native Turn correlation

Adapter event 必须关联 Agent、native Turn 和 PairRoom Message。多个 steering input 可以共享一个 native Turn，但仍保留各自 Processing。

可靠 terminal boundary 包括 native completion、明确 cancel/abort 或确认 process exit。generic `RuntimeError` 不自行结算 input、过期 approval、释放 owner 或启动 peer。

意外进程退出时，Adapter 先把 outstanding input 收口为失败，再发送 process-exit Turn boundary，保证先有输入结算、后有 owner 释放。

## Handoff 与控制标记

Agent 需要交棒时输出：

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

Handoff 是给下一 Agent 的有界上下文包，完整回答仍留在公共 Timeline。缺失/过短 handoff、冲突标记、新用户指令或 hop 超限都会 fail closed。

收敛标记：

```text
[PAIRROOM:DONE]
[PAIRROOM:WAIT]
[PAIRROOM:BLOCKED]
```

旧 `CONTINUE`、`IMPLEMENTED`、`REVIEW_CHANGES`、`REVIEW_APPROVED` 不属于当前合同。

## Workflow extension

至少两个显式 actor/action pair 才编译为自然语言 Workflow。支持 `plan`、`review`、`execute`、`audit`、`discuss` 及对应中文动作词；最多保留 12 个 stage。

Stage policy：

- plan/review/audit：只读；
- execute：live Driver workspace；
- discuss：保留当前角色边界；
- 需要用户决定：进入 waiting-human / awaiting-approval，不隐藏等待 stdin。

执行批准绑定当前计划 revision；计划变更使旧批准失效。明确“无需批准/直接执行”可关闭自动 gate，但更强的“必须批准”表述优先。

## Approval

Claude control request 与 Codex command/file/additional-permission request 投影为统一 Approval。Approval 绑定 native request、Message 与 Turn correlation；未知高权限 request fail closed。

Interrupt、stop、process exit、role change 或 restart 会使不能安全复用的 pending Approval 过期。UI 不把旧决定重放给新 request。

## Attachment

只接受通过内容签名和尺寸校验的 PNG/JPEG/GIF/WebP。Message/Event 只保存 opaque ID 与 presentation metadata；绝对路径仅在 native Adapter boundary resolve。

Claude 接收多模态 image content，Codex 接收 `localImage`。Agent 最终 Markdown 引用的本地图片只有在 canonical path 位于仓库内且无 symlink escape 时才导入。

## Role / Workspace

Driver 可按授权修改 live workspace。Reviewer 在独立 snapshot 中运行，并使用 Vendor 只读 policy。Reviewer 必须独立验证 handoff 事实，不把 peer 自述当作完成证据。

Role change 在安全 Turn 边界生效；Workspace policy 未成功应用时不得先持久化角色成功。

## Persistence 与 restart

Durable Event 先 append 再发布。Room FIFO、native process、transient delta 和 connection-local request ID 不持久化。Vendor 启动阶段返回的候选 Session/Thread ID 也不自动等于 durable Binding：例如未接受首个 Turn 的 Codex Thread 没有 rollout，进程退出后应丢弃候选 ID并重新创建。重启后未完成输入 fail closed，不自动 replay；用户检查工作区后显式 Retry。

## User precedence

新用户输入、明确取消/拒绝、冲突控制标记和 hop 上限优先于旧自动接力。PairRoom 不允许 Agent 通过输出文本绕过用户 gate、角色 policy 或 single-owner scheduler。

## Authority

```text
user decision
  > repository/native runtime facts
  > durable PairRoom state
  > peer handoff
  > model inference
```

实现事实源：`internal/model/`、`internal/room/`、`internal/agent/`、`internal/protocol/` 与 `internal/store/`。
