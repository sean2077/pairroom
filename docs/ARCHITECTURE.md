# PairRoom 架构

PairRoom 是官方 Claude Code / Codex Harness 之上的本地控制面。它解决会话绑定、顺序调度、角色隔离、审批、持久化、观察与恢复，不重新实现供应商 Agent loop。

## 组件图

```text
Browser / CLI
      |
Management HTTP          Room HTTP/SSE
      |                       |
Service Registry ------ Runtime Manager
                              |
                    Room Engine + Event Store
                              |
                    single-owner FIFO scheduler
                         /                 \
              Claude Adapter          Codex Adapter
                    |                       |
             official claude       official codex app-server
```

`pairroom serve` 省略上层 Registry / Runtime Manager，直接装配一个 Room Runtime；Room 内核仍相同。

## 模块职责

| 目录 | 职责 |
|---|---|
| `cmd/pairroom/` | CLI、配置覆盖、启动装配和数据工具 |
| `internal/service/` | Project/Room Registry、Binding ownership、provisioning、Runtime capacity、Management API |
| `internal/room/` | Event projection、Message lifecycle、single-owner scheduler、Workflow 与 Approval |
| `internal/agent/` | Claude stream-json/control、Codex app-server、Mock 与 probe |
| `internal/server/` | Room REST/SSE、Room View、附件和浏览器 Session |
| `internal/store/` | append-only JSONL Store 与 replay |
| `internal/archive/` | verify、backup、restore、diagnostics |
| `internal/attachment/` | 图片验证、immutable object 与安全解析 |
| `internal/workspace/` | Driver live tree 与 Reviewer snapshot |
| `internal/config/` | JSON 配置、Provider 与 cc-connect 引用导入 |
| `internal/model/` | 跨层 durable model、Event payload 与 schema |

## 状态权威

| 状态 | 权威来源 |
|---|---|
| Project、Room、Binding ownership | Room service events + Registry rebuild/checkpoint |
| Message、角色、Workflow、Approval、Turn summary | Room Event Log |
| native process、当前 request、stdout connection | Agent Adapter |
| active owner 与 Room FIFO | Room Engine process state |
| live source tree | Driver worktree |
| Reviewer 文件视图 | Reviewer snapshot |
| 页面显示 | 服务端 projection；浏览器不是 SSOT |

`service-registry.json` 是可重建 checkpoint，不是 Room 历史事实源。

## Single-owner scheduler

Room Engine 把跨 Agent 和 `next_turn` 输入放入一个 FIFO。Scheduler reserve item 后，在 native submit 前再次检查取消状态；只有当前 owner 到达可靠 terminal boundary 才释放 baton。

不变量：

1. 一个 Room 最多一个 active native Turn owner；
2. generic diagnostic error 不释放 owner；
3. Delivery accepted 不等于 Processing terminal；
4. FIFO cancellation 不 interrupt Runtime；
5. native cancellation 不删除无关 FIFO；
6. restart 不自动重放 process-memory FIFO；
7. 用户新输入使 stale auto-handoff fail closed。

## Terminal boundary

Codex App Server 可以在 Turn 中途发 generic `error`，随后才发 `turn/completed`。Adapter 因此把 diagnostic `RuntimeError` 与 lifecycle boundary 分开。

意外进程退出时，Adapter 先结算 outstanding input，再发出明确 process-exit Turn boundary，保证 event order 和 correlation；Engine 之后才可启动 peer。Claude 侧同样必须以结构化 completion / exit，而不是 stderr 文本作为终止事实。

## Event-before-effect

需要审计的控制面事实应先 append durable event，再驱动外部副作用。供应商 connection-local request ID 不能作为跨重启 durable identity；迟到 event 必须通过 Turn/message correlation 过滤，不能重开已结算工作。

例外必须被明确建模：Vendor 可以在 durable context 真正形成前返回候选 Session/Thread identity。PairRoom 使用 pending Binding + 明确的 materialization boundary 收口这个外部先行事实；候选 identity 不能仅因一次启动响应就成为 durable ownership。

## Provisioning 与 Binding

Existing Binding 在 Room 发布前精确验证。New Binding 只有在首个 native input 被接受、Vendor context 已可持久恢复后才 materialize，并在全局 ownership/checkpoint 边界内提交。Codex `thread/start` 返回的未 engaged Thread 只存在于当前 app-server 内存；若进程在首个 accepted Turn 前退出，Adapter 丢弃该候选 ID，下次 activation 重新创建，而不是尝试恢复一个没有 rollout 的 orphan Thread。

受管 Room provisioning 使用隐藏 stage directory、初始 Event Log、atomic rename 与 Registry checkpoint，避免可见半成品。外部 legacy Room import 不搬移或重写源目录。

## Runtime Manager

Runtime Manager 控制 activation、capacity、idle suspend、failed-retained、shutdown drain 和 cleanup uncertainty。容量策略可以回收 idle Runtime，不能抢占 active Turn；无法证明 cleanup 完成的 Runtime 继续占容量并暴露诊断。

## Workspace 与角色

Driver 使用 live worktree。Reviewer snapshot 捕获 HEAD、dirty tracked patch 和 untracked regular files，并拒绝不安全 symlink/越界。角色切换、snapshot refresh 与 delivery serialization 共享安全边界，避免 Reviewer 捕获时 Driver 仍在写。

Reviewer policy 是 defense in depth，不是容器级隔离。

## Web 与认证

Management 与 Room 是两个 server/session scope。两者只监听 loopback，并使用 Bearer bootstrap、HttpOnly cookie、CSRF、Origin/Sec-Fetch 与安全响应头。Attachment resolve 会复核 metadata、regular file、size、dimension 与 SHA-256。

SSE 同时包含 durable event 与 transient telemetry。页面断线后以 snapshot + durable sequence 恢复，不依赖临时 DOM。

## Failure model

- Event Log 中间损坏、sequence 分叉、future schema 和 unsupported routing event 会拒绝启动；
- 仅损坏的最后半行可在受控打开路径中修复；
- Registry checkpoint 缺失/损坏通常可由 Room Event Log 重建；
- restart 收口 orphaned Processing/Approval，但不自动 replay FIFO；
- destructive Room deletion 使用 durable intent、quarantine、checkpoint 与 committed marker 处理 crash window。

数据细节见 [Storage](STORAGE.md)，Agent wire 见 [Protocol](PROTOCOL.md) 与 [Runtime Compatibility](RUNTIME_COMPATIBILITY.md)。

## 验证层次

- unit/fixture：parser、state transition、correlation、wire serialization；
- race：scheduler、cancel、terminal boundary、Registry 与 lifecycle locks；
- Mock smoke：完整控制面、Room、Store、附件、Reviewer、backup/restore；
- doctor：本机 executable 与必需协议面；
- native smoke：真实官方 CLI、账号、网络与供应商行为。

Mock、build 或 cross-compile success 不得被描述为 native E2E。
