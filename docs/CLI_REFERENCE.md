# CLI reference

    CLI Reference 只说明命令职责和发现方式，不复制每个子命令的完整 `--help` 输出。精确默认值、可选值和平台差异始终由当前 binary 给出。

    ## 顶层命令

    | 命令 | 职责 |
    |---|---|
    | `pairroom daemon` | 在 OS 服务管理器中安装与管理 pairroom service |
    | `pairroom service` | 启动多 Project / 多 Room Management Shell |
    | `pairroom serve` | 启动单仓库兼容入口（legacy single-Room） |
    | `pairroom doctor` | 校验 Git 与 vendor CLI 安装 |
    | `pairroom providers` | 检视已脱敏的 provider profile 与分配 |
    | `pairroom verify` | 严格校验 room 数据完整性 |
    | `pairroom backup` | 创建经校验的 room-data 备份 |
    | `pairroom restore` | 恢复并校验 room-data 备份 |
    | `pairroom diagnostics` | 生成脱敏诊断包 |
    | `pairroom protocol` | 输出版本化 Agent 协作合同 |
    | `pairroom version` | 输出构建版本 |

    所有命令先使用：

    ```bash
    pairroom --help
    pairroom <command> --help
    ```

    ## 常用入口

    Mock Management Service：

    ```bash
    pairroom service --mock
    ```

    不自动打开浏览器：

    ```bash
    pairroom service --no-browser
    ```

    输出机器可读 Agent 协议：

    ```bash
    pairroom protocol --json
    ```

    查看版本：

    ```bash
    pairroom version
    ```

    Project、Room 的生命周期由 `pairroom service` 的 Management Shell 与 REST API 管理，不在本 CLI；Daemon、Backup、Restore 等命令有子命令或专属 flag。不要根据旧文档猜测参数，直接查看对应层级的 `--help`。

    ## 退出与错误

    - 参数、配置和安全前置条件错误返回非零退出码；
    - CLI 接受请求不代表 native Turn 已成功完成；
    - destructive command 应先显示目标和前置条件，脚本必须检查退出码及输出；
    - `protocol --routing` 只接受 `turns`，旧 routing 值直接失败。

    ## 源码参数清单

    下列名称从 `cmd/pairroom/*.go` 自动提取。它用于发现遗漏，不表示每个参数适用于所有命令。

    <!-- generated:flags -->
    <details>
    <summary>展开当前参数名</summary>

    - `--actor`
- `--auto-start`
- `--claude-command`
- `--claude-model`
- `--claude-permission-mode`
- `--codex-approval-policy`
- `--codex-command`
- `--codex-effort`
- `--codex-model`
- `--codex-sandbox`
- `--config`
- `--daemon-control-file`
- `--data-dir`
- `--data-root`
- `--follow`
- `--force`
- `--idle-timeout`
- `--input`
- `--json`
- `--listen`
- `--log-file`
- `--max-hops`
- `--mock`
- `--n`
- `--name`
- `--no-browser`
- `--output`
- `--recover-stale-lock`
- `--repo`
- `--role`
- `--routing`
- `--runtime-limit`
- `--shutdown-timeout`
- `--stall-warning-seconds`
- `--token`
    </details>
    <!-- /generated:flags -->
