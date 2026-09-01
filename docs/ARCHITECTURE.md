# Architecture

## 目标

PairRoom 是 native Agent harness 之上的本地控制面。它解决会话绑定、顺序调度、角色隔离、审批、持久化、观察和恢复，不重新实现 Claude Code / Codex 的工具循环。

```text
Browser / CLI
      |
HTTP + SSE control plane
      |
Service registry ---- Project / Room / Binding metadata
      |
Room Engine --------- append-only Event Log + projections
      |
deterministic FIFO scheduler
      |
Claude adapter       Codex adapter
      |                  |
official native CLI / session / tools / approvals
```

## 状态所有权

| 状态 | 权威来源 |
|---|---|
| Project / Room 注册与 Binding | Service registry 与 Room service events |
| 消息、审批、角色、Workflow、Turn summary | Room Event Log |
| 当前 native process / stdout / request ID | Agent adapter |
| live source tree | Driver workspace |
| review filesystem view | Reviewer snapshot |
| 页面显示 | 服务端 projection；浏览器不是 SSOT |

不允许 UI、prompt 或内存 cache 覆盖 durable authority。

## 关键不变量

### Single owner

一个 Room 最多一个 active native Turn owner。跨 Agent message 和 `next_turn` 进入 Room FIFO；scheduler 只有在可靠 terminal boundary 后才提交下一项。

### Diagnostic is not terminal

Codex 的 generic `error` notification 可以发生在 `turn/completed` 之前。因此 `RuntimeError` 是诊断，不是自动释放 owner 的依据。进程异常退出时，adapter 先结算 outstanding input，再发出明确 process-exit boundary。

### Cancellation is stage-aware

- FIFO 中：只取消目标 item；
- scheduler 已 reserve、尚未 submit：submission boundary 再次检查取消；
- native runtime 已接受：中断范围可能扩大到当前 Agent Turn，但不得清空无关 Room FIFO。

### Event-before-effect

对用户可见且需要审计的控制面事实应先写 Event Log，再驱动外部副作用。供应商临时 request ID 不能作为跨重启 durable key。

## 主要模块

- `cmd/pairroom/`：CLI 与启动装配；
- `internal/service/`：Project / Room lifecycle、Binding 和 runtime capacity；
- `internal/room/`：Event Log projection、scheduler、Workflow 与审批；
- `internal/agent/`：Claude / Codex native protocol adapter；
- `internal/server/`：Management Shell、Room View、HTTP 与 SSE；
- `internal/store/`：JSONL persistence；
- `internal/archive/`：archive / backup 相关实现；
- `internal/model/types.go`：跨层 durable model。

## Runtime lifecycle

Service 可以按 capacity 和 idle policy 激活或回收 Room runtime。回收 native process 不删除 Room；重新激活会恢复 durable projection 和 session binding，但不会自动重放进程内 FIFO。

Role / workspace 切换必须与 delivery serialization 使用同一安全边界，避免 reviewer snapshot 捕获时 Driver 仍在修改 live tree。

## 浏览器更新模型

SSE 传输 durable state event 与 transient telemetry。页面应增量更新或批量合并高频活动，不因每个 token 重建全部聊天树；重连时以 snapshot 为准，不能依赖浏览器中遗留的临时状态。

## 非目标

- 不提供分布式多节点队列；
- 不承诺任意旧 Event Log 自动迁移；
- 不把两个 Agent 变成无边界群聊；
- 不隐藏 native CLI 的权限、审批和失败；
- 不以 Mock E2E 代替真实 vendor E2E。
