# PairRoom 产品规划

## 1. 产品定义

PairRoom 是一个面向现成顶级 Coding Agent 的本地协作控制面：用户、Claude Code 和 Codex 在同一个共享房间中讨论；两个 Agent 继续使用各自官方 Harness；用户能随时介入、观察执行过程、处理审批并控制协作节奏。

它不是：

- 新的 Coding Agent 或模型客户端。
- Claude/Codex 的统一 API 兼容层。
- 固定的“规划 → 实现 → 审查”流水线。
- 依赖某个第三方多 Agent 框架的插件。
- 默认让两个 Agent 无限制自主对话的黑盒系统。

## 2. 核心用户故事

### 三方讨论

作为开发者，我可以把任务同时发给 Claude 和 Codex，让二者在公共房间里提出方案、指出分歧并相互回应，而不需要复制粘贴上下文。

### 实时介入

当 Agent 正在工作时，我可以给 Claude、Codex 或双方发出新指令，并知道消息是启动了新 Turn、注入当前 Turn、进入队列，还是失败。

### 过程检查

我可以在不污染聊天时间线的前提下查看工具、命令、计划、Diff、测试、审批、错误和会话状态。

### 角色约束

我可以指定一个 Driver 和一个 Reviewer，也可以让二者以 Peer 身份讨论；角色切换不要求重启房间。

### 可恢复

关闭应用后，消息和状态仍存在；重新启动后继续复用 Claude session 与 Codex thread。

## 3. 非功能目标

| 目标 | v0.1 验收方式 |
|---|---|
| 独立性 | `go.mod` 无第三方 module；不依赖外部编排 daemon |
| 本地优先 | 默认只绑定 loopback；事件存储在本机 |
| 可观察性 | 每次投递都有目标级状态；运行时事件进入 Inspector |
| 可恢复性 | 事件日志重放后恢复消息、设置、角色、session/thread ID |
| 防失控 | 最大 Agent hop；停止标记；较新用户消息抑制陈旧接力 |
| 低部署成本 | 单个 Go 二进制内嵌 Web UI |
| 跨平台 | Linux/Windows/macOS 构建通过 |

## 4. v0.1.0 MVP（本次交付）

### 范围

- 单房间、单仓库、本机单用户。
- Claude Code stream-json adapter。
- Codex app-server adapter。
- Mock adapter。
- 共享 IM 时间线、@mention、引用回复。
- Manual / Mentions / Roundtable 路由。
- Driver / Reviewer / Peer。
- Started / Injected / Queued / Failed / Skipped。
- Start / Stop / Restart / Interrupt。
- Codex 审批 UI。
- Git status/diff。
- Append-only JSONL + SSE。
- loopback 默认、安全头、Token 和同源校验。

### MVP 验收标准

- [x] Mock 模式中一条 `@all` 用户消息能投递给两个 Agent。
- [x] 两个 Agent 可在 Mentions 模式通过 @mention 接力。
- [x] Roundtable 达到 hop 上限或停止标记后终止。
- [x] 新用户消息可阻止旧 Agent 回答继续自动转发。
- [x] 角色切换后 Reviewer 输入包含只读规则，Codex 使用 read-only sandbox。
- [x] 房间重启后恢复原生 session/thread ID，但运行状态重置为 stopped。
- [x] API、事件存储、路由和协议解析具备自动测试。
- [x] `go test -race ./...` 与 `go vet ./...` 通过。
- [x] Web UI 无构建步骤，可内嵌至单二进制。

## 5. v0.2 — 日常可用

优先解决真实环境磨合，不扩张到大型团队平台。

1. **真实运行时兼容矩阵**
   - 固定验证过的 Claude Code / Codex 最低与推荐版本。
   - 启动时能力协商，避免只检查命令存在。
   - 协议 fixture 回放测试。

2. **更完整的 Inspector**
   - 结构化命令状态与退出码。
   - 文件变化列表和按文件 diff。
   - 测试结果卡片。
   - token / cost 汇总。
   - 每条房间回复关联其运行事件。

3. **交互可靠性**
   - 用户消息优先队列和明确的 cancel/supersede 状态。
   - 目标 Agent 读取/处理状态。
   - 失败投递一键重试。
   - Turn 超时与无响应提醒。

4. **工作区安全**
   - 自动创建 Driver worktree。
   - Reviewer 固定只读 checkout 或文件系统隔离。
   - UI 展示实际 sandbox/permission 能力，而非只显示角色。

5. **产品体验**
   - 房间搜索、消息编辑/补充、快捷命令。
   - 深色/浅色主题与移动端布局。
   - 导出 Markdown/JSON transcript。

## 6. v0.3 — 多房间与可扩展运行时

- 一个 daemon 承载多个 workspace/room。
- 房间列表、归档、模板和任务标题。
- `RuntimeAdapter` 插件协议，仍不依赖第三方编排框架。
- 可选 Gemini CLI/OpenCode adapter。
- 本地 Unix socket / named pipe 控制 API。
- 可选桌面壳，但 Web daemon 仍是核心。
- 远程 Worker，仅通过明确配对和加密隧道连接。

## 7. v1.0 — 稳定协作产品

v1.0 的门槛不是“更多 Agent”，而是以下保证：

- Claude/Codex 版本兼容策略清晰且自动验证。
- 中断、排队、恢复和审批在异常退出后不会产生幽灵状态。
- 共享工作区不会发生两个 Writer 的隐式并发修改。
- 所有自动路由都有可解释原因和可重放事件。
- 安全威胁模型、升级/迁移和备份恢复经过验证。
- 至少一个月的真实项目 dogfooding，无数据损坏和未解释自动回路。

## 8. 明确不做

- 托管模型 Key 或转售模型调用。
- 训练、微调或内建专有模型。
- 替代 GitHub/GitLab 的代码评审和 CI。
- 默认把私有代码上传到 PairRoom 服务。
- 依靠解析终端 ANSI 文本判断协议状态。
- 通过模拟键盘输入声称实现可靠的 active-turn steering。
- 用一个通用 Agent loop 包装所有模型并宣称等价于原生 Harness。
