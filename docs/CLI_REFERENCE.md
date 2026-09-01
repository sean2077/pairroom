# PairRoom CLI 参考

本页是命令职责和参数入口。每个命令的实际 `--help` 是精确语法事实源；配置字段见 [Configuration](CONFIGURATION.md)。

## 顶层命令

| 命令 | 职责 |
|---|---|
| `pairroom service` | 多 Project / 多 Room 控制面 |
| `pairroom daemon` | 安装和管理后台 Service |
| `pairroom serve` | 单仓库、单 Room 快捷入口 |
| `pairroom doctor` | 检查 Git 与 Vendor CLI 必需协议面 |
| `pairroom providers` | 查看脱敏 Provider 解析结果 |
| `pairroom verify` | 严格校验一个 Room 数据目录 |
| `pairroom backup` | 创建并验证 Room 备份 |
| `pairroom restore` | 恢复并验证备份 |
| `pairroom diagnostics` | 生成脱敏 Room diagnostics bundle |
| `pairroom protocol` | 输出版本化 Agent 协作合同 |
| `pairroom version` | 输出构建版本和来源信息 |

Project / Room lifecycle 通过 Management Shell/API 管理；没有 `pairroom project` 或 `pairroom room` 顶层命令。

## 通用约定

- `--config` 读取严格 JSON，CLI flag 覆盖配置；
- `--listen` 只接受数字 loopback；
- Service `--data-root` 要求绝对路径；Room 数据命令的 `--data-dir` 会解析并规范化为绝对路径，Management legacy import 的 path 则必须显式为绝对路径；
- `--routing` 只接受 `turns`；
- 参数、配置、校验和安全前置条件错误返回非零退出码；
- HTTP/CLI 接受输入不代表 native Turn 已完成。

## `pairroom service`

```bash
pairroom service [options]
```

主要参数：

| 参数 | 作用 |
|---|---|
| `--config FILE` | JSON 配置 |
| `--data-root DIR` | Service 数据根 |
| `--listen ADDR` / `--token TOKEN` | Management listener 与 bootstrap token |
| `--runtime-limit N` | 同时保留的 Room Runtime 上限 |
| `--idle-timeout DURATION` | idle Runtime 回收阈值 |
| `--shutdown-timeout DURATION` | Service 排空超时 |
| `--daemon-control-file FILE` | daemon 写入/读取的控制信息文件 |
| `--recover-stale-lock` | 仅在人工确认旧进程消失后恢复 crash-stale lock |
| `--routing turns` / `--max-hops N` | Turn 接力策略与 hop 上限 |
| `--stall-warning-seconds N` | working 无 Runtime event 提醒；`-1` 关闭 |
| `--mock` / `--no-browser` | Mock Runtime / 不自动打开页面 |
| `--auto-start[=BOOL]` | Runtime 激活时是否启动 Adapter；关闭时使用 `--auto-start=false` |

Agent override：

```text
--claude-command --claude-model --claude-permission-mode
--codex-command --codex-model --codex-effort
--codex-approval-policy --codex-sandbox
```

## `pairroom daemon`

```text
install | uninstall | start | stop | restart | status | logs | open
```

常用：

```bash
pairroom daemon install --runtime-limit 4 --idle-timeout 20m
pairroom daemon install --force -- --data-root /absolute/path --runtime-limit 4
pairroom daemon status
pairroom daemon logs -n 200
pairroom daemon logs -f
pairroom daemon open
pairroom daemon restart
```

`install -- ...` 之后的参数原样传给 Service；未被 daemon 自身识别的 install 参数也会转发。安装时可用 `--log-file` 指定日志文件；`logs --follow`（或 `-f`）持续跟随。`start` / `restart` 仅额外接受 `--recover-stale-lock`，不会替换已安装的 Service 参数。`open` 会验证当前 numeric-loopback Management URL 后打开。

## `pairroom serve`

```bash
pairroom serve --repo /absolute/path/to/project
pairroom serve --repo /absolute/path/to/project --mock --no-browser
```

主要参数：`--repo`、`--data-dir`、`--name`、`--listen`、`--token`、Agent override、`--auto-start`、`--routing turns`、`--max-hops`、`--stall-warning-seconds`。

它跳过上层 Service Registry / Runtime Manager，适合单仓库临时使用；长期多 Room 使用 `service`。

## `pairroom doctor`

```bash
pairroom doctor --repo /absolute/path/to/repo
pairroom doctor --repo /absolute/path/to/repo --json
```

可覆盖 `--config`、`--claude-command`、`--codex-command`。Doctor 检查 executable 和必需结构化协议，不登录账号、不创建模型 Turn，也不证明供应商当前可达。

## `pairroom providers`

```bash
pairroom providers --config pairroom.json
pairroom providers --config pairroom.json --json
```

输出脱敏 Provider、endpoint、model 与 Agent 分配；用于在启动前确认显式 profile、cc-connect import 和优先级。

## Room 数据命令

```bash
pairroom verify --data-dir /path/to/room --json
pairroom backup --data-dir /path/to/room --output room.tar.gz
pairroom restore --input room.tar.gz --data-dir /path/to/restored
pairroom diagnostics --data-dir /path/to/room --output diagnostics.tar.gz
```

省略 `--data-dir` 时，部分命令可用 `--repo` 解析单 Room 默认目录。`restore --force` 只在完整 archive validation 通过后替换非空目标。备份/恢复细节见 [Storage](STORAGE.md) 与 [Operations](OPERATIONS.md)。

## `pairroom protocol`

```bash
pairroom protocol
pairroom protocol --actor claude --role reviewer --routing turns
pairroom protocol --json
```

`--actor` 只接受 `claude|codex`，`--role` 只接受 `driver|reviewer|peer`，`--routing` 只接受 `turns`。输出是模型需要理解的合同；Engine 仍负责机械校验。

## `pairroom version`

```bash
pairroom version
pairroom version --json
```

输出版本、commit、build date、最近 tag 与距 tag 的提交数。README 不固定当前版本，发布身份以 `VERSION`、tag、构建元数据和 Changelog 一致性为准。
