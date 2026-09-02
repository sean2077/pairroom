# Configuration

    PairRoom 的配置目标是描述本机监听、Runtime policy、两个 native Agent 以及可选 Provider。完整可运行样例见 [`examples/pairroom.example.json`](../examples/pairroom.example.json)。命令行参数的最终解释以 `pairroom <command> --help` 为准。

    ## 加载与覆盖

    启动时先建立内置默认值，再读取 JSON 配置，最后应用当前命令的显式 CLI 参数。Room 内可修改的设置只作用于该 Room，不应被当成全局配置文件回写。

    JSON decoder 拒绝未知字段。这样拼写错误不会被静默忽略，但跨 breaking release 升级前必须阅读 [Upgrading](UPGRADING.md)。

    ## Collaboration policy

    `routing_mode` 只接受：

    ```json
    {"routing_mode": "turns"}
    ```

    `manual`、`mentions`、`roundtable` 已删除且不会迁移。`max_agent_hops` 限制一次自动接力链的 Agent Turn 数；`stall_warning_seconds` 只控制“长时间无 Runtime event”的提醒，不会仅因静默就判定 Turn 已终止。

    ## Native Agent

    Claude 与 Codex 分别配置 executable、模型、effort / thinking、permission / approval policy、sandbox 和 Provider。PairRoom 将这些设置映射到官方 CLI，但供应商支持范围仍以本机安装版本为准。

    建议：

    - 凭据放在供应商 CLI、环境变量或受控 Provider 配置中；
    - 不把 API key 写进命令参数、日志、Room message 或仓库；
    - Reviewer 使用只读 / plan 边界，Driver 才使用写权限；
    - 更新 executable 或 Provider 后先运行 Mock，再做一个真实只读 Turn。

    ## Provider 与 cc-connect

    Provider profile 可以描述 endpoint、model alias、环境变量映射和 Codex wire API。`cc_connect` 只引用现有 provider source，不应复制长期凭据到 Room Event Log。导入冲突需要显式 prefix 或重命名，不能依赖后加载覆盖。

    ## Service runtime policy

    Service 级字段控制同时活跃 Room 数、idle 回收、reconcile、关闭超时、监听地址和 token。它们影响进程生命周期，不改变 Event Log 中已经提交的事实。`--runtime-limit` 默认 8，合法范围 1–128；Management Settings 可以在运行中调整该上限（提高立即启动排队项，降低不打断正在跑的 Turn）。Idle timeout 仍由启动参数决定。

    ## 源码字段清单

    下列 JSON 名称从 `internal/config/` 的 struct tag 自动提取；它是查漏清单，不代替字段语义和样例。

    <!-- generated:config-fields -->
    <details>
    <summary>展开当前 JSON 字段</summary>

    - `agent_model_lists`
- `agent_models`
- `agent_types`
- `alias`
- `api_key`
- `approval_policy`
- `args`
- `auto_start`
- `base_url`
- `cc_connect`
- `claude`
- `codex`
- `command`
- `effort`
- `endpoints`
- `env`
- `env_key`
- `http_headers`
- `imported_from`
- `listen`
- `max_agent_hops`
- `model`
- `models`
- `name`
- `path`
- `permission_mode`
- `prefix`
- `provider`
- `providers`
- `room_name`
- `routing_mode`
- `sandbox`
- `stall_warning_seconds`
- `thinking`
- `token`
- `wire_api`
    </details>
    <!-- /generated:config-fields -->

    ## 变更检查

    配置字段变化时同时更新：

    1. `examples/pairroom.example.json`；
    2. 本文的字段语义；
    3. `docs/UPGRADING.md`（若为 breaking change）；
    4. 配置解析测试。
