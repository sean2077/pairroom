# PairRoom 产品规划

## 1. 产品定义

PairRoom 是面向现成顶级 Coding Agent 的本地协作控制面：用户、Claude Code 和 Codex 在一个共享房间讨论；两个 Agent 继续运行官方 Harness；用户随时介入、查看执行过程、处理审批并控制协作节奏。

它不是新的 Agent loop、模型网关、固定流水线，也不依赖第三方多 Agent daemon。

## 2. 核心产品原则

| 原则 | 含义 |
|---|---|
| Harness 原生 | 官方 Claude Code/Codex 负责推理、工具、会话和上下文 |
| 三方可见 | 用户与两位 Agent 共享同一公共时间线 |
| 结论与过程分层 | 聊天保留可读结论，工具/命令/Diff 在 Inspector |
| 用户优先 | 用户新指令阻止旧结果继续自动扩散 |
| 权限不降级 | 未知高权限请求 fail closed |
| 消息不丢失 | 先持久化再提交，失败可审计重试 |
| 单写入者优先 | 默认 Driver + Reviewer，而不是两个隐式 Writer |
| 最新优先 | 跟随当前官方协议，不维护历史版本矩阵 |

## 3. 已交付

### v0.1.0：MVP

- Claude stream-json、Codex App Server、Mock Adapter；
- 三方时间线、@mention、引用；
- Manual/Mentions/Roundtable；
- Driver/Reviewer/Peer；
- 启停、重启、打断、Codex 审批；
- Git status/diff；
- JSONL + SSE；
- 本地安全默认值。

### v0.2.0：可靠性与可观察性

- Delivery 与 Processing 分离；
- started/injected/queued 与 waiting/working/completed 等状态；
- per-target 可审计重试；
- RuntimeInfo、doctor、stall 提醒；
- 重启收口和 schema 迁移；
- 搜索、主题、导出和 Inspector correlation；
- DNS rebinding、同源和敏感导出防护。

### v0.3.0：富对话、原生审批与角色保护

#### 富对话

- [x] 安全 Markdown：标题、引用、列表、任务、表格、代码块；
- [x] 引用跳转、线程聚焦、复制、长消息折叠；
- [x] 搜索、参与者筛选、桌面/移动响应式布局；
- [x] PNG/JPEG/GIF/WebP 持久化附件；
- [x] 文件选择、拖拽、剪贴板粘贴；
- [x] 图片画廊、灯箱、前后导航、缩放、原图；
- [x] Agent 生成的仓库内图片自动发现和预览；
- [x] 远程 Markdown 图片不自动加载。

#### Harness 原生集成

- [x] Claude 原生多模态 image content blocks；
- [x] Codex App Server `localImage`；
- [x] Claude initialize/control handshake；
- [x] Claude 工具权限和 `AskUserQuestion` 统一 UI；
- [x] Codex/Claude 未知高权限请求 fail closed；
- [x] Claude Reviewer plan mode + 写工具拒绝；
- [x] Codex Reviewer read-only sandbox。

#### 附件安全

- [x] 不透明 ID，不暴露 host path；
- [x] 内容类型、真实解码、维度和像素上限；
- [x] SHA-256 不可变校验；
- [x] symlink/仓库逃逸防护；
- [x] 已进入 transcript 的附件不可删除；
- [x] ETag、nosniff、CSP 和认证读取。

## 4. 下一阶段：v0.4 工作区隔离与过程卡片

### P0：Reviewer 工作区隔离

目标不是仅依赖提示词，而是让“只读审查”在工作区层更容易验证。

1. 设计 Reviewer snapshot/worktree，不让 Reviewer 直接修改 Driver 工作树；
2. 在审查开始时记录 source HEAD、dirty patch hash、untracked manifest；
3. 将原生 permission/sandbox、文件系统能力与实际路径同时显示；
4. Role 切换必须在安全边界完成，并解释 session/cwd 影响；
5. 隔离不可用的平台明确降级，不宣称绝对只读。

这里需要特别处理未提交修改：普通 detached worktree 看不到 Driver 的 dirty state，因此不能只做 `git worktree add` 就声称已完成审查隔离。

### P0：结构化 Work Inspector

- 命令卡片：状态、exit code、持续时间、可折叠输出；
- 文件变化卡片：新增/修改/删除、per-file diff；
- 测试卡片：框架、通过/失败、耗时、失败摘要；
- Plan 与 Finding 卡片；
- 每 Turn 的 token、费用和耗时汇总；
- Vendor 原始事件默认折叠，必要时展开。

### P1：消息控制

- 区分“补充当前 Turn”和“替换/取消旧指令”；
- 对 Claude/Codex 分别显示 steer/queue/cancel 结果；
- 显式 supersede 单个目标；
- 会话标题、归档、固定消息和任务模板；
- 图片标注、局部裁剪和将截图区域作为新附件发送。

### P1：诊断与真实 dogfooding

- 最新官方版本的自动 smoke fixture，而不是历史版本矩阵；
- 脱敏 diagnostics 包；
- 真实长 Turn、resume、compaction、子 Agent 和审批回放；
- Windows/macOS/Linux 各至少一条真实使用路径。

## 5. v0.5 多房间与可扩展 Runtime

- 一个 daemon 承载多个 room/workspace；
- 房间列表、归档、标签和模板；
- Unix socket / Windows named pipe 管理 API；
- RuntimeAdapter 外部进程协议；
- 可选 Gemini CLI/OpenCode；
- 可选桌面壳，Web daemon 仍是核心；
- 远程 Worker 只经明确配对和加密隧道连接。

## 6. v1.0 门槛

- 消息、审批、中断、排队、恢复无幽灵状态；
- Reviewer 工作区边界可观察且不会隐式成为 Writer；
- 所有自动路由都有可解释原因和事件；
- 附件、备份、迁移和恢复经破坏性测试；
- 至少一个月真实项目 dogfooding，无数据损坏和未解释自动回路；
- 安全威胁模型和发布流程稳定。

## 7. 明确不做

- 托管或转售模型 Key；
- 重写 Claude/Codex Agent loop；
- 用通用模型 API 假装等价于官方 Harness；
- 依赖 ANSI 文本或键盘模拟判断 Turn 状态；
- 默认把私有代码/图片上传到 PairRoom 服务；
- 替代 GitHub/GitLab Review 与 CI。
