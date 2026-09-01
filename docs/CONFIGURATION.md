# PairRoom 配置参考

PairRoom 使用严格 JSON 配置。未知字段会被拒绝；CLI 参数覆盖配置值。配置只描述 PairRoom 启动和 Vendor CLI 投影，不接管官方 CLI 自身的账号、Skills、MCP、Hooks 或全局设置。

## 最小配置

```json
{
  "listen": "127.0.0.1:7332",
  "routing_mode": "turns",
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

省略配置文件时使用这些默认值。`room_name` 主要用于 `serve` 的单 Room 默认名称。

## 顶层字段

| 字段 | 含义 |
|---|---|
| `listen` | 数字 loopback 地址；默认 `127.0.0.1:7332` |
| `room_name` | 单 Room 入口默认名称 |
| `routing_mode` | 只接受 `turns`；旧 `manual` / `mentions` / `roundtable` 被拒绝 |
| `max_agent_hops` | 自动接力链最大 Turn 数，范围 1–30 |
| `stall_warning_seconds` | working Agent 无 Runtime event 的提醒阈值；`-1` 关闭，其他值 30–86400 |
| `auto_start` | Runtime 激活时是否启动两个 Adapter |
| `token` | 显式 Management/Room bootstrap token；不要提交到仓库 |
| `providers` | PairRoom Provider profile 列表 |
| `cc_connect` | 对 cc-connect 配置的只读引用导入 |
| `claude` / `codex` | 两个 Agent 的启动与 Provider 选择 |

Service-only 的 `data_root`、`runtime_limit`、`idle_timeout` 和 `shutdown_timeout` 是 CLI / daemon 参数，不属于本 JSON struct。

## Agent 字段

| 字段 | Claude | Codex | 说明 |
|---|---:|---:|---|
| `command` | ✓ | ✓ | executable；不能为空 |
| `args` | ✓ | ✓ | 追加的原生 CLI 参数；它不是 secret channel，可能出现在进程检查或 Vendor 日志中 |
| `model` | ✓ | ✓ | 模型覆盖；空值保留 Vendor 默认或 Provider 值 |
| `provider` | ✓ | ✓ | 引用 `providers[].name`，大小写不敏感 |
| `permission_mode` | ✓ | — | Claude Code permission mode |
| `effort` | — | ✓ | Codex effort |
| `approval_policy` | — | ✓ | Codex approval policy |
| `sandbox` | — | ✓ | Codex sandbox；默认 `workspaceWrite` |

Reviewer 角色会在这些基本设置上叠加 PairRoom 的 native 只读 policy。配置不能把 Reviewer 变成容器级安全边界。

## Provider profile

```json
{
  "providers": [
    {
      "name": "company-gateway",
      "api_key": "env:PAIRROOM_GATEWAY_KEY",
      "base_url": "https://gateway.example.invalid",
      "model": "default-model",
      "agent_types": ["claudecode", "codex"],
      "agent_models": {
        "claudecode": "claude-model",
        "codex": "codex-model"
      },
      "env": {
        "EXAMPLE_FLAG": "1"
      },
      "codex": {
        "wire_api": "responses",
        "env_key": "PAIRROOM_GATEWAY_KEY",
        "http_headers": {
          "X-Tenant": "tenant-a"
        }
      }
    }
  ],
  "claude": {
    "command": "claude",
    "model": "",
    "permission_mode": "auto",
    "provider": "company-gateway"
  },
  "codex": {
    "command": "codex",
    "model": "",
    "effort": "high",
    "approval_policy": "untrusted",
    "sandbox": "workspaceWrite",
    "provider": "company-gateway"
  }
}
```

### Provider 字段

| 字段 | 作用 |
|---|---|
| `name` | 必填且在配置中大小写不重复 |
| `api_key` | 直接值、`env:NAME` 或 `${NAME}`；推荐环境变量引用 |
| `base_url` | 通用 endpoint |
| `endpoints` | 按 `claudecode` / `codex` 覆盖 endpoint |
| `model` | 通用 model fallback |
| `models` | 来自 cc-connect 的可选模型目录；每项含 `model` 与可选 `alias`，PairRoom 不据此自动切模型 |
| `agent_models` | 按 Agent 类型覆盖 model |
| `agent_model_lists` | 按 Agent 类型保留模型目录 metadata |
| `thinking` | Provider metadata；不会替代 Agent 明确设置 |
| `env` | 注入该 Agent 进程的环境变量 |
| `agent_types` | 限制 Provider 可用于 Claude Code / Codex；空列表表示两者均可 |
| `codex.wire_api` | Codex model provider wire API，默认 `responses` |
| `codex.env_key` | Codex provider 读取 secret 的环境变量名 |
| `codex.http_headers` | 通过临时环境变量投影的 HTTP header |

显式 PairRoom Agent 设置优先于 Provider fallback。Provider secret 只保留在内存并通过环境传递，不放入命令行。

## cc-connect 引用导入

```json
{
  "cc_connect": {
    "path": "~/.cc-connect/config.toml",
    "providers": ["provider-a", "provider-b"],
    "prefix": "cc-"
  }
}
```

- `path` 为空时默认 `~/.cc-connect/config.toml`；相对路径相对于 PairRoom 配置文件；
- `providers` 为空时导入全部 `[[providers]]`；
- `prefix` 在导入名称前添加前缀；
- 同名显式 PairRoom profile 获胜，导入项被跳过；
- 只解析受支持的 provider table，不复制或改写源配置；
- 未被选中的 cc-connect 项目/平台设置会被忽略。

## 检查解析结果

```bash
pairroom providers --config /absolute/path/to/pairroom.json
pairroom providers --config /absolute/path/to/pairroom.json --json
```

输出会脱敏 credential、query 和 secret。不要通过 `ps` 可见的命令行直接传 API key；优先使用环境变量引用。

## CLI 覆盖与 daemon

`service` / `serve` 的 Agent、routing、max-hops、stall、listen、token 等 flags 覆盖配置。`daemon install` 会固化当次 Service 参数；修改 JSON 后需重启 daemon，修改安装参数则重新 `install --force`。

完整参数见 [CLI Reference](CLI_REFERENCE.md)，敏感数据边界见 [Privacy](PRIVACY.md) 与 [Security](../SECURITY.md)。
