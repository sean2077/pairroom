# Multi-Project / Multi-Room Service

`pairroom service` 将 PairRoom 从“启动一个进程服务一个仓库/Room”的快捷模式扩展为与当前工作目录无关的本地常驻控制面。旧的 `pairroom serve --repo ...` 保留，用于兼容现有单 Room 工作流。

## 启动

```bash
pairroom service
```

常用参数：

```bash
pairroom service \
  --listen 127.0.0.1:7332 \
  --runtime-limit 4 \
  --idle-timeout 20m \
  --shutdown-timeout 10m
```

Service 只接受 loopback 监听地址。未提供 `--token` 时会生成随机 Management API Bearer Token，并放在浏览器启动 URL 的 fragment 中；Management Shell 只在内存中使用它，不写入 `localStorage` 或 `sessionStorage`。
显式 `--data-root` 必须是绝对路径；未提供时始终使用操作系统用户配置目录下的 PairRoom 根目录，因此从不同 CWD 启动会打开同一份 Registry。

`--mock` 使用确定性 Mock Agent，可在没有供应商登录的机器上验证 Project、Room、队列、归档和恢复流程。真实模式继续调用用户本机官方 `claude` 与 `codex app-server`，不接管供应商凭据、Session Store 或 Transcript。

## Project Identity

Management Shell 只接受用户显式输入的绝对路径。服务端按以下顺序处理：

1. 要求路径为绝对路径并验证目录存在、可访问；
2. 解析符号链接；
3. 执行 `git rev-parse --show-toplevel`；
4. 再次 canonicalize Git worktree root；
5. 以 canonical root 生成 Project ID 并去重。

同一 worktree 的根目录、任意子目录和符号链接只能登记一次。Service 不提供服务器文件系统浏览器，也不会扫描用户常用开发目录。

## Room Provisioning 与 Binding

每个 Room 永久属于一个 Project，并恰好拥有：

- 一个 Claude 原生 Session Binding；
- 一个 Codex 原生 Thread Binding。

两侧可独立选择 `new` 或 `existing(session_id)`，因此支持四种组合。Binding Identity `(agent, vendor_session_id)` 在整个 Service 内全局唯一，归档 Room 仍保留所有权。

Provisioning 在隐藏暂存目录中完成。服务先验证两侧 Binding，再写入初始 append-only Event Log，随后以原子 rename 发布 Room。任一 Existing ID 无法精确恢复、任一 Binding 已被占用，或任一侧验证失败时，都不会出现可见 Room、Binding 索引或半成品数据目录。

Existing Binding 只恢复供应商原生 context。PairRoom 不读取、导入、复制、摘要、搜索或展示绑定前 Vendor Transcript；Room View 显示 transcript boundary 提示，PairRoom Event Log 仅从绑定成功后开始。

## Runtime Capacity

Room Runtime Manager 为每个激活 Room 创建独立的：

- Event Store 与 Attachment Store；
- Workspace Manager；
- Engine、Hub 与 Room HTTP/SSE 服务；
- Claude Adapter 与 Codex Adapter。

Runtime 按 Room ID 惰性激活。全局容量达到 `--runtime-limit` 时：

1. 优先挂起最久未使用且 idle 的 Runtime；
2. 若所有 Runtime 都有活动 Turn，新需求进入 FIFO 队列；
3. Management Shell 显示 phase、busy、queue position 和 Room URL；
4. 活动 Turn 不会为了释放容量而被中断。

Room 空闲超过 `--idle-timeout` 后释放 Agent 进程。再次打开或发送消息时，Runtime 以同一 durable Session/Thread ID 精确恢复，而不是创建新 Binding。切换浏览器中的 Room 不会触发 interrupt 或 stop。

## 生命周期

首版生命周期只有：

- 创建；
- 重命名；
- 归档；
- 恢复。

重命名、Binding 补全和归档会先等待活动 Turn 自然结束并挂起 Runtime，避免同一个 append-only log 同时存在 Engine 与控制面的两个写入投影。归档不删除 Event Log、附件或 Binding Identity。恢复后仍使用原有完整历史与绑定。

## 数据布局与恢复

默认数据根由操作系统用户配置目录决定，和启动目录无关：

```text
<pairroom-config-root>/pairroom/
├── service.lock
├── service-registry.json
└── rooms/
    └── <room-id>/
        ├── events.jsonl
        ├── metadata.json
        ├── attachments/
        └── runtime/
```

`events.jsonl` 是每个 Room 的事实源。`service-registry.json` 只是可替换 checkpoint：删除或损坏后，Service 会扫描默认 `rooms/`，从 Room Event Logs 重建 Project、Room 生命周期和 Binding Identity 索引。显式导入的自定义旧目录不在默认扫描边界内，因此 checkpoint 丢失后需要用户再次显式导入；导入仍只重建索引，不修改该 Room 的 Event Log。Checkpoint 写入失败且无法证明内存索引与已提交 Event Log 一致时，Registry 会 fail closed，阻止后续修改。

一个数据根只允许一个 Service 进程持有 `service.lock`。崩溃残留的 lock 不会被自动猜测为 stale；确认原进程已经退出后，用户可显式使用：

```bash
pairroom service --recover-stale-lock
```

## Legacy Room

首次启动 Service 时只扫描默认 PairRoom Room 数据根，不移动、不复制、不重写旧 `events.jsonl`。可从旧事件恢复出的 Session/Thread ID 会成为 durable Binding。

缺少任一原生 ID 的 Legacy Room 会标记为 pending，并阻止 Runtime 激活。Management Shell 提供一次性的 Binding 补全操作；两侧先原子验证，成功后只追加一个 `service.room.bindings.completed` 事件。已存在的 durable Binding 不能被替换。

自定义旧 `--data-dir` 不会被全盘扫描。用户必须在 Management Shell 中显式输入绝对路径导入；导入只建立可重建索引，不修改旧 Room Event Log。

## 关闭顺序

收到 SIGINT/SIGTERM 时，Service：

1. 停止接受新的 Management 请求和 Provisioning；
2. 等待正在处理的管理请求退出；
3. 等待活动 Room Turn 完成，并挂起各 Runtime；
4. 关闭各 Room Engine/Store；
5. 释放 Service lock。

`--shutdown-timeout` 是整个优雅关闭阶段的上限；除显式用户中断外，容量回收、Room 切换、idle timeout 与正常关闭均不调用 Agent interrupt。

## API 摘要

Management API 需要 `Authorization: Bearer <token>`，拒绝 query-string token。主要端点：

```text
GET    /api/v1/service
POST   /api/v1/projects
POST   /api/v1/projects/{project}/rooms
POST   /api/v1/rooms/{room}/activate
POST   /api/v1/rooms/{room}/bindings
PATCH  /api/v1/rooms/{room}
POST   /api/v1/rooms/{room}/archive
POST   /api/v1/rooms/{room}/restore
POST   /api/v1/import
```

每个激活 Room 使用独立的 loopback HTTP 地址和独立 token，继续暴露现有 Room View、REST 与 SSE 协议；一个 Room 的 token、snapshot、SSE cursor、附件和草稿不能用于另一个 Room。
