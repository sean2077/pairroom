# Storage and recovery

## Durable 与 ephemeral

| 类型 | 示例 | 重启后 |
|---|---|---|
| Durable | Room metadata、message、delivery / processing projection、role、Workflow、Turn summary、resolved approval、Binding、attachment metadata | 从 Event Log / registry 重放 |
| Ephemeral | native process、当前 stdout connection、供应商 request ID、active owner、Room FIFO、transient text delta | 不恢复 |

PairRoom 的 durable transcript 不等同于持久任务队列。任何可能已产生副作用但未确认 terminal 的输入，都不会在重启后自动再执行。

## Event Log

Room 使用 append-only JSONL store。启动时按序重放 event，重建 projection；不合法 event 或不支持的 routing state 应明确失败，而不是猜测修复。

schema 的事实源是 `internal/model/types.go`、事件写入 / apply 代码和 `internal/store/`，不是文档中手写的虚构 schema 文件。

高频 transient telemetry 可以不落盘，以免逐 token fsync 阻塞 native stdout reader。需要审计的状态转换必须 durable。

## Restart

进程异常退出或重启后：

1. native process 状态重置；
2. pending delivery / processing 被结算为 fail-closed 状态；
3. connection-local pending approval 过期；
4. Room FIFO 不自动重建；
5. 用户检查工作区与 Event Log，再创建新的 Retry message。

Retry 必须生成新的 auditable message ID，不能复用旧 ID，否则迟到的 vendor event 会产生歧义。

## Attachment

Event Log 只保存经过验证的 presentation metadata 和 opaque attachment ID。绝对宿主路径仅在 adapter boundary 解析，不进入 API transcript。附件有数量、类型和总大小限制。

## Backup 与 Restore

备份前应停止或归档相关 Room，避免将“文件已复制”误认为“外部副作用已完成”。恢复流程应验证：

- manifest / checksum；
- Project path 是否仍存在；
- Binding 对应的 native session 是否可恢复；
- Event Log 是否能完整重放；
- 旧 routing / schema 是否被当前 release 支持。

恢复备份不会启动未完成的旧 FIFO item。

## Corruption handling

不要直接编辑生产 JSONL。先复制数据目录，保留原始失败证据，再使用 diagnostics / backup 验证定位首个无效 event。无法安全迁移的 Room 应重建，而不是跳过中间 event 后继续运行。
