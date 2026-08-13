# PairRoom 运行时兼容策略

基准日期：2026-08-13

PairRoom 不实现 Claude Code 或 Codex 的 Agent loop。它只通过供应商公开的结构化接口驱动用户本机安装的官方 CLI，因此兼容性同时取决于：

1. PairRoom 的 Adapter 实现。
2. 用户安装的 CLI 版本。
3. 供应商协议在该版本中的字段与事件。
4. 用户自身的登录、权限、Skills、MCP、Hooks 和项目配置。

## 1. Claude Code

PairRoom 使用 Claude Code print mode 的双向 stream-json：

```text
claude -p
  --input-format stream-json
  --output-format stream-json
```

### 必需能力

- `--input-format stream-json`
- `--output-format stream-json`
- stdin 接收多条结构化 user message
- stdout 输出 `system`、`assistant`、`user`、`stream_event`、`result` 等 stream-json 事件

Claude Code 官方明确说明 `claude --help` 不是完整 flag 清单。因此 `doctor` 不会仅因帮助输出未列出 `--input-format` / `--output-format` 就误判不兼容；它会确认命令和版本，真正的 Runtime 启动结果才是必需协议的最终判据。PairRoom 不会退化为解析 ANSI 终端文本。

### 可选能力

PairRoom 结合 `claude --help` 与版本信息保守启用以下可选参数。帮助输出缺失只意味着“不主动启用或需要版本门槛”，不等价于官方 CLI 一定不支持；不可用时只降低 Inspector 丰富度或会话体验：

| 能力 | CLI 参数 | 不可用时的行为 |
|---|---|---|
| 流式文本增量 | `--include-partial-messages` | 仍等待最终 `result` |
| 用户消息回放 | `--replay-user-messages` | 不依赖原生回放做房间审计 |
| 子 Agent 文本 | `--forward-subagent-text` | 仍显示 Agent tool 生命周期，不展开子 Agent 文本 |
| Hook 生命周期 | `--include-hook-events` | Hooks 仍由 Claude Code 执行，但 PairRoom Inspector 不一定看到事件 |
| 追加协作规则 | `--append-system-prompt-file` / `--append-system-prompt` | 首条普通用户输入前置协议文本 |
| 原生会话恢复 | `--resume` | 新建 Claude session，并更新持久化 ID |

Claude Code 官方文档注明：`--forward-subagent-text` 至少需要 v2.1.211；嵌套子 Agent 文本转发至少需要 v2.1.219。PairRoom 会按版本关闭不兼容参数，不把它们当成启动硬要求。

### 当前限制

- PairRoom 的 Claude Adapter 不是交互式 TUI，因此不复制终端界面快捷键。
- Claude 的交互式权限请求尚未转换为 PairRoom Approval；实际权限由 Claude Code permission mode、规则与 Hooks 管理。
- 工作期间的新输入按 Claude stream-json 原生顺序排队，不宣称具备与 Codex `turn/steer` 完全相同的同 Turn 注入语义。

## 2. Codex

PairRoom 使用官方 Codex App Server 的默认 stdio 传输：

```text
codex app-server
```

该传输为逐行 JSON-RPC/JSONL。PairRoom 使用：

```text
initialize → initialized
thread/start | thread/resume
turn/start
turn/steer
turn/interrupt
item/*
turn/completed
```

### 必需能力

- `codex app-server` 可启动并完成 initialize 握手。
- `thread/start` 或 `thread/resume`。
- `turn/start` 与 Turn 生命周期事件。

### 使用但可降级的能力

| 能力 | 协议面 | 不可用或拒绝时的行为 |
|---|---|---|
| 活跃 Turn 介入 | `turn/steer` + `expectedTurnId` | 输入进入 PairRoom 本地队列，等待安全边界重新 `turn/start` |
| 精确输入关联 | `clientUserMessageId` / `userMessage.clientId` | `startingInput` 与 Turn ID 回退关联仍保留基础顺序 |
| 原生恢复 | `thread/resume` | 创建新 thread，重新注入房间协作规则 |
| 审批 | command/file/permissions server request | 已实现类型进入 UI；未知 request fail closed |
| Inspector | item、plan、diff、usage notifications | 未识别字段保留在原始 event data，不参与控制决策 |

同一 Codex Turn 可以接受多次 `turn/steer`。PairRoom 会保存该 Turn 接收的所有输入，并在 `turn/completed` 后分别结算其 ProcessingState；公共最终回复关联最近一次有效介入。

## 3. `pairroom doctor`

```bash
pairroom doctor --repo /path/to/repo
pairroom doctor --repo /path/to/repo --json
```

检查内容：

- Git 路径与版本。
- Claude/Codex 可执行文件实际路径。
- CLI 版本字符串。
- Claude 公开 stream-json 协议的命令/版本基础条件，以及可从帮助或版本安全识别的可选参数。
- Codex `app-server` 入口。
- 推断出的协议、能力和兼容警告。

Doctor 是非破坏性检查：不会创建供应商会话、不会触发模型调用，也不会读取仓库文件内容。它不能替代真实登录后的 Turn 验证。

## 4. 支持等级

| 等级 | 含义 |
|---|---|
| Available | 可执行文件与必需协议面通过探测，Runtime init 也成功 |
| Degraded | 必需协议可用，但一个或多个可选能力被关闭；UI 显示 warning |
| Unsupported | 命令不存在、版本探测失败、Runtime 实际启动不接受 Claude stream-json，或 Codex 缺少 app-server |
| Unverified | 能编译且协议单元测试通过，但尚未在该具体 CLI 版本与账号环境完成真实 Turn |

v0.2.0 仍处于真实环境兼容矩阵建立阶段。推荐使用供应商当前稳定版，先运行 `doctor`，再在非关键仓库验证：

1. 新建会话与恢复。
2. Claude 排队输入。
3. Codex active-turn steering。
4. Interrupt/Stop/Restart。
5. Codex command/file/permission approvals。
6. Skills、MCP 与项目说明文件是否按用户预期加载。
7. 长 Turn、压缩和子 Agent 事件。

## 5. 协议变化原则

当供应商协议变化时，PairRoom遵循：

1. 不解析 ANSI/TUI 文本作为控制事实。
2. 不为未知字段猜测高权限语义。
3. 未知 server request 默认拒绝。
4. 已持久化消息不会因 Adapter 失败而消失。
5. Transport 已接受与 Runtime 已完成分开记录。
6. 可选能力优先降级；必需能力缺失则明确失败。
7. 新版本事件先保留原始 payload，再逐步加入 canonical projection。

## 6. 官方参考

- Claude Code CLI reference: https://code.claude.com/docs/en/cli-reference
- Claude Code hooks: https://code.claude.com/docs/en/hooks
- Codex App Server: https://developers.openai.com/codex/app-server
- Codex App Server source README: https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md
