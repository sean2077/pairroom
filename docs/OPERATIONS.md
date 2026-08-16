# PairRoom 运维手册

> [文档首页](README.md) · [快速上手](GETTING_STARTED.md) · [CLI 参考](CLI_REFERENCE.md) · [排障](TROUBLESHOOTING.md) · [升级与回滚](UPGRADING.md)

本文面向长期运行 PairRoom 的单机管理员。所有命令示例都假设 PairRoom 仅监听数字 loopback 地址；远程使用通过 SSH 本地端口转发完成。

## 1. 选择运行方式

| 方式 | 适合 | 生命周期 |
|---|---|---|
| `pairroom service` | 调试、短期运行、观察启动输出 | 当前终端拥有进程 |
| `pairroom daemon install` | 日常后台运行 | systemd / launchd / Task Scheduler 托管 |
| `pairroom serve --repo ...` | 单仓库兼容、数据修复前检查 | 当前终端拥有单 Room |
| `--mock` | 无 Vendor CLI 的产品验证 | 不访问真实模型 |

生产式日常使用建议：先用前台 `service --mock` 验证数据根和浏览器，再以前台真实模式做 smoke，最后安装 daemon。

## 2. 上线前检查

### 2.1 二进制与环境

```bash
pairroom version --json
which pairroom        # Windows 使用 where pairroom
pairroom doctor --repo /absolute/path/to/safe-test-repo --json
```

确认：

- 实际执行的是预期二进制；
- `git`、`claude`、`codex` 可被后台环境中的 `PATH` 找到；
- Vendor CLI 已分别完成登录；
- 代理、证书和组织策略在非交互后台环境中同样有效；
- 测试仓库不是生产敏感仓库。

`doctor` 验证可执行文件与协议面，但不创建真实模型 Turn。上线前仍应在非关键仓库完成一次真实交互。

### 2.2 数据根

显式数据根必须是绝对路径：

```bash
pairroom service --data-root /absolute/path/to/pairroom-data --mock
```

不要把数据根放在：

- 会被仓库清理脚本删除的目录；
- 自动公开同步的位置；
- 多台机器同时写入的网络盘；
- 不支持可靠 rename/fsync/权限语义的临时文件系统。

一个数据根只允许一个 Service 持有 `service.lock`。

### 2.3 端口与监听

允许：

```text
127.0.0.1:7332
[::1]:7332
127.0.0.1:0
```

拒绝：

```text
0.0.0.0:7332
192.168.x.x:7332
localhost:7332
host.example:7332
```

`localhost` 也会被拒绝，因为实现要求可验证的数字 loopback，而不是依赖主机名解析。

## 3. 前台 Service

推荐的首次真实启动：

```bash
pairroom service \
  --listen 127.0.0.1:7332 \
  --runtime-limit 2 \
  --idle-timeout 15m \
  --shutdown-timeout 10m
```

启动输出包含 Management URL、data root、Runtime policy 和模式。不要把完整 URL 粘贴到公开日志；fragment 中含 Management Token。

常见策略：

- 小型本机：`--runtime-limit 1` 或 `2`；
- 同时维护多个活跃 Room：按内存、子进程和供应商并发限制逐步提高；
- 不希望页面打开即启动 Agent：`--auto-start=false`；
- 首选显式提及接力：`--routing mentions`；
- 禁止长时间无事件告警：`--stall-warning-seconds -1`，否则使用 30–86400 秒。

Runtime limit 允许 1–128，但这只是输入范围，不代表机器或供应商适合运行 128 个 Room。

### 3.1 停止

向前台进程发送 SIGINT/SIGTERM。Service 按顺序：

1. 停止接受新的 Management mutation/provisioning；
2. 等待在途管理请求退出；
3. 等待活动 Room Turn 自然完成并挂起 Runtime；
4. 关闭 Room Engine/Store；
5. 释放 `service.lock`。

`--shutdown-timeout` 会先用于等待 Management handler，再用于等待 Room Runtime drain；它是每阶段预算，不是精确的端到端 SLA。daemon 的操作系统 stop budget 由该值再加一分钟推导。普通容量回收、idle timeout、切换 Room 和正常 shutdown 不应为了更快退出而主动 interrupt 活动 Turn。

## 4. 安装为后台 daemon

### 4.1 安装

```bash
pairroom daemon install --runtime-limit 4 --idle-timeout 20m
pairroom daemon open
pairroom daemon status
pairroom daemon logs -f
```

也可以用 `--` 明确区分 daemon 选项与 Service 选项：

```bash
pairroom daemon install \
  --log-max-size 10485760 \
  --log-max-backups 3 \
  -- \
  --data-root /absolute/path/to/pairroom-data \
  --runtime-limit 4 \
  --idle-timeout 20m \
  --routing mentions
```

安装时会固定：

- 当前 PairRoom 二进制的绝对路径；
- 工作目录；
- 当前 `PATH` 和受支持的代理环境变量；
- 日志与 daemon 控制文件；
- 完整 Service 参数；
- `--no-browser` 后台行为。

因此升级二进制位置或修改任何参数时，不能只 `restart`。

### 4.2 打开页面、查看状态和日志

```bash
pairroom daemon open
pairroom daemon status
pairroom daemon logs
pairroom daemon logs -f
pairroom daemon logs -n 300
```

`daemon open` 从受保护的当前及轮转日志中解析候选 Management URL，只接受带 bootstrap token 的数字 loopback HTTP 地址，并用 Bearer Token 验证当前 Service 后才交给默认浏览器；Token 不复制到 daemon metadata。应用日志默认约 10 MiB 轮转并保留 3 个备份；可在安装时调整。操作系统服务管理器日志与 PairRoom 应用日志可能是两条信息源，排障时都应检查。

### 4.3 修改配置

`daemon restart` 只重启已安装定义：

```bash
pairroom daemon restart
```

修改 Runtime limit、data root、listener、Token、代理、Agent command/model、routing 或日志策略时，必须重装完整定义：

```bash
pairroom daemon install --force -- \
  --data-root /absolute/path/to/pairroom-data \
  --listen 127.0.0.1:7332 \
  --runtime-limit 4 \
  --idle-timeout 20m \
  --shutdown-timeout 10m \
  --routing mentions
```

先记录现有定义，再重装。只复制一段局部示例可能意外丢失原有 Token、代理或 Agent 参数。

### 4.4 停止与卸载

```bash
pairroom daemon stop
pairroom daemon start
pairroom daemon uninstall
```

卸载服务定义不会自动删除 Service data root。先确认不再需要 Room 数据和备份，再按组织策略删除。

### 4.5 stale lock

崩溃可能留下 `service.lock`。不要在未确认旧进程状态时删除或恢复它。

先检查：

```bash
pairroom daemon status
# 再使用系统进程/服务管理工具确认旧 PairRoom 已退出
```

确认后显式授权：

```bash
pairroom daemon start --recover-stale-lock
# 或前台
pairroom service --recover-stale-lock
```

若旧进程仍在运行，强行恢复会制造两个写者。

## 5. 浏览器访问与凭据

### 5.1 Management Shell

Management URL 的 fragment 含启动 Token。页面读取后立即移除 fragment，用 Bearer 调用 `/api/v1/session`，再把 Token 换成 Service-scoped `HttpOnly`、`SameSite=Strict` Session Cookie；后续浏览器请求使用 Cookie，mutation 还需要内存 CSRF Token。CLI/API 客户端可继续直接使用 Bearer Header。

运维含义：

- 刷新页面可恢复仍有效的 browser session；Service 重启、会话过期、注销或 Cookie 被清理后重新取得完整 URL；
- 已安装 daemon 优先运行 `pairroom daemon open`，前台模式从安全启动输出取得完整 URL；
- 不要把完整 URL、Authorization Header 或 Service diagnostics 原始文件公开；
- Management UI preference 也不跨刷新持久化。

### 5.2 Room View

Service 激活 Room 后返回独立 Room URL。Room 使用自己的 listener 与 Token。启用 Token 时，浏览器把 fragment 启动凭据交换为 HttpOnly Session Cookie，并在内存中持有 CSRF Token。

Room A 的凭据、cookie、SSE cursor、附件和草稿不适用于 Room B。

### 5.3 远程访问

PairRoom 不内建 TLS，也拒绝 LAN/公网绑定。使用 SSH local forwarding：

```bash
ssh -N -L 7332:127.0.0.1:7332 user@remote-host
```

随后在本地浏览器打开远端 Service 启动时打印的路径与 fragment，但把 origin 改成本机转发端口。不要通过聊天工具发送完整含 Token URL。

注意：激活后的 Room Runtime 使用另一个 loopback 端口。远程打开 Room 时也需要为对应 Room 端口建立转发；当前产品没有远程反向代理或单端口 multiplexing。可从 Management snapshot/Room URL 获得实际端口，再新增本地转发。

## 6. Runtime 容量管理

Management Shell 的 Runtimes 页面区分：

- phase；
- busy；
- occupies capacity；
- queue position；
- last used；
- error/cleanup state。

达到容量时：

1. Service 尝试挂起最久未使用且 idle 的 Runtime；
2. 所有 slot 都 busy 时，新激活进入 FIFO；
3. active Turn 不被中断；
4. queued Runtime 可安全取消；
5. active+idle 可安全挂起；
6. busy 或 cleanup uncertain 返回冲突。

遇到“容量已满”不要直接杀子进程。先在 Runtimes 页面确认哪些实例真正 `occupies_capacity`，再安全挂起 idle Room 或等待 Turn 完成。

## 7. 数据布局

默认根目录位于操作系统用户配置目录下的 `pairroom`：

```text
<pairroom-root>/
├── service.lock
├── service-registry.json
└── rooms/
    └── <room-id>/
        ├── events.jsonl
        ├── metadata.json
        ├── attachments/
        └── runtime/
```

| 内容 | 重要性 | 恢复策略 |
|---|---|---|
| `events.jsonl` | Room 事实源 | 必须备份与严格校验 |
| `metadata.json` | Store schema/Room 元数据 | 与 Event Log 一起备份 |
| `attachments/` | 图片 bytes 与 manifest | 与 Event Log 一起备份 |
| `service-registry.json` | 可替换索引/checkpoint | 默认 Room 可从 Event Logs 重建 |
| `runtime/` | 可重建缓存/工作区 | 不作为主要备份内容 |
| `service.lock` | 单实例保护 | 不复制到恢复目标作为有效锁 |

自定义导入的 Legacy Room 位于默认 `rooms/` 扫描边界之外。Registry checkpoint 丢失后，它们需要再次显式导入。

## 8. 完整性、备份与恢复

内置数据工具以 **单个 Room 数据目录** 为单位，不是整个 Service 根目录。

### 8.1 校验

```bash
pairroom verify --data-dir /absolute/path/to/room --json
```

在以下时点执行：

- 升级前后；
- 备份前；
- 非正常关机后；
- 复制/迁移到另一块磁盘后；
- 怀疑附件或 Event sequence 损坏时。

### 8.2 备份

```bash
pairroom backup \
  --data-dir /absolute/path/to/room \
  --output /safe/location/room-backup.tar.gz
```

备份是自描述归档，会进行完整性检查。保存在 Room data 与仓库之外，并按项目敏感度加密、控制访问和设置保留期。

多 Room 备份应枚举每个 durable Room 目录并分别生成归档。不要仅备份 `service-registry.json`。

### 8.3 恢复到新目录

```bash
pairroom restore \
  --input /safe/location/room-backup.tar.gz \
  --data-dir /absolute/path/to/restored-room

pairroom verify --data-dir /absolute/path/to/restored-room --json
```

优先恢复到新目录，验证后再决定是否替换现有 Room。`--force` 只应在已保存旧目标且理解替换语义时使用。

### 8.4 把恢复 Room 重新纳入 Service

- 恢复到默认 `rooms/<room-id>` 边界时，停服后恢复，再启动 Service 让 Registry 重建/发现；
- 恢复到自定义路径时，通过 Management Shell 的 Legacy Import 显式导入；
- 不要手工给 Binding Identity 改名或把同一备份同时导入两次；全局唯一性检查会拒绝冲突。

## 9. Registry 恢复

若 `service-registry.json` 损坏：

1. 停止 Service；
2. 备份整个 data root；
3. 不修改 Room Event Logs；
4. 移走损坏 checkpoint；
5. 启动 Service，让它扫描默认 `rooms/` 重建；
6. 重新显式导入自定义 Legacy 路径；
7. 检查 Project、Room、Binding ownership 和 archived 状态。

若 Service 报告无法证明 checkpoint 与 Event Log 一致，不要绕过 fail-closed 检查；先生成副本并做结构化诊断。

## 10. 诊断

### 10.1 环境诊断

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/repo --json
```

### 10.2 Room 数据诊断

```bash
pairroom diagnostics \
  --data-dir /absolute/path/to/room \
  --output /safe/location/pairroom-diagnostics.tar.gz
```

该归档设计为不包含 transcript 正文和附件 bytes，但仍可能带环境相关路径、版本、结构化事件头和错误。分享前必须人工检查。

### 10.3 Service diagnostics

Management Shell 的 Settings → Service 可导出脱敏 snapshot，包含 build、Project/Room/Runtime 结构、policy、summary、capabilities 与 registry diagnostic；不包含 Room Event Log、消息正文、图片或 Vendor Transcript。

Service snapshot 与 Room diagnostics 解决不同问题，不能相互替代。

## 11. 升级与回滚

升级前：

1. 记录 `pairroom version --json`；
2. 停止 Service/daemon；
3. 对每个关键 Room 执行 `verify` 与 `backup`；
4. 保存 daemon 的完整安装参数；
5. 安装新二进制；
6. 若二进制路径或参数变化，`daemon install --force` 重建定义；
7. 先以 `--auto-start=false` 或 `--mock` 检查控制面，再激活非关键 Room；
8. 再做真实 Vendor smoke。

不要让旧二进制打开已被新 schema 写入的数据目录。回滚应恢复升级前备份到新目录。完整步骤见 [升级与回滚](UPGRADING.md)。

## 12. 故障响应

### 12.1 Service 无法启动

按顺序检查：

1. numeric loopback listener；
2. 端口占用；
3. data root 是否绝对、可写；
4. `service.lock` 与真实进程状态；
5. Registry/Event Log 错误；
6. daemon 环境与前台环境差异。

### 12.2 Room 无法激活

检查：

- Project 路径是否仍存在并可访问；
- Legacy Binding 是否 pending；
- Existing Session/Thread 是否能精确恢复；
- Runtime queue/capacity；
- Room Store/attachment integrity；
- Vendor CLI 协议 probe。

### 12.3 Agent 卡在 working

先看 Inspector 是否仍有 Runtime 事件和 pending approval。不要因为页面看似安静就直接删除状态。可在安全边界使用 UI interrupt；进程崩溃后重启会收口 orphaned Processing。详见 [排障手册](TROUBLESHOOTING.md)。

### 12.4 磁盘空间不足

立即停止创建新 Room/附件，等待在途写入安全结束并停服。不要在运行中手工截断 `events.jsonl` 或删除被引用附件。释放其他空间后对每个受影响 Room 运行 `verify`。

## 13. 最低运维清单

- listener 始终为 numeric loopback；
- Management URL/Token 不进入工单和公共日志；
- 每次 Vendor CLI 升级后运行 `doctor` 与真实 smoke；
- daemon 配置变更使用完整 `install --force`；
- stale lock 仅在确认旧进程退出后恢复；
- 关键 Room 定期 `verify` + 独立备份；
- diagnostics 分享前人工检查；
- 强隔离任务使用容器/VM/独立 checkout；
- 删除仓库时同时评估对应 PairRoom Room 和备份的保留策略。
