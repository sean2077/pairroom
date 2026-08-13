# PairRoom 升级与回退

## v0.2.0 → v0.3.0

v0.3.0 的 Store schema 为 `3`。升级重点是富对话、图片附件、Claude 原生审批和 Reviewer 原生保护。

首次打开 v0.2 房间时，PairRoom 会：

1. 重放原有 `events.jsonl`；
2. 保留消息、角色、路由、Delivery/Processing、Claude session ID 和 Codex thread ID；
3. 创建受限权限的 `attachments/` 媒体目录；
4. 将 metadata schema 升级为 3；
5. 对上次异常退出遗留的 pending/working/approval 状态执行与 v0.2 相同的明确收口；
6. 继续以追加事件的方式记录升级结果，不重写历史 JSONL。

旧消息没有附件字段时按空数组处理，不需要手工迁移。

## 状态目录变化

v0.3 房间目录：

```text
pairroom/rooms/<repo-name>-<path-hash>/
├── events.jsonl
├── metadata.json
├── attachments/
│   ├── att-<opaque-id>.json
│   └── att-<opaque-id>.<ext>
└── runtime/
    └── claude-pairroom-prompt.md
```

备份时必须同时复制 `attachments/`。只复制 `events.jsonl` 会留下可见的附件引用，却丢失图片内容。

## 升级前

1. 停止 PairRoom；
2. 复制完整房间目录；
3. 记录当前二进制版本；
4. 确认备份不位于将被 Agent 修改的工作树内。

## 升级后检查

```bash
pairroom version
pairroom doctor --repo /path/to/repo
pairroom serve --repo /path/to/repo --auto-start=false
```

在网页中检查：

- 历史消息和角色；
- Claude session / Codex thread ID；
- 旧在途状态是否已标记 cancelled/skipped；
- 旧审批是否 expired；
- 图片上传、粘贴、删除未发送图片和灯箱；
- Markdown、代码块、表格、引用和线程视图；
- Runtime 警告与 Reviewer 原生策略。

确认后再启动真实 Agent。

## v0.1.0 → v0.2.0 → v0.3.0

可以直接使用 v0.3 打开 v0.1 数据目录。迁移按顺序应用：先补齐 v0.2 的 Delivery/Processing 和 schema 2 语义，再升级到 schema 3。

## 回退

不建议让旧版与新版交替写同一目录。v0.2 不理解 schema 3 的附件生命周期和 Claude 审批语义，并会拒绝打开未来 schema。

需要回退时：

1. 停止 v0.3；
2. 保留当前目录作为取证副本；
3. 恢复升级前完整备份到新目录；
4. 让旧版 `--data-dir` 指向该备份。

不要手工降低 `metadata.json` 的 schema 数字。数字变化不会移除新事件，只会让旧版产生不完整投影。
