# PairRoom 升级、迁移与回滚

> [文档首页](README.md) · [CLI 参考](CLI_REFERENCE.md) · [运维手册](OPERATIONS.md) · [排障](TROUBLESHOOTING.md) · [Changelog](../CHANGELOG.md)

本文覆盖三类变化：Room Store schema 升级、前台/后台 Service 二进制升级，以及旧单 Room 数据迁移到多 Project / 多 Room Service。

## 1. 基本原则

- Event Store 迁移通过 replay 与显式默认值完成，不重写历史事件；
- 不手工编辑 `metadata.json`、Event sequence、attachment manifest、Turn summary 或 Binding Identity；
- 新二进制写入后，不让旧二进制直接打开同一数据目录；
- 回滚使用升级前备份恢复到新目录；
- Service 数据工具以单个 Room 为单位，关键 Room 必须逐一校验/备份；
- daemon `restart` 不更新二进制路径或 Service 参数，变化时重装完整定义。

## 2. 确认当前运行形态

```bash
pairroom version --json
pairroom daemon status
```

判断你在使用：

- 前台 `pairroom service`；
- 系统托管 `pairroom daemon`；
- 兼容 `pairroom serve --repo ...`；
- 旧自定义 `--data-dir`。

同时记录：

- PairRoom version/commit/build date；
- data root 或 Room data-dir；
- daemon 完整安装参数、二进制路径与日志路径；
- Vendor CLI version；
- 关键 Project/Room/Binding；
- 当前 Runtime policy 与 routing。

## 3. 升级前备份

### 3.1 停止写入

优先正常停止 Service/daemon，让 Management mutation 与活动 Turn 排空：

```bash
pairroom daemon stop
# 或前台 Ctrl-C / SIGTERM
```

不要在 Runtime 仍 active 时复制 `events.jsonl`。

### 3.2 校验每个关键 Room

```bash
pairroom verify --data-dir /absolute/path/to/room --json
```

如果 verify 失败，先保留原始副本和 diagnostics，不要用升级来“修复”未知损坏。

### 3.3 创建自验证备份

```bash
pairroom backup \
  --data-dir /absolute/path/to/room \
  --output /safe/location/room-before-upgrade.tar.gz
```

每个关键 Room 单独备份。把归档放到 data root、仓库和自动清理目录之外。

### 3.4 保存 Service/daemon 配置

`daemon install --force` 需要完整定义。记录：

- `--data-root`、`--listen`、`--token`；
- `--runtime-limit`、`--idle-timeout`、`--shutdown-timeout`；
- routing、hop、stall warning；
- Claude/Codex command/model/permission/sandbox；
- proxy/PATH/work-dir；
- log path、size、backup count。

不要只保存一段局部命令。

## 4. 升级二进制

从源码：

```bash
make build
./dist/pairroom version --json
```

安装到 Go bin：

```bash
make install
pairroom version --json
```

确认 shell 实际解析到的新路径。若 daemon 固定的是旧 binary path，即使 shell 中版本已更新，后台仍可能运行旧版本。

## 5. 前台 Service 首次启动

建议先禁用自动启动 Agent：

```bash
pairroom service \
  --data-root /absolute/path/to/pairroom-data \
  --auto-start=false \
  --no-browser
```

检查启动输出后手动打开 Management URL。验证：

- Project/Room 数量与 archived/pending 状态；
- Binding ownership 无冲突；
- Runtime policy 与预期一致；
- Registry healthy/diagnostic；
- Management Session 刷新/重启/过期语义；
- numeric loopback listener；
- 一个非关键 Room 能激活、打开和安全挂起。

再运行：

```bash
pairroom doctor --repo /absolute/path/to/safe-test-repo --json
```

最后在非关键 Room 做一次真实 Claude/Codex Turn。

## 6. daemon 升级

如果二进制内容或路径变化，停服后完整替换定义：

```bash
pairroom daemon install --force -- \
  --data-root /absolute/path/to/pairroom-data \
  --listen 127.0.0.1:7332 \
  --runtime-limit 4 \
  --idle-timeout 20m \
  --shutdown-timeout 10m \
  --routing mentions
```

随后：

```bash
pairroom daemon open
pairroom daemon status
pairroom daemon logs -n 200
```

不要把 `daemon restart` 当作配置迁移。它只重启已安装定义。

Linux user service 还应确认 linger 策略符合“退出登录后继续运行”的预期。

## 7. Room Store 首次打开检查

对升级后第一个 Room，建议用兼容入口在不自动启动 Agent 的情况下检查：

```bash
pairroom serve \
  --repo /absolute/path/to/repository \
  --data-dir /absolute/path/to/room-data \
  --auto-start=false
```

检查：

- transcript 最新窗口与历史分页；
- Driver/Reviewer role；
- Reviewer workspace kind、source HEAD、dirty、snapshot digest、read-only strength；
- 没有遗留 `working`、`waiting` 或 pending approval；
- 上传与 Agent 生成图片；
- Turn summary 与 message correlation；
- browser session/CSRF；
- wildcard/LAN/hostname/`localhost` listener 被拒绝。

这一步不适合在同一时间让 Service 也打开该 Room；必须保持单写者。

## 8. 从旧单 Room 迁移到 Service

### 8.1 默认 Room 根

Service 首次启动会扫描默认 PairRoom Room 根，从 Event Logs 重建可识别的 Project/Room/Binding 索引；它不移动、复制或重写 Room 数据。

### 8.2 自定义 `--data-dir`

自定义旧目录不会被全盘扫描。在 Management Shell 选择 Legacy Import，输入绝对路径。导入：

- 只建立 Registry/index；
- 不搬迁或重写 Event Log；
- 从 Event Log 读取并登记已有 Session/Thread ID；缺失任一 ID 时保留为 pending；
- 保留原 Room ID 与历史。

### 8.3 Pending Binding

旧 Room 缺少任一 Vendor ID 时标记 pending，无法激活。通过 Project detail 的 Binding completion：

- 选择 existing 并精确验证；或
- 选择 new，作为 deferred Binding，在首个真实输入后 materialize。

已存在 durable Binding 不可替换。不要为了消除 pending 手工修改 Event Log。

### 8.4 Transcript boundary

Existing Binding 只恢复 Vendor 原生 context。绑定前 Vendor Transcript 不会导入 PairRoom 时间线，这是数据边界，不是迁移失败。

## 9. 重要行为变化

从早期单 Room 版本到当前主线，关键变化包括：

- Reviewer 从 live Driver tree 改为包含 HEAD、dirty tracked 和 untracked regular files 的独立 snapshot；
- 用户消息支持 append、next-turn、supersede/cancel，迟到结果不继续唤醒 stale handoff；
- Inspector summary 可跨重启持久化；
- 提供 `verify`、`backup`、`restore`、`diagnostics`；
- 长对话使用最新窗口 + cursor pagination；
- Room query token/Web Storage credential 被移除，改为 fragment bootstrap + HttpOnly session + CSRF；
- Service Management Shell 使用 Service-scoped HttpOnly Session + CSRF，不使用 Room session；API 客户端仍可直接使用 Bearer；
- 所有内置 listener 仅接受 numeric loopback；
- Service 支持多 Project/Room、Binding ownership、Runtime capacity 和 daemon；
- Registry 是可重建 checkpoint，Room Event Log 是事实源。

## 10. 数据目录中的持久与可重建内容

持久：

```text
events.jsonl
metadata.json
attachments/
```

Service 索引：

```text
service-registry.json   # 默认 Room 可从 Event Logs 重建
```

可重建/不作为主要备份：

```text
runtime/
reviewer worktree
browser sessions
temporary uploads
lock/cache/control files
```

## 11. 回滚

不要让旧二进制直接打开已被新版本写入的 data root/Room。

安全回滚：

1. 停止新版本；
2. 保留新版本 data root 的只读副本用于诊断；
3. 把升级前 Room backup 恢复到新的目录；
4. 使用旧二进制指向恢复目录；
5. 运行旧版本可用的完整性检查；
6. 先禁用 auto-start 检查状态；
7. 再做非关键 Vendor smoke。

```bash
pairroom restore \
  --input /safe/location/room-before-upgrade.tar.gz \
  --data-dir /absolute/path/to/rollback-room
```

未来 schema rejection 是有意保护。降低 metadata 中的 schema 数字不会删除新事件，只会制造无效投影。

## 12. Registry 回滚

若问题只在 Registry checkpoint：

- 停止 Service；
- 备份整个 data root；
- 保留 Room Event Logs；
- 移走损坏 checkpoint；
- 用同一版本重建默认 Room 索引；
- 重新导入自定义 Legacy 路径。

不要从旧 checkpoint 覆盖已经提交的新 Room lifecycle/Binding 事件。

## 13. 升级验收

- `version --json` 与预期 commit 一致；
- daemon 实际运行同一 binary path；
- `doctor` 通过；
- 关键 Room upgrade 前后 verify 通过；
- Management Project/Room/Binding 数量正确；
- capacity/queue/suspend 正常；
- Management 与 Room 认证链路分别正确；
- 一个 Mock collaboration/recovery smoke 通过；
- 一个非关键真实 Vendor smoke 通过；
- daemon stop/restart 能优雅排空；
- 回滚归档与完整配置记录仍可访问。
