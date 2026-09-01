# PairRoom Runtime 跟随与兼容策略

> [文档首页](README.md) · [CLI 参考](CLI_REFERENCE.md) · [架构](ARCHITECTURE.md) · [排障](TROUBLESHOOTING.md)

文档基线：PairRoom `main` 提交 `a14d6705fdcd057a74fa3275437aca205dab06d9`（2026-08-16）。

PairRoom 不维护 Claude Code/Codex 的历史版本兼容矩阵。项目面向两套官方 Coding Agent 的当前稳定公开协议：供应商升级后及时跟随，旧版本只在自然兼容时继续工作，不承诺长期回溯适配。

这不是忽略兼容性，而是把兼容预算放在结构化协商、精确恢复、失败诊断、消息不丢失和权限不静默放宽上。

## 1. 兼容原则

1. 只连接官方结构化接口，不把 ANSI/TUI 文本当作控制事实；
2. 必需握手/身份恢复失败时明确停止；
3. 可选事件缺失时降级 Inspector 展示，但不丢公共消息；
4. 未知高权限请求默认拒绝；
5. Delivery 与 Processing 分离，Adapter 失败不抹掉已持久化输入；
6. Existing/durable Binding 必须精确 resume，绝不静默换成新身份；
7. `doctor` 是协议预检，不是账号、网络或真实 Turn 的完整证明；
8. Mock 证明 PairRoom 状态机，不证明 Vendor 协议；
9. 每次升级 PairRoom 或任一 Vendor CLI 后，在非关键仓库做真实 smoke；
10. 文档记录当前能力，不把过时版本表固化为长期承诺。

## 2. 版本与能力模型

PairRoom 关注三层兼容：

| 层 | 例子 | 失败策略 |
|---|---|---|
| 启动入口 | `claude -p ...`、`codex app-server` | Runtime unavailable |
| 必需协议 | initialize、Session/Thread、Turn、final/error | fail closed，保留 PairRoom 消息 |
| 可选观测 | partial text、Hook、subagent、usage、细粒度 item | 降级 Inspector，显示 warning |

不能仅靠版本字符串判断所有能力。PairRoom 使用帮助输出、公开参数、协议 probe 和实际启动结果共同判断。

## 3. Claude Code

PairRoom 启动官方 CLI 的 headless 双向 stream-json：

```text
claude -p
  --input-format stream-json
  --output-format stream-json
  --permission-prompt-tool stdio
  ...current optional flags
```

### 3.1 必需协议面

- stdin 能持续接收结构化 `user` 消息；
- stdout 提供 `system`、`assistant`、`user`、`stream_event`、`result` 等结构化消息；
- 能完成 `control_request/initialize` → `control_response`；
- `can_use_tool`/`AskUserQuestion` 可在 control channel 处理；
- Session ID 可读取；
- durable/existing Binding 可按指定 ID 精确 resume；
- interrupt/exit 可被识别并收口 pending waiter/approval/processing。

Initialize 成功前，PairRoom 不发送第一条普通用户消息，避免权限、Hook 或 SDK 配置尚未完成。

### 3.2 可选能力

| 能力 | 常见参数/事件 | 缺失时 |
|---|---|---|
| 文本增量 | partial/stream event | 等待 final，公共结果不丢失 |
| 用户消息回放 | replay user messages | PairRoom Event Log 仍完整 |
| 子 Agent 文本 | forwarded subagent text | 只显示生命周期/工具摘要 |
| Hook 生命周期 | hook events | Hook 仍由 Claude 执行，Inspector 更简略 |
| 协作协议追加 | append system prompt file/text | 首条输入中附加房间协议 |
| 附件目录能力 | add-dir | 图片仍可用原生 base64 content block |

可选能力变化不应让 Adapter 伪造结构化事件。

### 3.3 图片

- PairRoom 在 Adapter 边界前复核附件类型、大小、维度与 SHA-256；
- 图片以 Claude 原生 base64 image content block 发送；
- 附件本机绝对路径不进入公共 Event/API；
- 失败必须落到该目标的 Delivery/Processing，不静默删图。

### 3.4 审批与 Reviewer

- `can_use_tool` 映射到统一 Approval；
- `AskUserQuestion` 映射到单选、多选或文本表单；
- Reviewer 使用原生 plan permission mode；
- 写工具被 disallowed/deny；
- 即使写请求仍到达控制层，也再次 fail closed；
- 未知 control request 返回协议错误。

### 3.5 Session 恢复

对于 durable/existing Claude Binding：

```text
resume exact Session ID
  -> success: continue same native context
  -> failure: Room Runtime error / explicit diagnostic
```

PairRoom 不以“新建 Session”替代失败的精确恢复，因为那会让 Room 记录的 Binding 与实际 Vendor context 分叉。

## 4. Codex

PairRoom 启动：

```text
codex app-server
```

使用逐行 JSON-RPC/JSONL：

```text
initialize → initialized
thread/start | thread/resume
turn/start
turn/steer
turn/interrupt
item/*
turn/completed
```

### 4.1 必需协议面

- App Server 能 initialize；
- 能创建新 Thread；
- 能按指定 ID 精确 resume durable/existing Thread；
- 能启动 Turn 并发出结构化 lifecycle；
- final/completed/interrupted/error 可识别；
- server request 可按 request ID 正确答复；
- interrupt/exit 可收口 waiter、approval 和 processing。

### 4.2 能力与降级

| 能力 | 协议面 | 不可用时 |
|---|---|---|
| 活跃 Turn 介入 | `turn/steer` + expected Turn | 保守 queue 到安全边界，不伪造 injected |
| 图片输入 | `localImage` | 目标明确失败，不静默丢图 |
| 输入关联 | client user message ID / user message client ID | 用 Turn + 提交顺序保守关联并标记限制 |
| 新 Thread | `thread/start` | Room 无法创建/首次执行 |
| durable 恢复 | `thread/resume` | Runtime error；绝不静默换新 Thread |
| 审批 | command/file/additional-permission request | 已知请求进入 UI；未知请求 fail closed |
| Reviewer | read-only sandbox policy | Runtime 拒绝则 Turn 失败，不退回写权限 |

### 4.3 Sandbox wire format

Thread 与 Turn 使用不同 wire enum：

```text
thread/start.sandbox:
  read-only | workspace-write | danger-full-access

turn/start.sandboxPolicy.type:
  readOnly | workspaceWrite | dangerFullAccess
```

PairRoom 可接受现有配置中的 camelCase、snake_case 与 kebab-case 别名，但在每个 wire boundary 按目标 schema 序列化。

### 4.4 多次 steer

一个 active Codex Turn 可以接收多次 steer；PairRoom 仍为每条用户输入分别记录 Delivery/Processing 与 correlation。Vendor 没有确认注入时，不能仅因为请求已发送就标记 `injected`。

### 4.5 Thread 恢复

对于 durable/existing Codex Binding：

```text
thread/resume exact ID
  -> success: continue same native context
  -> failure: explicit Runtime error
```

旧文档中“resume 失败后建立新 Thread”的做法不符合当前 Binding 一致性模型，不应用于受 Service 管理的 durable Room。

> 与之不冲突的一点：`thread/start` 只在 Codex 内存里建 thread，rollout 要等首个被接受的 turn 才落盘。若 app-server 进程在首个 turn 被接受前意外退出，这条**尚未 materialize 的内存 thread ID 没有持久 rollout**，会在 `takeOutstanding` 时被丢弃（pending new binding、`threadEngaged=false`），下次激活重新 `thread/start`。这属于“未持久化的临时身份被丢弃”，不是“durable resume 失败后新建替代身份”。已 materialize 的 existing binding 仍按上述规则精确 resume，resume 失败即显式报错。

## 5. New 与 Existing Binding 的兼容要求

### Existing

Provisioning 阶段就验证精确恢复：

- 任一侧失败，Room 不原子发布；
- identity 已被另一 Room 占用，拒绝；
- 不读取或导入 Binding 前 Vendor Transcript。

### New

New 初始为 deferred：

- 首个输入触发创建原生身份；
- 输入被 Vendor 接受后才 durable materialize；
- Event append/ownership/checkpoint 失败则中断；
- 进程在 materialize 前退出，可再次创建空会话；
- materialize 后的后续激活必须精确 resume。

协议升级不能绕过该状态机。

## 6. `pairroom doctor`

```bash
pairroom doctor --repo /absolute/path/to/repo
pairroom doctor --repo /absolute/path/to/repo --json
```

检查：

- Git/Claude/Codex executable path 与版本；
- Claude stream-json/control 所需入口与可选参数；
- Codex `app-server`；
- 推断出的 protocol、capabilities、warnings 与 errors。

每个 Vendor probe 有 15 秒上下文；Git version probe 有 6 秒上下文。

Doctor 不：

- 登录账号；
- 创建真实模型 Turn；
- 验证供应商服务当前可达；
- 创建模型 Turn 来分析或修改仓库内容；
- 执行用户 MCP/Skill/Hook 的完整路径；
- 证明 Existing ID 一定在每个组织/账号上下文可恢复。

Probe 会以目标仓库为工作目录启动 Vendor CLI，CLI 仍可能加载用户/项目配置；配置的 wrapper command 本身也是可执行代码，只使用可信仓库与脚本。

## 7. 分层验证

### 7.1 Unit / fixture

证明：parser、correlation、wire serialization、unknown request、exit settlement 等 PairRoom 代码路径。

不证明：当前官方 CLI 的真实行为。

### 7.2 Mock smoke

```bash
make smoke
```

证明：Room 路由、状态、持久化、图片、Reviewer、backup/restore/recovery 的确定性产品路径。

不证明：Claude/Codex 协议、账号、模型或网络。

### 7.3 Doctor

证明：当前机器找到可执行文件并通过有限协议 probe。

不证明：真实 Turn 或所有可选事件。

### 7.4 Native smoke

只有真正启动官方 CLI 并完成场景才算。报告记录 PairRoom build、Vendor version、仓库、实际能力和未覆盖项。

## 8. Vendor 升级后的推荐 smoke

1. `pairroom version --json`；
2. `pairroom doctor --repo ... --json`；
3. `pairroom service --mock` 打开/挂起两个 Room；
4. 在非关键真实 Room 分别给 Claude/Codex 发送文本；
5. 同时发送一张小型 PNG；
6. 对 active Codex Turn 追加 steer，确认 injected 或明确 queued；
7. 触发一项已知审批并 allow/deny；
8. 切换 Reviewer，确认 Claude plan 与 Codex read-only；
9. 停止/激活 Runtime，确认同一 Session/Thread 精确恢复；
10. daemon stop/restart，确认 shutdown 与 orphan settlement；
11. `verify` 关键 Room。

## 9. 不兼容时的处理

| 变化 | 处理 |
|---|---|
| 新增无安全语义字段 | 忽略或原样保留供诊断 |
| 可选事件消失/改名 | 降级 Inspector，公共消息与终态保持 |
| 权限请求新增 | 默认拒绝，明确实现后开放 |
| 必需 initialize/Turn schema 变化 | Runtime error，消息保留可重试 |
| exact resume 失败 | Binding/Runtime error，不新建替代身份 |
| CLI 删除结构化入口 | 不退回终端抓屏/键盘模拟 |
| 只在旧版本可用 | 不建立长期兼容分支，记录当前最低可用事实（如确有需要） |

## 10. 提交兼容修复时

PR 说明：

- 受影响 Vendor 与实际版本；
- 哪个 wire message/flag/handshake 改变；
- 必需还是可选能力；
- 对 Delivery/Processing/Binding/approval 的影响；
- 安全失败路径；
- fixture、doctor 与 native smoke 分别运行了什么；
- 文档与 Changelog 更新。

不要仅以“支持最新版”描述无法复现的变化。

## 11. 官方参考

- Claude Code CLI 参考：<https://code.claude.com/docs/en/cli-reference>
- Claude Agent SDK Python：<https://github.com/anthropics/claude-agent-sdk-python>
- Codex App Server 官方文档：<https://learn.chatgpt.com/docs/app-server>
- Codex App Server source README：<https://github.com/openai/codex/blob/main/codex-rs/app-server/README.md>
