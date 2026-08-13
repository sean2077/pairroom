# PairRoom 架构设计

## 1. 设计目标

PairRoom 的核心约束是：

> 保留官方 Claude Code 和 Codex 的完整 Harness，只实现两者之上的本地三方协作、介入、审批、可观察性与持久化层。

PairRoom 不实现模型调用、Agent loop、工具执行器、上下文压缩、代码补丁算法或供应商账号系统。

## 2. 总体结构

```text
┌────────────────────────── Browser ────────────────────────────┐
│ Shared timeline                                               │
│ rich text · image gallery · reply · thread · search           │
│                                                               │
│ Work Inspector                                                │
│ tools · commands · plan · diff · usage · approvals            │
└──────────────────────── REST / SSE ───────────────────────────┘
                              │
┌──────────────────────── PairRoom daemon ──────────────────────┐
│ HTTP/API        Room engine        Event store                │
│ Media store     Router             Runtime projection         │
│ Role policy     Approval bridge    Git inspector              │
└───────────────┬─────────────────────────────┬─────────────────┘
                │                             │
┌───────────────▼──────────────┐  ┌──────────▼──────────────────┐
│ ClaudeAdapter                │  │ CodexAdapter                │
│ stream-json                  │  │ app-server JSON-RPC         │
│ native control protocol      │  │ thread/turn lifecycle       │
│ image content blocks         │  │ localImage                  │
└───────────────┬──────────────┘  └──────────┬──────────────────┘
                │                             │
          official claude                official codex
```

## 3. 组件边界

### 3.1 Web UI

职责：

- 展示共享时间线；
- 安全渲染 Markdown；
- 上传、预览和浏览图片；
- 发送 `@mention`、引用回复和线程聚焦；
- 显示每个目标的 Delivery/Processing 状态；
- 展示 Runtime 事件、Git Diff 和审批；
- 执行启停、打断、角色切换和重试；
- 通过 SSE 接收增量事件，发现序列缺口时重取 snapshot。

前端是内嵌静态资源：

```text
index.html
styles.css
richtext.js
app.js
favicon.svg
```

没有 npm 依赖、打包器或运行时框架。

### 3.2 HTTP/API 层

职责：

- REST 命令入口；
- SSE 事件流；
- 附件上传、读取和未引用附件删除；
- Git status/diff；
- 会话导出；
- Bearer Token、同源、Host 与安全头检查。

静态页面始终可打开；配置 Token 时，敏感 API 和附件内容必须认证。

### 3.3 Room Engine

Room Engine 是领域状态机，负责：

- 消息、线程、引用和目标；
- Manual/Mentions/Roundtable 路由；
- 用户新指令抢占旧自动接力；
- Delivery 与 Processing 双生命周期；
- Runtime correlation；
- Agent final response 投影；
- 角色切换和审批生命周期；
- 失败重试；
- 重启时瞬态状态收口；
- 将领域事件先持久化再发布。

Engine 不解析 ANSI 终端文本，也不执行模型推理。

### 3.4 Event Store

事件存储采用 append-only JSONL：

```text
events.jsonl
metadata.json
```

重要性质：

- `seq` 单调递增；
- 领域变化在发布到 SSE 前先写入并同步；
- 启动时通过重放恢复 snapshot；
- 只修复损坏的最后半行，不静默跳过中间损坏；
- metadata 记录 Store schema；
- 高于当前二进制支持的未来 schema 会被拒绝。

v0.3 Store schema 为 `3`。

### 3.5 Media Store

媒体库位于：

```text
<data-dir>/attachments/
├── att-<opaque-id>.json
└── att-<opaque-id>.<ext>
```

只接受 PNG、JPEG、GIF、WebP。每个附件包含：

```text
opaque id
safe display name
media type
byte size
SHA-256
width / height
source
created_at
```

关键边界：

- 本机路径只存在于进程内部的 `AgentAttachment.Path`；
- Message、Event、API 和 transcript 只保存安全元数据；
- 每次跨浏览器/Agent 边界前重新校验文件类型、大小、维度与 SHA-256；
- Agent 回答中的图片只允许从当前仓库内部导入；
- 路径经过 canonicalization 和 symlink 边界检查；
- 远程 URL 不进入自动导入流程。

### 3.6 ClaudeAdapter

ClaudeAdapter 启动官方 `claude`：

```text
claude -p
  --input-format stream-json
  --output-format stream-json
  --permission-prompt-tool stdio
  ...optional current flags
```

职责：

- 长驻双向 stream-json；
- 启动后先完成原生 `control_request/initialize` 握手，再接收第一条用户消息；
- 通过 `--permission-prompt-tool stdio` 接收 `can_use_tool` 与 `AskUserQuestion`；
- session ID 创建、持久化和 resume；
- 用户输入队列与每条消息 correlation；
- 文本、工具、Hook、子 Agent 和结果事件投影；
- 将图片编码为原生 base64 image content blocks；
- 将 UI 决策写回 `control_response`；
- 进程退出时收口输入、审批和 control waiter。

Control handshake 成功前不会把 Claude 标记为可用。未知 control request 直接返回协议错误，不做通用“允许”。

Reviewer 策略：

```text
permission mode = plan
disallowed tools = Edit, Write, NotebookEdit, ExitPlanMode
```

控制层仍会对到达的写请求再次 fail closed。

### 3.7 CodexAdapter

CodexAdapter 启动：

```text
codex app-server
```

职责：

- `initialize`；
- `thread/start` / `thread/resume`；
- `turn/start` / `turn/steer` / `turn/interrupt`；
- `clientUserMessageId` correlation；
- 多条用户输入绑定同一 active Turn；
- `localImage` 输入；
- item/plan/diff/usage/command 等结构化事件；
- command/file/additional-permission 审批；
- app-server overload 有界重试；
- 未知 server request fail closed。

Reviewer 的每个 Turn 使用：

```json
{"type":"readOnly"}
```

### 3.8 MockAdapter

MockAdapter 使用相同 Adapter 接口和 RuntimeEvent 流，用于：

- 无供应商 CLI 的产品体验；
- 路由、状态和 UI E2E；
- 崩溃恢复与消息关联测试；
- 确定性发行验证。

Mock 不用于证明真实模型行为。

## 4. 状态事实源

PairRoom 刻意避免多个相互竞争的状态源。

| 信息 | 唯一事实源 |
|---|---|
| 房间消息、角色、路由、审批投影 | PairRoom event log |
| 图片元数据引用 | PairRoom event log |
| 图片二进制 | PairRoom media store |
| Claude 原生会话上下文 | Claude Code |
| Codex 原生 thread/turn 上下文 | Codex App Server |
| Git 工作区内容 | Repository |
| UI | snapshot + SSE 的派生投影 |

PairRoom 不复制供应商完整会话数据库，也不声称能从自己的日志恢复供应商内部推理状态。

## 5. 一条富媒体消息的数据流

```text
Browser selects/pastes images
        │
        ├─ POST /attachments
        │      └─ validate → hash → durable media ID
        │
        └─ POST /messages {text, attachment IDs}
                  │
                  ├─ Engine resolves canonical metadata
                  ├─ Message event fsync
                  ├─ Delivery pending
                  │
                  ├─ Claude boundary
                  │      └─ base64 image block + text envelope
                  │
                  └─ Codex boundary
                         └─ localImage + text input
```

浏览器读取附件时不会直接拿本机文件路径，而是通过认证 API 获取 Blob，再创建页面内 object URL。

## 6. Agent 生成图片的数据流

```text
Agent writes repo/docs/chart.png
Agent final: ![chart](docs/chart.png)
        │
        └─ Media Store discovers candidate
               ├─ resolve canonical path
               ├─ confirm inside repo
               ├─ reject symlink escape/remote URL
               ├─ import immutable copy
               └─ attach safe metadata to final room message
```

这使 Agent 生成的截图和图表可以进入聊天预览，同时不允许回答文本任意导入仓库外文件。

## 7. 审批数据流

### Claude

```text
Claude control_request(can_use_tool)
        │
        ├─ reviewer deny check
        ├─ durable ApprovalRequested event
        ├─ Web Approvals panel
        └─ control_response allow/deny/updatedInput/updatedPermissions
```

### Codex

```text
App Server requestApproval
        │
        ├─ durable ApprovalRequested event
        ├─ Web Approvals panel
        └─ JSON-RPC result/error
```

连接中断后旧审批不能安全复用，因此会过期。

## 8. 角色切换

角色变化遵循：

1. 校验角色；
2. 要求 Runtime 处于安全边界；
3. 先把策略应用到 Adapter；
4. 必要时重启空闲的 Claude 进程并恢复 session；
5. Adapter 成功后再持久化房间角色。

这样 UI 不会显示“Reviewer”，但底层仍按 Driver 权限运行。

## 9. 并发模型

- Engine 用互斥锁保护 snapshot 与 Adapter map；
- 每个 Adapter 负责自己的 stdin/RPC 写锁和 pending correlation；
- Claude Submit 串行化队列变更与 stdin 写入；
- Codex RPC request 使用唯一 ID 与 waiter；
- Store 串行追加并在发布前同步；
- SSE 使用 durable `seq`，瞬态 Runtime event 不推进 replay cursor；
- 前端采用批量 render，避免每个 token delta 触发完整重排。

## 10. 依赖边界

```bash
go list -m all
```

只包含 PairRoom 自身 module。运行时外部程序只有：

```text
git
claude
codex
```

浏览器是用户界面，不需要 Node.js server。

## 11. 当前限制

- 单 daemon / 单 room / 单 repository；
- Reviewer 使用供应商原生约束，不是 OS 级只读文件系统；
- 共享 working tree 中仍应维持单写入者；
- UI 不嵌入完整供应商 TUI；
- 供应商协议变化需要跟随当前公开接口更新；
- 没有内建 TLS、账号系统或多人权限模型。
