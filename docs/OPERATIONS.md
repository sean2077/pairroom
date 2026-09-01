# PairRoom 运维手册

本页面向部署、后台运行、容量、备份和事故处置。日常页面操作见 [User Guide](USER_GUIDE.md)，数据原理见 [Storage](STORAGE.md)。

## 选择运行方式

| 方式 | 适用 |
|---|---|
| `pairroom service` | 推荐；多 Project / 多 Room，前台运行 |
| `pairroom daemon` | 长期后台运行同一个 Service |
| `pairroom serve` | 单仓库临时使用或兼容路径 |
| `--mock` | 安装验收、回归和故障隔离 |

## 上线前检查

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/project --json
pairroom providers --config /absolute/path/to/pairroom.json --json
```

确认：

- data root 是本机可靠文件系统上的绝对路径；
- listener 是数字 loopback；
- Token 未进入 shell history、进程参数审计或仓库；
- Claude/Codex 可独立运行并已登录；
- 备份目录不位于待恢复目标内；
- 有足够磁盘容纳 Event Log、附件、Reviewer snapshot 和备份。

## 前台 Service

```bash
pairroom service \
  --config /absolute/path/to/pairroom.json \
  --data-root /absolute/path/to/data \
  --runtime-limit 4 \
  --idle-timeout 20m
```

停止时使用 Ctrl-C / SIGTERM。Service 进入 drain：停止接受新激活，等待 handler / safe lifecycle 边界，并在 `--shutdown-timeout` 内关闭 Runtime。不要直接删除 `service.lock` 代替正常停止。

## 后台 daemon

```bash
pairroom daemon install --runtime-limit 4 --idle-timeout 20m
pairroom daemon status
pairroom daemon open
pairroom daemon logs -f
```

平台投影：Linux systemd user service、macOS launchd user agent、Windows Task Scheduler。`install` 固化 binary、workdir、log 与 Service 参数；修改安装参数后使用 `install --force`，普通 `restart` 只重启现有定义。

```bash
pairroom daemon stop
pairroom daemon restart
pairroom daemon uninstall
```

## Crash-stale lock

若 Service 崩溃，`service.lock` 可能残留。先用系统进程/服务工具确认旧 PairRoom 已不存在，再显式：

```bash
pairroom daemon start --recover-stale-lock
# 或 daemon restart / service 对应 flag
```

不要在旧进程仍存活时恢复锁，否则可能出现两个 Registry/Event writer。

## 浏览器与远程访问

Management/Room 只允许 loopback。远程访问使用：

```bash
ssh -L 7332:127.0.0.1:7332 host
```

然后在本机浏览器打开转发端口。不要把 PairRoom 直接绑定到 `0.0.0.0`、LAN IP、hostname 或公网，也不要用会重写 Origin/Host/Auth 的反向代理冒充支持拓扑。

## Runtime capacity

`--runtime-limit` 限制同时保留的 Room Runtime。状态可能为 inactive、queued、starting、active、suspending、failed/cleanup-retained。

- active busy Turn 不因容量回收被 interrupt；
- idle Runtime 达到 `--idle-timeout` 后可挂起；
- failed 或 cleanup uncertain Runtime 可能继续占容量；
- queued Room 在容量释放后按 Runtime Manager 策略启动；
- Archive 是显式停止当前 Turn 的 destructive lifecycle，不等同于普通 capacity suspend。

容量问题先看 Management Runtimes 和 diagnostics，不要只按页面 phase 猜进程是否存在。

## 一致备份

对关键 Room：

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom backup --data-dir /absolute/path/to/room --output /safe/path/room.tar.gz
```

备份前停止写入。备份不包含 Git worktree、Vendor context、Service Registry 或 daemon 定义；另外保存配置、安装参数和关键 Project path。

## 恢复演练

```bash
pairroom restore --input /safe/path/room.tar.gz --data-dir /new/absolute/path
pairroom verify --data-dir /new/absolute/path --json
```

先恢复到新目录并验证，再通过 Management import 登记。不要直接覆盖当前 live Room。Existing Binding 仍受全局 ownership 约束；旧 Room 未删除/解绑时，恢复副本可能无法同时激活。

## Registry 恢复

`service-registry.json` 丢失或损坏时，先停止 Service并备份整个 data root，再重启让 Registry 从 `rooms/*/events.jsonl` 重建。没有 Room 的显式 Project 注册可能无法恢复，需要重新登记。若 Registry fail closed，保留原目录和 diagnostics，不手工拼 JSON 继续运行。

## Room 永久删除

先 Archive，再检查 Room、备份和外部副作用，最后显式 data-loss acknowledgement。删除后：

- PairRoom Room 数据和 Binding ownership 被移除；
- Git worktree、Vendor Session/Thread 不受影响；
- committed 后的物理清理失败显示 pending cleanup，可重试；
- 不要手工删除 `.deleted-rooms` 中未完成的 intent/marker。

## 诊断顺序

```bash
pairroom version --json
pairroom daemon status
pairroom daemon logs -n 200
pairroom doctor --repo /absolute/path/to/project --json
pairroom verify --data-dir /absolute/path/to/room --json
pairroom diagnostics --data-dir /absolute/path/to/room --output diagnostics.tar.gz
```

再用 `--mock` 复现，区分 PairRoom 控制面与 Vendor Runtime。分享 diagnostics 前人工脱敏。

## 升级与回滚

遵循 [Upgrading](UPGRADING.md)：备份、停止写入、替换 binary、前台启动、验证 Room replay 和 Mock，再恢复 daemon。不要仅因构建/Mock CI 绿色就假设当前官方 CLI 兼容。

## 最低运维清单

- [ ] data root 和备份权限仅当前用户；
- [ ] listener 仍是数字 loopback；
- [ ] 定期 `verify` 关键 Room 并做异地备份；
- [ ] 记录 PairRoom 与 Vendor CLI 版本；
- [ ] 监控磁盘、failed-retained Runtime 与 pending deletion cleanup；
- [ ] 升级前做恢复演练；
- [ ] 只在真实执行过时声称 native E2E。
