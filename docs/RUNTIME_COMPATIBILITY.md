# PairRoom 运行时跟随策略

基准日期：2026-08-13

PairRoom 不维护 Claude Code/Codex 的历史版本兼容矩阵。项目面向两套官方 Coding Agent 的**当前稳定版/最新公开协议面**开发：供应商升级后及时跟随，旧版本只在仍然自然可用时继续工作，不承诺长期回溯适配。

这不是“忽略兼容性”，而是把精力放在真正有价值的部分：结构化协议、失败可诊断、消息不丢失、权限不静默放宽，以及升级后能快速验证。

## 1. 总原则

1. 只连接官方结构化接口，不解析终端 ANSI/TUI 文本作为控制事实。
2. 当前公开协议是实现基线；不为历史版本维护分支矩阵。
3. 可选事件缺失时降级展示；必需握手失败时明确停止。
4. 未知高权限请求默认拒绝，不猜测供应商意图。
5. Delivery 与 Processing 分离，Adapter 失败不会抹掉已持久化消息。
6. `doctor` 用于环境诊断，不用于证明真实账号下的完整 Turn 一定成功。
7. 每次升级 Claude Code、Codex 或 PairRoom 后，在非关键仓库做一次 smoke test。

## 2. Claude Code

PairRoom 启动官方 CLI 的 headless 双向 stream-json 模式：

```text
claude -p
  --input-format stream-json
  --output-format stream-json
  --permission-prompt-tool stdio
```

### 必需协议面

- stdin 接收多条结构化 `user` 消息；
- stdout 输出 `system`、`assistant`、`user`、`stream_event`、`result`；
- `--permission-prompt-tool stdio` 将工具权限请求发送到同一控制通道；
- 启动后能够完成 `control_request/initialize` → `control_response` 握手；
- `can_use_tool` 请求可以由 PairRoom 回写 allow/deny；
- session ID 能被读取；当前版本支持时使用 `--resume`。

PairRoom 会在 initialize 成功前阻止第一条普通用户消息，避免权限、Hook 或 SDK 配置尚未安装完成时提前进入 Turn。

### 可选展示能力

| 能力 | 常见 CLI 参数 | 缺失时 |
|---|---|---|
| 文本增量 | `--include-partial-messages` | 等待最终结果，公共消息不丢失 |
| 用户消息回放 | `--replay-user-messages` | PairRoom 自己的公共日志仍完整 |
| 子 Agent 文本 | `--forward-subagent-text` | 只显示子 Agent/工具生命周期 |
| Hook 生命周期 | `--include-hook-events` | Hook 仍由 Claude 执行，Inspector 可能更简略 |
| 协作规则追加 | `--append-system-prompt-file` / `--append-system-prompt` | 首条输入中附加房间协议 |
| 附件目录 | `--add-dir` | 多模态图片仍以 base64 内容块发送 |

这些能力不会通过维护固定版本表来判断；PairRoom 结合当前 CLI 的帮助输出、已知公开参数和实际启动结果进行协商。

### 图片与审批

- 图片以 Claude 原生 base64 image content block 进入用户消息；
- 图片在 Adapter 边界前重新检查类型、大小和不可变哈希；
- `can_use_tool` 被投影为统一 Approval；
- `AskUserQuestion` 被投影为单选、多选或文本表单；
- Reviewer 使用原生 `plan` permission mode，并移除写工具；控制层对仍到达的写请求再次拒绝。

## 3. Codex

PairRoom 启动：

```text
codex app-server
```

连接使用逐行 JSON-RPC/JSONL：

```text
initialize → initialized
thread/start | thread/resume
turn/start
turn/steer
turn/interrupt
item/*
turn/completed
```

### 必需协议面

- App Server 能完成 initialize；
- 能创建或恢复 thread；
- 能启动 Turn 并发出 Turn 生命周期事件；
- 最终完成或中断能得到结构化状态。

### 使用能力

| 能力 | 协议面 | 失败时 |
|---|---|---|
| 活跃 Turn 介入 | `turn/steer` + `expectedTurnId` | 进入本地队列，等安全边界重新启动 Turn |
| 图片输入 | `localImage` | 目标处理失败并显示精确诊断，不静默丢图 |
| 输入关联 | `clientUserMessageId` / `userMessage.clientId` | 使用 Turn 与提交顺序作保守关联 |
| 原生恢复 | `thread/resume` | 建立新 thread，并保留 PairRoom 公共历史 |
| 审批 | command/file/permissions request | 支持的请求进入 UI；未知请求 fail closed |
| Reviewer | `sandboxPolicy: {type: "readOnly"}` | 若运行时拒绝该策略，Turn 直接失败而不是退回写权限 |

`thread/start.sandbox` 与 Turn 级 policy 使用不同的枚举格式：前者发送 `read-only`、`workspace-write` 或 `danger-full-access`，后者的 `sandboxPolicy.type` 发送 `readOnly`、`workspaceWrite` 或 `dangerFullAccess`。PairRoom 接受现有配置中的 camelCase、snake_case 与 kebab-case 别名，但始终按目标字段要求序列化。

一个 active Turn 可以接收多次 steer；PairRoom 仍为每条用户输入分别记录 Delivery/Processing。

## 4. `pairroom doctor`

```bash
pairroom doctor --repo /path/to/repo
pairroom doctor --repo /path/to/repo --json
```

检查：

- Git、Claude Code、Codex 的可执行路径和版本字符串；
- Claude stream-json/control 所需入口与可选参数；
- Codex `app-server` 是否存在；
- 推断出的协议、能力、降级项与错误。

Doctor 不登录、不创建模型会话、不读取仓库文件内容。包装脚本本身是可执行代码，只应配置可信命令。

## 5. 升级后的推荐 smoke test

1. 运行 `pairroom doctor --json`。
2. 在 Mock 模式确认网页和本地状态目录可用。
3. 在非关键仓库给 Claude/Codex 分别发送一条文本消息。
4. 粘贴一张图片并同时发送给两者。
5. 对 Codex 正在运行的 Turn 发送补充消息，确认显示 `injected` 或明确降级为 `queued`。
6. 触发一项 Claude/Codex 审批并在 UI 处理。
7. 切换 Reviewer，确认原生只读/plan 策略显示在参与者卡片。
8. Stop/Restart 后恢复原生 session/thread。

## 6. 不兼容时的处理

- **可选事件变化**：保留原始 payload，减少 Inspector 展示。
- **字段新增**：忽略未使用字段，除非它改变安全语义。
- **权限请求新增**：默认拒绝，待明确实现后再开放。
- **必需握手变化**：Runtime 标记为 error，消息保留并允许重试。
- **CLI 删除结构化入口**：不退回终端抓屏或键盘模拟。

## 7. 官方参考

- Claude Code CLI / Agent SDK 文档：<https://code.claude.com/docs/>
- Claude Agent SDK Python 实现：<https://github.com/anthropics/claude-agent-sdk-python>
- Codex App Server：<https://developers.openai.com/codex/app-server>
- Codex App Server source README：<https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md>
