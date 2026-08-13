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

## 4. v0.4–v0.8 已交付

### v0.4：Reviewer 工作区隔离

- Git HEAD + dirty tracked patch + untracked regular files 的独立快照；
- 快照来源、摘要、dirty 状态和只读强度可观察；
- 角色交换在安全边界执行，失败回滚；
- Claude/Codex 原生 Reviewer 策略与工作区路径同时约束。

### v0.5：显式消息控制

- `append`、`next_turn`、`supersede` 三种意图；
- 单目标取消、可审计重试和过期自动接力抑制；
- Codex active turn steering 与 next-turn 排队语义分离。

### v0.6：持久化 Work Inspector

- Turn、工具、命令、计划、Diff、用量和完成状态的紧凑摘要；
- 高频输出保持有界，重启后仍可查看关键过程；
- 消息到 Turn/工作项的关联。

### v0.7：验证、备份与恢复

- `verify`、`backup`、`restore`、`diagnostics`；
- 清单、SHA-256、路径穿越、重复文件、超限和原子替换防护；
- diagnostics 默认不包含消息正文和附件字节。

### v0.8：长房间与图片查看

- 首屏窗口化、向前分页和滚动位置保持；
- 每房间草稿、未读、桌面通知；
- 图片旋转、Fit/1:1、25%–800% 缩放和复制。

## 5. 下一阶段：v0.9 发布前安全与运维

- 一次性启动凭据换取 HttpOnly 浏览器会话，清除 URL/Storage Token；
- 浏览器写操作 CSRF、防滥用速率限制和会话过期；
- 明确的本地/远程监听安全模式；
- 发布验收、CI、构建溯源、SBOM 和操作手册。

## 6. v1.0 门槛

- 消息、审批、中断、排队、恢复无幽灵状态；
- Reviewer 工作区边界可观察且不会隐式成为 Writer；
- 所有自动路由都有可解释原因和事件；
- 附件、备份、迁移和恢复经破坏性测试；
- 安全威胁模型、发布流程和跨平台构建稳定；
- 真实 Claude/Codex 账号验证仍由发布者在目标机器执行，不以 Mock 冒充。

## 7. 明确不做

- 托管或转售模型 Key；
- 重写 Claude/Codex Agent loop；
- 用通用模型 API 假装等价于官方 Harness；
- 依赖 ANSI 文本或键盘模拟判断 Turn 状态；
- 默认把私有代码/图片上传到 PairRoom 服务；
- 替代 GitHub/GitLab Review 与 CI。
