# PairRoom 升级与回退

## v0.1.0 → v0.2.0

v0.2.0 的 Store schema 为 `2`。首次打开 v0.1 房间目录时，PairRoom 会：

1. 重放原有 `events.jsonl`。
2. 为旧消息补齐 Delivery/Processing 生命周期默认值。
3. 将无法跨进程确认的 `waiting` / `working` 处理标记为 `cancelled`。
4. 将尚未提交的 `pending` delivery 标记为 `skipped`。
5. 将连接级 pending approval 标记为 `expired`。
6. 保留 Claude session ID、Codex thread ID、消息、角色与房间设置。
7. 将 `metadata.json` 升级到 schema 2，并写入当前 PairRoom 版本。

这些收口事件会追加到日志，不会重写历史事件。

## 升级前备份

先停止 PairRoom，再复制完整房间目录：

```text
pairroom/rooms/<repo-name>-<path-hash>/
├── events.jsonl
├── metadata.json
└── runtime/
```

使用显式 `--data-dir` 时，备份该目录即可。不要在进程运行中只复制 `events.jsonl` 而遗漏刚写入的 metadata 或 runtime 文件。

## 升级后检查

```bash
pairroom version
pairroom doctor --repo /path/to/repo
pairroom serve --repo /path/to/repo --auto-start=false
```

打开房间后检查：

- 历史消息和角色是否存在。
- 原有 Claude session / Codex thread ID 是否显示。
- 旧的在途消息是否被明确标记为取消，而不是继续显示 Working。
- 旧审批是否显示 expired。
- 两个 Runtime 的版本、协议和 warnings。

确认后再启动 Agent。

## 回退

不建议让 v0.1 与 v0.2 交替写入同一数据目录。v0.1 不理解 ProcessingState、RuntimeInfo、retry 和 schema 2 的完整语义。

需要回退时：

1. 停止 v0.2。
2. 保留当前目录作为取证副本。
3. 恢复升级前备份到一个新目录。
4. 用 v0.1 的 `--data-dir` 指向该备份副本。

不要手工降低 `metadata.json` 中的 schema 数字；这不会删除 v0.2 事件，反而可能让旧版本对新事件做不完整投影。

## 未来 schema

v0.2 会拒绝打开高于自身支持版本的 Store schema。该策略用于防止旧二进制静默损坏新格式数据。遇到该错误时，应升级 PairRoom 或使用与该数据目录匹配的版本，而不是修改 metadata。
