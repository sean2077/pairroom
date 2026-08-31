# PairRoom CLI 参考

> [文档首页](README.md) · [快速上手](GETTING_STARTED.md) · [运维](OPERATIONS.md) · [排障](TROUBLESHOOTING.md)

本页最后按 `main` 提交 `71c4d007892d60f0299735a5647c4ea8afca4974` 的 `cmd/pairroom/main.go` 与 `cmd/pairroom/daemon.go` 核对。命令行为变更时，应在同一 PR 更新本页和 `CHANGELOG.md`。

## 顶层命令

```text
pairroom daemon <command>       安装和管理后台 Service
pairroom service [options]      多 Project / 多 Room Management Shell
pairroom serve [options]        单 Room 兼容入口
pairroom doctor [options]       检查 Git 和官方 Vendor CLI
pairroom verify [options]       严格验证 Room 数据
pairroom backup [options]       创建自校验 Room 备份
pairroom restore [options]      恢复并验证 Room 备份
pairroom diagnostics [options]  创建脱敏诊断包
pairroom protocol [options]     输出版本化 Agent 协作契约
pairroom version [--json]       输出版本/构建信息
```

通用帮助：

```bash
pairroom help
pairroom service --help
pairroom daemon --help
```

未知命令、无效范围和必填参数缺失会返回非零退出状态。`daemon install` 会先把未识别的安装参数转发到 Service 定义；若它不是合法 Service 参数，后台进程会在启动解析时失败，应结合 `daemon status`/`daemon logs` 检查。

## 路径与监听约定

- `--repo` 必须指向现有目录，PairRoom 会转为 absolute cleaned path；
- `service --data-root` 必须是绝对路径；
- `serve --data-dir` 与数据工具的 `--data-dir` 会转为绝对路径；
- `service` 和 `serve` 只接受数字 loopback host：`127.0.0.1:PORT` 或 `[::1]:PORT`；
- `localhost`、主机名、通配地址、LAN/public IP 都会被拒绝；
- 远程访问使用 SSH local forwarding，不改变 PairRoom listener。

## `pairroom service`

推荐入口。启动一个与当前工作目录无关的本地控制面，并按需激活多个 Room Runtime。

```bash
pairroom service [options]
```

### Service 与容量

| 选项 | 默认/约束 | 作用 |
|---|---|---|
| `--config PATH` | 可选 | JSON 配置文件 |
| `--listen ADDRESS` | 配置值；数字 loopback | Management Shell 地址 |
| `--data-root PATH` | OS 用户配置目录下 `pairroom`；必须绝对 | Service Registry 与默认 Room 数据根 |
| `--token TOKEN` | 省略时随机生成 | Management API Bearer/bootstrap Token；浏览器随后交换为 Session Cookie |
| `--runtime-limit N` | `2`；范围 1–128 | 同时占用容量的 Room Runtime 上限 |
| `--idle-timeout DURATION` | `15m`；必须大于 0 | idle Runtime 自动挂起时间 |
| `--shutdown-timeout DURATION` | `10m`；必须大于 0 | Management handler drain 与 Room Runtime drain 各自使用的等待上限 |
| `--recover-stale-lock` | false | 明确恢复已验证为 crash-stale 的 `service.lock` |
| `--mock` | false | 使用确定性 Mock Agent |
| `--no-browser` | false | 不自动打开 Management Shell |

`--daemon-control-file` 是 daemon 内部参数，不应由用户直接设置。

### Room 默认策略

这些选项成为新激活 Room Runtime 的默认值：

| 选项 | 约束 | 作用 |
|---|---|---|
| `--auto-start BOOL` | 配置值 | Runtime 激活时是否启动双方 Agent |
| `--routing MODE` | `manual` / `mentions` / `roundtable` | 自动路由策略 |
| `--max-hops N` | 1–30 | 每个 Room 的自动 Agent 接力上限 |
| `--stall-warning-seconds N` | `-1` 或 30–86400 | 无 Runtime event 提醒；`-1` 关闭 |
| `--claude-command CMD` | 通常 `claude` | Claude Code 可执行文件 |
| `--claude-model MODEL` | 可选 | Claude 模型覆盖 |
| `--claude-permission-mode MODE` | 配置值 | Driver 的 Claude permission mode |
| `--codex-command CMD` | 通常 `codex` | Codex 可执行文件 |
| `--codex-model MODEL` | 可选 | Codex 模型覆盖 |
| `--codex-effort LEVEL` | 配置值 | Codex reasoning effort |
| `--codex-approval-policy POLICY` | 配置值 | Codex approval policy |
| `--codex-sandbox MODE` | 配置值 | Driver 的 Codex sandbox mode |

示例：

```bash
pairroom service \
  --runtime-limit 4 \
  --idle-timeout 20m \
  --routing mentions \
  --stall-warning-seconds 300
```

## `pairroom serve`

单 Room 兼容入口。直接为一个工作区启动 Room Engine、Room View 和两个 Adapter。

```bash
pairroom serve [options]
```

| 选项 | 默认/约束 | 作用 |
|---|---|---|
| `--config PATH` | 可选 | JSON 配置文件 |
| `--repo PATH` | `.` | repository/workspace 目录 |
| `--name NAME` | 配置值 | Room 显示名 |
| `--listen ADDRESS` | 配置值；数字 loopback | Room View 地址 |
| `--data-dir PATH` | 按 repo absolute path 计算 | Room 状态目录 |
| `--token TOKEN` | 配置值；通常空 | 可选 Bearer/session 纵深防护 |
| `--mock` | false | 使用 Mock Agent |
| `--no-browser` | false | 不自动打开 Room View |
| `--auto-start BOOL` | 配置值 | Room 启动时是否启动 Agent |
| `--routing MODE` | 三种合法模式 | 路由策略 |
| `--max-hops N` | 1–30 | 自动接力上限 |
| `--stall-warning-seconds N` | `-1` 或 30–86400 | stall 提醒 |
| `--claude-command CMD` | 通常 `claude` | Claude Code 可执行文件 |
| `--claude-model MODEL` | 可选 | Claude 模型覆盖 |
| `--claude-permission-mode MODE` | 配置值 | Driver 的 Claude permission mode |
| `--codex-command CMD` | 通常 `codex` | Codex 可执行文件 |
| `--codex-model MODEL` | 可选 | Codex 模型覆盖 |
| `--codex-effort LEVEL` | 配置值 | Codex reasoning effort |
| `--codex-approval-policy POLICY` | 配置值 | Codex approval policy |
| `--codex-sandbox MODE` | 配置值 | Driver 的 Codex sandbox mode |

示例：

```bash
pairroom serve \
  --repo /absolute/path/to/project \
  --routing roundtable \
  --max-hops 8
```

## `pairroom daemon`

将 `pairroom service` 投射到操作系统服务管理器。

### 子命令

| 子命令 | 参数 | 作用 |
|---|---|---|
| `install` | daemon 选项 + Service 选项 | 安装并立即启动 |
| `uninstall` | 无 | 停止并删除服务定义 |
| `start` | 可选 `--recover-stale-lock` | 启动已安装定义 |
| `stop` | 无 | 优雅排空并停止 |
| `restart` | 可选 `--recover-stale-lock` | 重启已安装定义 |
| `status` | 无 | 显示安装、运行、PID、日志与轮转信息 |
| `logs` | `-n`、`-f`/`--follow`、`--log-file` | 查看或跟随日志 |
| `open` | 无 | 从受保护日志验证当前 Management URL 并打开默认浏览器 |

支持平台：Linux systemd、macOS launchd、Windows 当前用户 Task Scheduler。

### `daemon install`

```bash
pairroom daemon install [daemon-options] [service-options]
pairroom daemon install [daemon-options] -- [service-options]
```

| Daemon 选项 | 默认/说明 |
|---|---|
| `--binary PATH` | 当前执行的 PairRoom 二进制 |
| `--work-dir DIR` | 当前目录；安装时转为稳定绝对路径 |
| `--log-file PATH` | 平台默认日志路径 |
| `--log-max-size SIZE` | `10 MiB`（CLI help 显示 `10MB`）；接受 B/K/KB/M/MB/G/GB/T/TB 后缀 |
| `--log-max-backups N` | `3`；范围 1–1000 |
| `--force` | 替换已安装的服务定义 |
| `--` | 其后全部作为 `pairroom service` 选项 |

未识别的 install 选项会被转发给 `pairroom service`。daemon 自动加入 `--no-browser` 和内部 graceful-shutdown control file；`--config`、`--data-root` 等路径会相对 `--work-dir` 转为绝对路径。

示例：

```bash
pairroom daemon install \
  --log-file /absolute/path/to/pairroom-service.log \
  --log-max-size 20M \
  --log-max-backups 5 \
  -- \
  --config /absolute/path/pairroom.json \
  --runtime-limit 4
```

`--force` 重装时必须重新提供完整 Service 与 daemon 配置。`daemon restart` 不接受新配置。

### `daemon open`

```bash
pairroom daemon open
```

要求已安装且正在运行的 daemon。命令从当前和轮转日志中读取最近的 `management:` 地址，只接受带 fragment bootstrap token 的 HTTP 数字 loopback 地址，并用 Bearer Token 请求 `/api/v1/service` 验证它属于当前运行的 Service，验证成功后才打开默认浏览器。它不会把 Token 写入 daemon metadata；找不到可验证地址时返回非零。

### `daemon logs`

```bash
pairroom daemon logs -n 200
pairroom daemon logs -f
pairroom daemon logs --follow
pairroom daemon logs --log-file /path/to/service.log
```

`-f` 是 `--follow` 的短写；`-n` 默认 `100`，范围为 1–1,000,000。

## `pairroom doctor`

```bash
pairroom doctor [options]
```

| 选项 | 作用 |
|---|---|
| `--config PATH` | 读取命令和 Runtime policy |
| `--repo PATH` | 目标 repository/workspace，默认 `.` |
| `--claude-command CMD` | 覆盖 Claude 可执行文件 |
| `--codex-command CMD` | 覆盖 Codex 可执行文件 |
| `--json` | 输出机器可读报告 |

`doctor` 检查 Git 和两个 Vendor Runtime 的路径、版本、协议与能力。每个 Runtime probe 有 15 秒上下文；Git version probe 有 6 秒上下文。检查失败返回非零，但 Mock 模式仍可使用。

它不创建真实模型 Turn，也不证明账号、网络、MCP 或供应商服务在所有仓库中都可用。Probe 会以 `--repo` 指定目录启动 Vendor CLI，可能加载用户/项目配置或 wrapper。

## `pairroom providers`

```bash
pairroom providers [--config PATH] [--json]
```

加载并校验 PairRoom provider profiles 与可选 cc-connect provider 引用，然后输出 Claude/Codex 的独立 provider 和 model 分配。文本与 JSON 都不会启动 Vendor Runtime；JSON 仅返回 provider 摘要、经过凭据清理的 Base URL、agent 分配和参数数量，不回显 API key、provider header 值、环境变量值或原始自定义参数。

| 选项 | 作用 |
|---|---|
| `--config PATH` | PairRoom JSON 配置；相对 cc-connect 路径以该文件所在目录解析 |
| `--json` | 输出机器可读的脱敏报告 |

完整配置格式、`env:NAME` 引用与 cc-connect 导入边界见 [Flexible workflows and provider profiles](FLEXIBLE_WORKFLOWS_AND_PROVIDERS.md)。

## Room 数据命令

以下命令操作 **一个 Room 数据目录**。提供 `--data-dir` 时它优先；否则根据 `--repo` 解析单 Room 默认数据目录。

### `pairroom verify`

```bash
pairroom verify [--repo PATH] [--data-dir PATH] [--json]
```

验证 Event sequence、metadata/schema、Room identity、附件 manifest、大小和 SHA-256。失败返回非零。

### `pairroom backup`

```bash
pairroom backup \
  [--repo PATH] [--data-dir PATH] \
  --output room-backup.tar.gz \
  [--json]
```

`--output` 必填。归档写入 manifest，并在完成前验证可恢复性。运行时缓存、Reviewer 临时工作区、浏览器会话和临时文件不进入备份。

### `pairroom restore`

```bash
pairroom restore \
  --input room-backup.tar.gz \
  [--repo PATH] [--data-dir PATH] \
  [--force] \
  [--json]
```

`--input` 必填。非空目标需要 `--force`；归档在替换目标前完整验证。路径穿越、link、重复/未声明文件、超限和 hash 不匹配会被拒绝。

### `pairroom diagnostics`

```bash
pairroom diagnostics \
  [--repo PATH] [--data-dir PATH] \
  --output diagnostics.tar.gz
```

`--output` 必填。诊断包包含结构、计数、Event header、构建/平台和完整性结果；省略消息正文、附件 bytes 与完整 Runtime payload。分享前仍应人工检查路径和环境元数据。

## `pairroom protocol`

输出 PairRoom 的版本化 Agent 协作契约。该命令不打开 Room、仓库或 Service 状态；它把固定规则作为确定性的 CLI 数据提供给 Agent、文档和诊断工具，避免把完整规则重复拼接到每个原生 Turn。当前契约版本为 `pairroom-protocol/v3`。

```bash
pairroom protocol
pairroom protocol --actor codex --role reviewer --routing roundtable
pairroom protocol --actor claude --json
```

| 选项 | 约束 | 作用 |
|---|---|---|
| `--actor ACTOR` | `claude` / `codex` | 只输出目标 Agent 相关的 mention 规则 |
| `--role ROLE` | `driver` / `reviewer` / `peer` | 只输出当前角色规则 |
| `--routing MODE` | `manual` / `mentions` / `roundtable` | 只输出当前路由规则 |
| `--json` | false | 输出稳定的 `version`、筛选条件和规则数组 |

不提供筛选条件时会输出完整契约。筛选只缩小角色和路由分支；Human authority、原生 Harness authority、输入/输出、媒体、可观察性和收敛规则始终保留。Room Engine 与 Adapter 继续机械执行路由、角色沙箱、投递和生命周期；Agent prompt 只保留需要模型判断的紧凑 bootstrap，并通过版本号引用本命令。

## `pairroom version`

```bash
pairroom version
pairroom version --json
```

文本输出 `pairroom <版本>`；构建管线注入 git 元数据时追加构建提交与距最近 tag 的提交数，例如 `pairroom 1.1.0 (commit 44b6a7a12345, 8 commits since v1.1.0)`。JSON 输出 `version`、`commit`、`build_date`、`last_tag`、`commits_since_tag` 与 `store_schema`。当前源码常量为版本 `1.1.0`、Store schema `8`。

git 元数据由 `make build`、`make install`、`make release` 与 CI/release 工作流通过 `-ldflags` 注入：`commit` 为完整 SHA，`last_tag` 为构建提交可达的最近 tag，`commits_since_tag` 为二者之间的提交数。未经注入的本地 `go build` 保留开发默认值（`dev`/`unknown`），文本输出退化为仅版本号。

## 配置文件与优先级

配置示例位于 [`../examples/pairroom.example.json`](../examples/pairroom.example.json)。基本结构：

```json
{
  "listen": "127.0.0.1:7332",
  "room_name": "Claude × Codex",
  "routing_mode": "mentions",
  "max_agent_hops": 6,
  "stall_warning_seconds": 300,
  "auto_start": true,
  "claude": {
    "command": "claude",
    "model": "",
    "permission_mode": "auto"
  },
  "codex": {
    "command": "codex",
    "model": "",
    "effort": "high",
    "approval_policy": "untrusted",
    "sandbox": "workspaceWrite"
  }
}
```

优先级为：

```text
显式 CLI 参数 > 配置文件 > 程序默认值
```

PairRoom 接受旧配置中的 `unlessTrusted`，并在调用当前 Codex App Server 前迁移为 `untrusted`。不要把 Token 或含认证信息的代理 URL 提交到仓库。
