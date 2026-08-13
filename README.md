# PairRoom

PairRoom 是一个面向 **Claude Code + Codex** 的本地三方协作房间：你、Claude Code 和 Codex 出现在同一条 IM 式公共时间线中，两个 Agent 保留各自官方 Harness，并按房间策略讨论、独立审查和接力工作。

> PairRoom 是全新的独立项目，不依赖 OMA、CCCC、ccteam、wmux、Cherry Studio、Chatbox 或其他 Agent 编排框架。原生模式只启动用户本机已有的官方 `claude` 与 `codex` 命令；Go 核心没有第三方 module，前端没有 npm 依赖和构建步骤。

当前版本：**v0.6.0**

## 核心目标

普通“双模型聊天”通常只利用模型差异，无法完整保留 Claude Code 与 Codex 各自的：

- Agent loop、上下文管理与原生会话恢复
- 项目级 `CLAUDE.md` / `AGENTS.md`
- Skills、MCP、Hooks、子 Agent 与原生工具
- 文件编辑、命令执行、沙箱和审批语义
- 计划、Diff、用量与工具事件

PairRoom 不重写这些能力，只增加一层本地协作、介入与可观察性控制面：

```text
┌──────────────────────── PairRoom Web UI ────────────────────────┐
│ Shared Room: You · Claude Code · Codex                          │
│ rich Markdown · images · @mention · reply · thread · search    │
│                                                                  │
│ Inspector: tools · commands · plans · diffs · approvals · logs │
└───────────────────────────┬─────────────────────────────────────┘
                            │ REST + SSE
┌───────────────────────────▼─────────────────────────────────────┐
│ PairRoom daemon                                                  │
│ room state · routing · event log · media store · lifecycle      │
└───────────────────┬───────────────────────────┬─────────────────┘
                    │                           │
        Claude Code stream-json          Codex app-server
        + native control protocol         + JSON-RPC
                    │                           │
             official `claude`             official `codex`
```

![PairRoom v0.3 rich conversation](docs/images/pairroom-v0.3-desktop.png)

## v0.3.0 重点能力

### 1. 完整的富对话体验

公共时间线现已支持：

- 标题、段落、引用、有序/无序列表和任务列表
- 安全链接、自动链接、`@mention`、粗体、斜体、删除线和行内代码
- Markdown 表格
- 带语言标签和复制按钮的代码块
- 长消息折叠/展开
- 消息全文搜索与“全部 / Agent / 用户”筛选
- 引用回复、跳转原消息和单线程聚焦视图
- 从任意消息直接筛选关联 Turn、工具、命令与运行事件
- 深色/浅色主题、桌面和移动端响应式布局

渲染器只使用安全 DOM API，不执行消息中的 HTML、脚本或事件属性。

### 2. 图片是真正的 Agent 输入，不只是网页装饰

用户可以通过文件选择、拖拽或剪贴板粘贴图片；一条消息最多包含 8 张图片。当前支持：

```text
PNG · JPEG · GIF · WebP
单张最大 5 MiB
单条消息总计最大 20 MiB
```

图片会：

1. 以不透明附件 ID 写入本地持久化媒体库；
2. 进入同一条房间消息；
3. 通过 Claude Code 原生多模态内容块发送给 Claude；
4. 通过 Codex App Server `localImage` 输入发送给 Codex；
5. 在聊天中显示缩略图、尺寸、格式和大小；
6. 支持同消息画廊、上一张/下一张、50%–400% 缩放和打开原图。

![PairRoom image lightbox](docs/images/pairroom-v0.3-lightbox.png)

Agent 若在仓库内生成截图、图表或架构图，并在最终回答中使用相对 Markdown 图片路径引用，PairRoom 会在安全确认文件仍位于仓库内后导入并展示预览。

安全边界：

- 房间记录和 API 不暴露本机绝对附件路径；
- 图片内容按 SHA-256 校验，防止持久化后被同尺寸文件静默替换；
- 远程 Markdown 图片不会自动加载，避免无意泄漏访问行为；
- SVG、HTML 和其他主动内容不作为消息附件接受；
- 图片只能通过认证后的本地 API 获取，并使用 `nosniff` 与限制性 CSP。

### 3. Claude 与 Codex 的统一审批界面

Codex 原有的命令执行、文件修改和额外权限审批继续保留。

v0.3 又接入了 Claude Code 原生双向 control protocol：PairRoom 在会话启动时完成 `initialize` 握手，并将以下请求投影到统一 Approvals 面板：

- Claude 工具权限请求
- `AskUserQuestion` 单选、多选和自由文本问题
- Codex 命令、文件修改和追加权限请求

用户可以允许一次、允许本会话或拒绝。未知的高权限请求默认 **fail closed**；中断、停止、重启或运行时错误会使连接相关审批过期。

### 4. Reviewer 角色具有原生保护

角色不再只是一段提示词：

- **Claude Reviewer**：切换到 Claude Code 原生 `plan` permission mode，并移除 `Edit`、`Write`、`NotebookEdit` 与 `ExitPlanMode`；即使写工具请求仍到达控制层，也会被 PairRoom 自动拒绝。
- **Codex Reviewer**：每个 Turn 使用 App Server 原生 `readOnly` sandbox policy。
- **Driver / Peer**：恢复用户配置的 Claude permission mode 或 Codex sandbox policy。

角色切换只允许在安全 Turn 边界进行；若 Agent 正在工作或等待审批，必须先打断或停止。

这些是供应商原生策略，不等价于操作系统级只读挂载。默认仍建议保持一个 Driver、一个 Reviewer，不要让两者同时写同一个工作树。

### 5. 三方 IM 房间与协作控制

支持：

```text
@claude · @codex · @all · @peer · @human
```

三种路由模式：

- `manual`：Agent 回答不会自动转给 Peer；
- `mentions`：只有显式提到 Peer 才继续，默认推荐；
- `roundtable`：双方自动往返，但受最大 hop、停止标记和用户插话约束。

用户新消息拥有最高优先级。旧 Turn 的最终回答仍保留用于审计，但不会继续触发过期的自动接力。

### 6. 消息生命周期与可审计重试

PairRoom 将“消息是否进入 Harness”与“Agent 是否处理完成”分开。

投递状态：

| 状态 | 含义 |
|---|---|
| `pending` | 消息已经持久化，等待提交 |
| `started` | 启动了新的原生 Turn |
| `injected` | 注入正在运行的 Codex Turn |
| `queued` | 等待安全 Turn 边界 |
| `failed` | 未成功提交给运行时 |
| `skipped` | 在进入运行时前因重启或策略作废 |

处理状态：

| 状态 | 含义 |
|---|---|
| `waiting` | 已进入队列，尚未开始 |
| `working` | 原生运行时正在处理 |
| `completed` | 运行时确认完成 |
| `cancelled` | 被打断、停止或异常重启取消 |
| `failed` | 输入或执行失败 |
| `superseded` | 被明确的新指令取代 |

失败、取消或跳过的目标可单独重试。重试会创建带 `retry_of` 引用的新消息，不修改原历史，也不会重复执行已经成功的另一个 Agent。

### 7. Work Inspector

- Agent 状态、Session/Thread、当前 Turn
- 流式文本与公共最终回复分层展示
- 工具调用、命令输出、计划、Diff、用量、错误和日志
- 按 Agent、消息或 Turn 筛选过程
- 当前 Git 状态与 staged/unstaged Diff
- Claude/Codex 统一审批
- 长时间无运行事件提醒
- Runtime 路径、当前版本、协议、能力和诊断警告

## 运行时策略：跟随最新官方版本

PairRoom 不维护历史版本兼容矩阵，也不把长期兼容旧 CLI 当成产品目标。策略是：

1. 对齐 Claude Code 和 Codex 当前稳定版公开接口；
2. 使用 `doctor` 检查当前机器上的协议入口和必要能力；
3. 可选能力缺失时明确降级；
4. 必要协议缺失时直接停止，不猜测、不解析终端 ANSI 输出；
5. 供应商升级后运行一次 `doctor` 和 Mock/非关键仓库 smoke test。

详见 [`docs/RUNTIME_COMPATIBILITY.md`](docs/RUNTIME_COMPATIBILITY.md)。

## 快速开始

### 1. Mock 模式

不需要安装或登录任何 Agent：

```bash
go run ./cmd/pairroom serve --repo . --mock
```

浏览器会打开 `http://127.0.0.1:7332`。Mock Agent 会模拟三方讨论、接力、运行过程和完成状态。

### 2. 检查真实环境

```bash
go run ./cmd/pairroom doctor --repo /path/to/project
```

机器可读报告：

```bash
pairroom doctor --repo /path/to/project --json
```

需要：

- Git
- 官方 Claude Code CLI，命令为 `claude`
- 官方 Codex CLI，命令为 `codex`，且包含 `codex app-server`
- 两个 CLI 已按官方方式完成登录
- Go 1.23+（仅从源码构建 PairRoom 时需要）

`doctor` 不创建供应商会话，也不读取仓库内容。

### 3. 启动真实 Pair

```bash
go run ./cmd/pairroom serve --repo /path/to/project
```

或构建单文件二进制：

```bash
make build
./dist/pairroom serve --repo /path/to/project
```

Windows PowerShell：

```powershell
go build -trimpath -o dist\pairroom.exe .\cmd\pairroom
.\dist\pairroom.exe serve --repo C:\path\to\project
```

## 常用命令

```bash
# 不自动启动 Agent
pairroom serve --repo . --auto-start=false

# 默认：只有显式 @Peer 时继续
pairroom serve --repo . --routing mentions

# 自动轮桌讨论，最多 8 次 Agent-to-Agent 接力
pairroom serve --repo . --routing roundtable --max-hops 8

# 完全手动路由
pairroom serve --repo . --routing manual

# 5 分钟没有运行事件时提醒；-1 关闭
pairroom serve --repo . --stall-warning-seconds 300

# 指定状态目录
pairroom serve --repo . --data-dir ./.pairroom-local

# 可选模型/推理配置覆盖
pairroom serve --repo . \
  --claude-model your-claude-model \
  --codex-model your-codex-model \
  --codex-effort high
```

查看完整参数：

```bash
pairroom serve -help
pairroom doctor -help
```

## 配置文件

复制 [`examples/pairroom.example.json`](examples/pairroom.example.json)：

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
    "approval_policy": "unlessTrusted",
    "sandbox": "workspaceWrite"
  }
}
```

启动：

```bash
pairroom serve --config ./pairroom.json --repo /path/to/project
```

命令行参数覆盖配置文件。

## 推荐使用方式

默认保持一个写入者、一个只读审查者：

```text
Claude Code  Driver
Codex        Reviewer
```

第一条消息示例：

```text
@all 请分别独立理解仓库和任务。Claude 提出实现方案，Codex 检查并发、兼容性和测试遗漏；没有达成共识前不要修改代码。
```

开始实现和审查：

```text
@claude 按已确认方案实现并运行测试。完成后 @codex 做完整 diff 审查。
```

随时介入：

```text
@codex 暂停当前方向，先检查对象生命周期。
@claude 不要改变公开 API，沿用现有 executor。
@all 先停止实现，解释两种方案的权衡。
```

可以直接附上截图、错误界面、架构图或论文图，让两个 Agent 在同一消息中读取和讨论。

### Roundtable 停止标记

Agent 可在最终回复末尾使用：

```text
[PAIRROOM:CONTINUE]
[PAIRROOM:CONSENSUS]
[PAIRROOM:WAIT]
[PAIRROOM:BLOCKED]
[PAIRROOM:DONE]
```

这些标记不显示在公共聊天。除 `CONTINUE` 外都会停止自动接力；达到 hop budget 或出现更新的用户消息同样停止。

## 数据目录与导出

默认状态目录按仓库绝对路径计算，位于用户配置目录：

```text
pairroom/rooms/<repo-name>-<path-hash>/
├── events.jsonl
├── metadata.json
├── attachments/
│   ├── att-<id>.json
│   └── att-<id>.<ext>
└── runtime/
    └── claude-pairroom-prompt.md
```

普通 JSON/Markdown 导出包含讨论与附件元数据，不默认包含可能含有命令输出、路径和工具参数的 Inspector event tail。完整取证 JSON：

```text
GET /api/v1/export?format=json&include_events=1
```

PairRoom 不保存供应商 API Key；身份和凭据仍由官方 CLI 管理。

## 当前边界

- 一个 daemon 当前承载一个房间和一个仓库。
- 两个 Agent 默认共享一个 working tree；Reviewer 原生策略不是 OS 级隔离。
- 网页展示结构化过程，不嵌入供应商完整终端 TUI。
- 远程 Markdown 图片默认不加载；需要先作为本地附件上传。
- Claude/Codex 真实登录、网络、账号权限和供应商服务状态不属于 PairRoom 的可控范围。
- 内建 HTTP 没有 TLS，不应直接暴露公网。

## 验证

```bash
make check
```

包括：

```text
go test ./...
go test -race ./...
go vet ./...
node --check internal/server/assets/app.js
node --check internal/server/assets/richtext.js
git diff --check
```

浏览器 E2E 覆盖富 Markdown、双图片上传、原生消息附件、画廊/缩放、回复、线程、搜索、筛选、长消息、Inspector 关联、移动端布局与外部图片不自动请求。详见 [`docs/VALIDATION.md`](docs/VALIDATION.md)。

## 文档

- [架构设计](docs/ARCHITECTURE.md)
- [房间与运行时协议](docs/PROTOCOL.md)
- [富对话与图片设计](docs/RICH_CONVERSATION.md)
- [最新 Runtime 跟随策略](docs/RUNTIME_COMPATIBILITY.md)
- [升级说明](docs/UPGRADING.md)
- [验证记录](docs/VALIDATION.md)
- [产品规划](docs/PRODUCT_PLAN.md)
- [安全说明](SECURITY.md)

## License

MIT
