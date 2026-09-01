# PairRoom 支持范围

> [快速上手](docs/GETTING_STARTED.md) · [排障](docs/TROUBLESHOOTING.md) · [运维](docs/OPERATIONS.md) · [安全](SECURITY.md)

## 提交 Issue 前

收集：

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/project --json
pairroom daemon status       # 使用 daemon 时
pairroom diagnostics --data-dir /absolute/path/to/room --output diagnostics.tar.gz
```

再用同一版本执行一次 Mock 对照：

```bash
pairroom service --mock --no-browser
# 或 pairroom serve --repo /absolute/path/to/project --mock --no-browser
```

Mock 正常而 native 失败，通常指向 Vendor CLI、账号、网络、模型/Provider 或协议兼容；Mock 同样失败，通常指向 PairRoom 控制面、数据或环境。

Diagnostics 默认省略 transcript 正文和附件 bytes，但仍可能包含版本、事件头、错误和本机路径。分享前必须人工检查。

## Bug 报告应包含

- PairRoom 版本、commit、OS/architecture；
- `service`、`daemon` 或 `serve` 运行方式及脱敏参数；
- Claude Code / Codex 版本，以及是否独立登录成功；
- Project/Room 操作步骤和期望/实际结果；
- Mock 是否复现；
- 最小相关日志、HTTP 状态或 diagnostics；
- 是否涉及 Existing Binding、Reviewer、附件、归档、删除、恢复或 Provider profile；
- 是否可在最小公开仓库复现。

不要只提交截图；同时附上可复制的错误文本和发生时间。

## 不要公开提交

- Management/Room Token、Cookie、CSRF；
- API key、Provider secret 或带 credential 的 URL；
- 私有源码、完整 Event Log、Transcript 或附件；
- Claude Session ID、Codex Thread ID；
- 可利用的安全 payload。

安全漏洞按 [SECURITY.md](SECURITY.md) 的私有报告方式处理。

## 问题分类

| 现象 | 首选资料 |
|---|---|
| Service/daemon 无法启动 | status、logs、listen/data-root、lock 状态 |
| Project/Room lifecycle 错误 | Project path、Room ID、lifecycle、Binding 状态 |
| Room 数据或恢复失败 | `verify`、backup/restore 命令和 diagnostics |
| Browser 401/CSRF/SSE | 页面类型、URL、HTTP status、是否经过代理 |
| Vendor Runtime 卡住/协议失败 | Vendor 版本、独立 CLI 结果、runtime event 顺序 |
| Workflow/审批问题 | 阶段序列、计划 revision、approval 与 Turn ID |

先按 [Troubleshooting](docs/TROUBLESHOOTING.md) 的症状路径排查。

## 兼容策略

PairRoom 跟随官方 Claude Code 与 Codex 的公开结构化协议。Vendor 发布后可能发生 wire/schema 变化；项目不会通过 ANSI/TUI 文本解析掩盖不兼容。`doctor` 只验证本机 executable 与必要协议面，真实账号/网络/模型行为仍需 native smoke。详见 [Runtime Compatibility](docs/RUNTIME_COMPATIBILITY.md)。

## 支持边界

当前稳定边界：

```text
one human + one Claude + one Codex per Room
local Git worktrees
numeric loopback listeners
official native CLIs
one active native Turn owner
```

明确不提供或不保证：

- 直接 LAN/公网 listener、内建 TLS 或远程 worker；
- 一个 Room 多个同类型 Agent；
- 删除 Git worktree、Vendor Session/Thread 或外部导入目录；
- Runtime policy 热修改；
- Reviewer 容器级安全隔离；
- 隐藏 telemetry、托管 credential 或通用 Vendor 插件 API；
- 旧 routing mode 的兼容迁移。

功能请求请说明真实用户问题、为什么现有 Workflow/CLI/API 不足、对 native Harness 的影响以及最小可验证验收标准。先阅读 [文档地图](docs/README.md) 与 [架构](docs/ARCHITECTURE.md)。
