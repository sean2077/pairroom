# PairRoom 产品边界与演进计划

> [项目首页](../README.md) · [文档首页](README.md) · [架构](ARCHITECTURE.md) · [Changelog](../CHANGELOG.md)

本文记录产品定义、当前 `main` 的稳定边界、已交付里程碑、后续优先级与明确非目标。具体已实现事实以源码、测试和 `CHANGELOG.md` 为准。

## 1. 产品定义

PairRoom 是现有顶级 Coding Agent Harness 之上的本地协作控制面。一个人、官方 Claude Code 和官方 Codex 在同一可见房间中讨论、实现、审查和接受人工介入；两个官方 Harness 继续拥有模型推理、工具、上下文、会话、Skills、MCP、Hooks、sandbox 与账号。

PairRoom 不是：

- 模型 API 网关；
- 自制通用 Agent loop；
- 终端录屏/ANSI 解析器；
- 托管凭据或云同步服务；
- GitHub/GitLab PR、CI 或代码所有权的替代品。

## 2. 产品原则

| 原则 | 契约 |
|---|---|
| Native Harness | 只接官方结构化接口，不用通用模型 API 模拟 Claude Code/Codex |
| Three-party visibility | 人和两个 Agent 共享一条可读公共时间线 |
| Process separation | 结论在聊天；工具、命令、计划、Diff、用量、审批在 Inspector |
| Human priority | 人的新指令可取消/替代过期自动接力 |
| Single writer by default | Driver 写 live tree，Reviewer 读独立 snapshot + 原生只读策略 |
| Durable honesty | Delivery 与 Processing 分离，终态显式，重试生成新记录 |
| Fail closed | 不安全 workspace、未知高权限请求、损坏恢复与身份冲突被拒绝 |
| Local-first privacy | 无 PairRoom 云、遥测、托管 credential 或远程图片自动抓取 |
| Explicit capability | 未实现能力显示为边界，不伪装保存成功 |
| Source-aligned docs | 文档与当前命令、认证和数据模型同步 |

## 3. 已交付的 1.0 Room 内核

- **v0.1**：原生 Adapter、共享 Room、路由、角色、SSE/JSONL、Git Inspector、Mock；
- **v0.2**：Delivery/Processing、重试、Runtime probe、重启收口、export/security hardening；
- **v0.3**：安全 Markdown、原生多模态图片、图库、Claude control approvals、Reviewer policy；
- **v0.4**：包含 dirty/untracked 的 Reviewer Git snapshot、boundary metadata、角色切换回滚；
- **v0.5**：append/next-turn/supersede/cancel 与 stale-handoff suppression；
- **v0.6**：durable Turn/Tool/Command/Plan/Diff/Usage summary；
- **v0.7**：strict verify、自验证 backup/restore、redacted diagnostics；
- **v0.8**：长房间分页、draft、unread、notification、图片 viewer；
- **v0.9**：Room HttpOnly browser session、CSRF、query-token removal、rate limiting；
- **v1.0**：版本契约、CI/release automation、多平台构建、SBOM/provenance、运维/隐私/支持文档。

1.0 的核心边界是“一个 Room = 一个 Git worktree + 一个人 + 一个 Claude + 一个 Codex”。

## 4. 当前 `main` 的 Service 层

当前 `main` 已在 1.0 Room 内核上加入：

- 一个本地 Service 管理多个 Project 与多个 Room；
- canonical Git worktree identity 与去重；
- Claude Session / Codex Thread 的 new/existing Binding；
- deferred Binding materialization 与全局 ownership；
- 原子 Room provisioning；
- 按 Room 惰性 Runtime、全局 capacity、idle suspend 与 FIFO queue；
- Room create/rename/archive/restore 与 Legacy import；
- Management Shell 的 Overview/Projects/Runtimes/Settings；
- systemd/launchd/Task Scheduler daemon；
- numeric-loopback-only listener；
- Management 与 Room 各自独立的 fragment Bearer bootstrap、HttpOnly Session 和 CSRF 认证模型，且保留 API 客户端直接 Bearer 兼容。

因此当前产品边界不再是“一个 daemon 只能服务一个 repository/room”。准确表述为：

```text
one local Service per data root
multiple canonical Git Projects
multiple durable Rooms per Project
bounded concurrently active Room Runtimes
one human + one Claude + one Codex per Room
one Driver + one Reviewer by default
```

## 5. 当前稳定约束

### 5.1 Project / Room

- 一个 Project 对应一个 canonical Git worktree；
- 一个 Room 永久属于一个 Project；
- 一个 Room 只有一个 Claude participant 和一个 Codex participant；
- Binding Identity 在 Service 内全局唯一；
- 归档不删除数据或释放 Binding；
- 当前没有 Project removal 或永久 Room deletion。

### 5.2 Runtime

- Runtime 按 Room 激活；
- 全局 capacity 有界；
- 活动 Turn 不为容量回收而中断；
- busy/cleanup-uncertain Runtime 不可被假装挂起；
- Runtime policy 通过进程参数配置，当前不热修改。

### 5.3 协作

- 人的新指令优先；
- 默认一个写入者、一个审查者；
- Reviewer snapshot 不是容器级隔离；
- PairRoom 时间线不导入 Binding 前 Vendor Transcript；
- UI 展示结构化过程，不嵌入完整 Vendor TUI。

### 5.4 部署

- 单用户、本地优先；
- numeric loopback only；
- 无内建 TLS、远程 worker、云同步或团队 RBAC；
- 远程访问只通过 SSH local forwarding。

## 6. 后续优先级

### P0：把 Service 体验打磨成默认产品入口

- Management 与 Room 之间更清晰的导航、状态连续性与错误解释；
- 更完整的多 Project 搜索、筛选、最近使用与 attention workflow；
- daemon 配置可观察性与安全重装辅助；
- 容量、queue、failed cleanup 和 Project unavailable 的恢复指引；
- 文档/CLI/UI 的持续一致性检查。

### P1：深化双 Agent 协作质量

- 更清晰的 Driver→Reviewer handoff 与验证证据；
- per-file Diff、测试结果和未决问题卡片；
- 更强的 thread/reply/mention 可读性；
- 图片、产物和长会话的浏览体验；
- 自动接力的可解释性与停止条件。

### P2：可靠性与迁移

- Service 级多 Room 备份编排与可视化；
- Registry/Legacy import 的更强恢复工具；
- daemon definition 的可导出/比较配置；
- 升级前检查和回滚证据；
- 更多跨平台真实生命周期测试。

### P3：可选强隔离

- 在不削弱默认可移植性的前提下，提供容器/VM/只读 mount 级 Reviewer 方案；
- 明确隔离强度并可观测，而不是一个模糊“安全模式”开关。

### P4：受控扩展

只有在 Claude+Codex 原生路径长期稳定后，才评估：

- versioned external RuntimeAdapter protocol；
- 额外 Agent vendor；
- 更复杂的多参与者房间。

扩展不能迫使现有原生 Adapter 降级为最小公分母。

## 7. 明确非目标

- 托管、转售或代理模型 key；
- 替换 Vendor Agent loop；
- 从 ANSI/TUI 文本反推关键状态；
- 自动把私有代码或图片上传到 PairRoom 云；
- 直接绑定 LAN/公网并自行承担公网安全；
- 内置多人账号、组织、权限与计费；
- 把 Reviewer snapshot 宣称为安全 sandbox；
- 同一 live tree 上默认允许两个 Agent 并行写；
- 为过时 Vendor CLI 维护永久兼容矩阵；
- 用 Mock 测试声明真实 Vendor E2E 已通过。

## 8. 产品决策门槛

新增功能应回答：

1. 是否保留官方 Harness 的完整能力；
2. 是否让人更容易观察、介入或审计；
3. 事实源与失败恢复在哪里；
4. 是否扩大本地权限、出站数据或公网暴露；
5. 多 Room 下身份、容量与单写者是否仍一致；
6. Mock、单测、浏览器 smoke 和真实 Vendor E2E 各验证了什么；
7. CLI、UI、运维、安全、隐私和升级文档需要同步哪些内容。

无法回答这些问题的功能，不应仅因为“看起来像聊天工具”而进入核心产品。
