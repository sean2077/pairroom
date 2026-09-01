# PairRoom 存储与恢复模型

本页定义 durable / ephemeral 数据边界、目录布局和恢复语义。操作命令见 [Operations](OPERATIONS.md)，Event/Message 字段见 [Protocol](PROTOCOL.md)。

## Service 数据根

默认数据根为 `os.UserConfigDir()/pairroom`；`pairroom service --data-root` 可指定绝对路径。

典型布局：

```text
<data-root>/
├── service-registry.json
├── service.lock
└── rooms/
    ├── <room-id>/
    │   ├── metadata.json
    │   ├── events.jsonl
    │   ├── attachments/
    │   │   ├── <attachment-id>.json
    │   │   └── <attachment-id>.<ext>
    │   └── runtime/                 # 可重建运行时材料
    └── .deleted-rooms/              # 永久删除 crash recovery quarantine
```

外部 legacy Room 可以登记任意绝对数据目录；Service 不搬移或重写它。

## Room Event Log

`events.jsonl` 是 append-only Room 事实源。每行一个 Event，sequence 必须从 1 连续递增，Room ID 一致，Event ID 唯一。

写入顺序：

```text
serialize event
-> append
-> file sync
-> update in-memory projection
-> publish SSE
```

启动 replay 拒绝：

- 中间 JSON 损坏或未终止行；
- sequence gap / duplicate；
- future store schema；
- 不支持的 routing event；
- Event 不能应用到当前状态机。

只允许安全截断崩溃留下的最后半行；`pairroom verify` 本身只读，不修复。

## Metadata

`metadata.json` 标识 `pairroom-jsonl` format、store schema 和写入版本。它不是 Room projection；Room ID、Binding、lifecycle、Message 等事实来自 Event Log。

## Service Registry

`service-registry.json` 是原子替换的 checkpoint，保存显式注册 Project、Room 索引和恢复所需 metadata。启动时 Room 与 Binding ownership 仍从各 Room Event Log 重建；checkpoint 缺失或损坏通常不会抹去 Room 历史。

若 checkpoint 已替换但目录 sync 失败等边界无法证明，Registry 会 fail closed，而不是继续接受写入。

## Durable 与 ephemeral

| Durable | Ephemeral / 可重建 |
|---|---|
| Room Event Log、metadata、附件 | native Claude/Codex process |
| Project/Room/Binding projection | active Turn owner、Room FIFO |
| Message、Workflow、Approval history | transient text delta、tool progress、usage telemetry |
| durable Turn summary | connection-local request/waiter/approval handle |
| lifecycle 与删除 intent | Reviewer runtime worktree、generated prompt file |

Restart 后未完成输入被收口为失败/取消/跳过，不自动 replay。用户必须检查 Git 工作区与外部副作用，再显式 Retry。

## Attachment Store

每个附件由 immutable bytes 和 JSON manifest 组成。保存时验证内容签名、允许格式、大小、像素尺寸与 SHA-256；读取时再次验证 regular file、manifest 和 hash。

Message/Event 只保存 opaque ID 和 presentation metadata，不保存绝对本机路径。尚未被 Message 引用的附件可删除；进入 durable Transcript 后只能随 Room 数据整体保留/删除。

## Reviewer runtime material

`runtime/` 下的 Reviewer worktree、临时 prompt 等不是历史事实，可在 Runtime 重建。不要把这些目录当作备份源或手工迁移对象。

## Verify

```bash
pairroom verify --data-dir /absolute/path/to/room --json
```

Verify 检查 metadata、Event sequence / ID / Room ID、attachment manifest/bytes/hash、引用完整性和总量；不启动 Runtime、不修改目录。

## Backup

```bash
pairroom backup --data-dir /absolute/path/to/room --output room.tar.gz
```

Backup 在打包前验证 Room，生成 `pairroom-backup` manifest，为每个文件记录 path、size 与 SHA-256。它面向一个 Room，不包含 Service Registry、daemon 定义、Vendor Session 内容或 Git worktree。

创建一致备份前应先停止对该 Room 的写入：归档/挂起并确认 Runtime settle，或停止 Service。

## Restore

```bash
pairroom restore --input room.tar.gz --data-dir /absolute/path/to/restored
```

Restore 使用 staging directory，拒绝 traversal、link、duplicate path、超限文件/总大小和 checksum mismatch；完整验证后才原子发布。`--force` 也不会绕过 archive validation。

恢复目录不会自动登记到 Service。可通过 Management legacy import 或相应迁移流程纳入 Registry。

## Diagnostics

Diagnostics bundle 包含版本、OS/architecture、Verify report、有界 Event header tail 和说明；默认不包含 Message 正文和附件 bytes。它不是备份，也可能含本机路径和错误信息，分享前需人工检查。

## Archive 与永久删除

Archive 是 durable lifecycle event，不删除数据，也不释放 Binding ownership。

永久删除只允许 archived Room：

1. 写入 durable deletion intent；
2. 将数据移动到 `.deleted-rooms/` quarantine；
3. 发布 Registry checkpoint，逻辑删除 Room/ownership；
4. 写 committed marker；
5. 删除 quarantine bytes。

Checkpoint 前失败会尝试恢复 Room；checkpoint 已可信排除 Room 后，物理清理失败不会让 Room 复活，而会留下 pending cleanup，供 maintenance API 重试。

删除 Git worktree、Vendor Session/Thread 和外部 import 目录都不属于该操作。
