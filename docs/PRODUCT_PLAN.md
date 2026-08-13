# PairRoom 产品规划

## 1. 产品定义

PairRoom 是面向现成顶级 Coding Agent 的本地协作控制面：用户、Claude Code 和 Codex 在同一个共享房间讨论；两个 Agent 继续运行各自官方 Harness；用户能随时介入、观察执行过程、处理审批并控制协作节奏。

它不是：

- 新的 Coding Agent 或模型客户端。
- Claude/Codex 的统一模型 API 兼容层。
- 固定的“规划 → 实现 → 审查”流水线。
- 依赖第三方多 Agent daemon 的插件。
- 默认让两个 Agent 无限制自主互答的黑盒系统。

## 2. 核心用户故事

### 三方讨论

开发者可以把任务同时发给 Claude 和 Codex，让二者在公共房间提出方案、暴露分歧并相互回应，而不需要复制粘贴。

### 实时介入

Agent 工作时，用户可以向 Claude、Codex 或双方发送新指令，并看到消息是启动 Turn、注入 Turn、进入队列、执行中、完成、取消还是失败。

### 过程检查

聊天时间线保持结论可读，工具、命令、计划、Diff、测试、审批、错误和会话状态在 Inspector 中按 Agent、消息和 Turn 查看。

### 角色约束

用户可以指定一个 Driver 和一个 Reviewer，也可以让二者以 Peer 身份讨论；切换角色不需要重建房间。

### 可恢复与可审计

关闭应用后，消息、状态和供应商 session/thread ID 仍存在；失败重试不改历史，所有重要状态变化可从事件日志重放。

## 3. 非功能目标

| 目标 | 验收方向 |
|---|---|
| 独立性 | Go 核心无第三方 module；不依赖外部编排 daemon |
| 原生性 | 只驱动官方 Claude Code/Codex Harness，不重写 Agent loop |
| 本地优先 | 默认 loopback；状态留在本机 |
| 可观察性 | transport 与 processing 分开；运行事件可关联消息 |
| 可恢复性 | 事件重放恢复持久状态；瞬态状态异常退出后明确收口 |
| 防失控 | 最大 hop、停止标记、用户抢占、fail-closed 审批 |
| 低部署成本 | 单个 Go 二进制内嵌 Web UI |
| 跨平台 | Linux/Windows/macOS 构建通过 |

## 4. 已交付：v0.1.0 MVP

- 单房间、单仓库、本机单用户。
- Claude Code stream-json adapter。
- Codex app-server adapter。
- Mock adapter。
- 共享 IM 时间线、@mention、引用回复。
- Manual / Mentions / Roundtable。
- Driver / Reviewer / Peer。
- 基础投递状态、启停/重启/打断。
- Codex 审批、Git status/diff。
- Append-only JSONL + SSE。
- loopback 默认、Token、同源和安全头。

## 5. 已交付：v0.2.0 可靠性与可观察性

### 运行时兼容

- [x] `pairroom doctor --json`。
- [x] 可执行文件路径、版本、协议入口和能力探测。
- [x] Claude 必需/可选参数协商与能力降级。
- [x] UI 展示 RuntimeInfo、实际版本、协议、能力和警告。
- [x] Codex 请求只使用公开稳定字段。

### 消息生命周期

- [x] Delivery 与 Processing 分离。
- [x] waiting/working/completed/cancelled/failed durable projection。
- [x] 同一 Codex Turn 内多次 steer 的输入全部 settle。
- [x] 运行时失败不覆盖已成功的 transport disposition。
- [x] stop/restart/process exit 对遗留输入和审批收口。
- [x] 可审计 per-target retry。
- [x] Turn 无事件提醒。

### 产品体验

- [x] 消息搜索。
- [x] 深色/浅色主题。
- [x] Markdown/JSON transcript export。
- [x] 从消息筛选关联 Turn 与 Inspector 事件。
- [x] SSE gap 自动重新同步。
- [x] 更清晰的运行时和处理状态提示。

### 数据与安全

- [x] Store schema metadata、旧版升级和未来版本拒绝。
- [x] JSONL 半行修复后安全继续追加。
- [x] Tokenless loopback Host 检查，降低 DNS rebinding 风险。
- [x] 普通 transcript 默认不导出 verbose Inspector events。
- [x] 移除大小写冲突文档文件，避免 Windows/macOS 包解压覆盖。

## 6. 下一阶段：v0.3 工作区安全与真实环境兼容

优先级高于增加更多 Agent。

### P0：真实 CLI dogfooding

1. 建立 Claude Code/Codex 版本兼容矩阵。
2. 保存脱敏协议 fixture，加入回放回归测试。
3. 完成真实账号下 session/thread resume、interrupt、approval、compaction 验证。
4. 为协议变化提供明确的 degraded/unsupported 状态，而不是隐式猜测。
5. 增加 `pairroom diagnostics` 支持包：只导出脱敏版本、状态和错误，不导出代码内容。

### P0：单写入者强约束

1. 可选 Driver worktree。
2. Reviewer 只读 checkout 或操作系统级写保护。
3. 展示“角色策略”与“实际 sandbox 能力”的差异。
4. 发现两个 Agent 都具备写权限时给出显著警告。
5. Agent 角色切换时安全迁移工作区绑定。

### P1：Inspector 结构化

1. 命令状态、exit code、持续时间。
2. 文件变化列表和 per-file diff。
3. 测试结果卡片。
4. Token、费用和耗时的 per-turn / session 汇总。
5. 可折叠原始 vendor event，默认展示 canonical 摘要。

### P1：消息控制

1. 显式 Cancel/Supersede 单条目标处理。
2. “补充当前指令”与“替换当前指令”分开。
3. 用户消息对两个 Agent 的独立投递/处理时间线。
4. 失败重试可选择复用或新建 thread。
5. 会话归档、标题和任务模板。

## 7. v0.4 多房间与可扩展运行时

- 一个 daemon 承载多个 workspace/room。
- 房间列表、归档、模板和标签。
- 本地 Unix socket / Windows named pipe 管理 API。
- RuntimeAdapter 外部进程协议；核心仍不依赖第三方编排框架。
- 可选 Gemini CLI/OpenCode adapter。
- 可选桌面壳；Web daemon 仍是核心。
- 远程 Worker 仅通过明确配对和加密隧道连接。

## 8. v1.0 稳定门槛

v1.0 的门槛不是“支持更多模型”，而是：

- Claude/Codex 兼容策略清晰且自动验证。
- 中断、排队、恢复和审批在异常退出后不产生幽灵状态。
- 默认共享工作区不会发生两个 Writer 的隐式并发修改。
- 所有自动路由都有可解释原因和可重放事件。
- 安全威胁模型、升级/迁移、备份恢复经过验证。
- 至少一个月真实项目 dogfooding，无数据损坏和未解释自动回路。

## 9. 明确不做

- 托管模型 Key 或转售模型调用。
- 训练、微调或内建专有模型。
- 替代 GitHub/GitLab 的代码评审和 CI。
- 默认上传私有代码到 PairRoom 服务。
- 依靠解析终端 ANSI 文本判断协议状态。
- 模拟键盘并声称可靠实现 active-turn steering。
- 用一个通用 Agent loop 包装所有模型并宣称等价于官方 Harness。
