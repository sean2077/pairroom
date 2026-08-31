# PairRoom 文档

> [项目首页](../README.md) · [快速上手](GETTING_STARTED.md) · [核心概念](CONCEPTS.md) · [CLI 参考](CLI_REFERENCE.md) · [排障](TROUBLESHOOTING.md)

这里是 PairRoom 文档的统一入口。README 负责说明产品价值和最短路径；本目录按“教程、操作指南、概念解释、参考资料”拆分，避免同一行为在多个文件中重复且互相漂移。

## 从这里开始

| 你现在要做什么 | 首选文档 |
|---|---|
| 第一次运行，先验证 UI 与流程 | [快速上手](GETTING_STARTED.md) |
| 不理解 Project、Room、Binding 或 Runtime | [核心概念](CONCEPTS.md) |
| 查命令、参数、默认值和限制 | [CLI 参考](CLI_REFERENCE.md) |
| 浏览器、Agent、容量、锁或数据出问题 | [排障手册](TROUBLESHOOTING.md) |
| 长期后台运行、远程访问、备份和升级 | [运维手册](OPERATIONS.md) |
| 修改代码或提交 PR | [开发者指南](DEVELOPMENT.md) 与 [贡献指南](../CONTRIBUTING.md) |

## 使用者文档

### 入门与日常使用

- [快速上手](GETTING_STARTED.md)：从构建、Mock 到真实 Runtime 和后台 Service；
- [核心概念](CONCEPTS.md)：Project、Room、Binding、Runtime、Turn、Delivery/Processing、角色与路由；
- [Multi-Project / Multi-Room Service](MULTI_ROOM_SERVICE.md)：Service 拓扑、Provisioning、容量、生命周期和恢复；
- [Management Shell](MANAGEMENT_SHELL.md)：页面路由、操作、状态、能力和浏览器 Session/内存边界；
- [富对话与图片](RICH_CONVERSATION.md)：Markdown、附件、原生多模态投递和安全限制。
- [Flexible workflows and provider profiles](FLEXIBLE_WORKFLOWS_AND_PROVIDERS.md)：自然语言阶段编排、审批门与独立供应商配置。

### 操作与恢复

- [CLI 参考](CLI_REFERENCE.md)：所有顶层命令和 daemon 子命令；
- [运维手册](OPERATIONS.md)：前台/后台部署、SSH 转发、数据、备份、升级和事故响应；
- [排障手册](TROUBLESHOOTING.md)：按症状定位问题；
- [升级与回滚](UPGRADING.md)：Store 迁移、备份、首次启动和回滚边界；
- [隐私模型](PRIVACY.md)：本地数据、浏览器状态、供应商数据路径和删除；
- [安全策略](../SECURITY.md)：威胁模型、安全默认值和漏洞报告；
- [支持范围](../SUPPORT.md)：Issue 前置检查与可分享信息。

## 实现与协议参考

- [架构设计](ARCHITECTURE.md)：进程拓扑、组件、事实源、并发、认证和故障边界；
- [Room 协议](PROTOCOL.md)：Event、snapshot、REST/SSE、消息和运行时投影；
- [Runtime 跟随策略](RUNTIME_COMPATIBILITY.md)：当前官方 Claude Code/Codex 协议基线和降级原则；
- [产品计划](PRODUCT_PLAN.md)：稳定边界、已交付能力、非目标和后续决策原则；
- [开发者指南](DEVELOPMENT.md)：包结构、测试层次、不可破坏的系统不变量和文档维护规则。

## 验证与发布

- [验证说明](VALIDATION.md)：PairRoom 可控验证与真实供应商 E2E 的区别；
- [发布检查清单](RELEASE_CHECKLIST.md)：发布门禁；
- [v1.0.0 Release Notes](RELEASE_NOTES_v1.0.0.md)：已发布基线的历史说明；
- [PR 记录](PULL_REQUEST.md)：实现阶段的 PR 说明；
- [cc-connect UX 调研](CC_CONNECT_UX_RESEARCH.md)：管理体验参考与适配决策。

历史 Release Notes 和 Validation Record 是当时版本的证据，不应被修改成当前 `main` 的功能说明。当前未发布变化以根目录 [`CHANGELOG.md`](../CHANGELOG.md) 的 `Unreleased` 为准。

## 文档事实源

出现冲突时按下列顺序核对：

| 主题 | 首要事实源 | 文档同步位置 |
|---|---|---|
| CLI 命令与参数 | `cmd/pairroom/main.go`、`cmd/pairroom/daemon.go` | [CLI_REFERENCE.md](CLI_REFERENCE.md) |
| Service/Room 行为 | `internal/service/`、`internal/room/` | [MULTI_ROOM_SERVICE.md](MULTI_ROOM_SERVICE.md)、[ARCHITECTURE.md](ARCHITECTURE.md) |
| HTTP、SSE 与浏览器认证 | `internal/service/management.go`、`internal/server/`、内嵌 assets | [MANAGEMENT_SHELL.md](MANAGEMENT_SHELL.md)、[PROTOCOL.md](PROTOCOL.md)、[SECURITY.md](../SECURITY.md) |
| Event 与 Store schema | `internal/model/`、`internal/store/` | [PROTOCOL.md](PROTOCOL.md)、[UPGRADING.md](UPGRADING.md) |
| 构建和测试命令 | `Makefile`、`scripts/`、CI workflow | [DEVELOPMENT.md](DEVELOPMENT.md)、[VALIDATION.md](VALIDATION.md) |
| 已发布事实 | Tag、`VERSION`、Release asset、对应 Changelog | Release Notes 与 Release Checklist |

“源码优先”不代表文档可以落后。行为变更的 PR 必须同时更新最接近用户入口的文档，并在 `CHANGELOG.md` 的 `Unreleased` 中说明。

## 文档维护约定

1. README 只保留最短可执行路径，不复制完整参数表；
2. 命令默认值、范围和必填项只在 CLI 参考中集中维护；
3. 概念解释不绑定具体 UI 文案；UI 操作细节放在 Management Shell/Room 文档；
4. 安全、隐私、恢复行为必须描述失败路径，不能只写成功路径；
5. Mock、单元测试、浏览器 smoke 与真实 Vendor E2E 必须明确区分；
6. 历史验证记录保持历史真实性，当前变化另写 `Unreleased`；
7. 相对链接、代码围栏、标题锚点和命令示例应在提交前检查。
