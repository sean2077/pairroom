# PairRoom 支持范围

> [快速上手](docs/GETTING_STARTED.md) · [排障手册](docs/TROUBLESHOOTING.md) · [安全策略](SECURITY.md) · [运维手册](docs/OPERATIONS.md)

PairRoom 是本地优先开源项目，支持通过 GitHub 仓库按 best-effort 方式提供。提交问题前先判断它属于环境、Service/daemon、Room 数据、浏览器 UI 还是 Vendor Runtime。

## 1. 提交 Issue 前

### 1.1 版本与环境

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/repository --json
```

记录操作系统、架构、安装方式、PairRoom binary path，以及当前官方 Claude Code/Codex 版本。

### 1.2 Service / daemon

```bash
pairroom daemon status
pairroom daemon logs -n 200
```

前台模式保留启动输出，但分享前删除完整 Management/Room URL、Token 和敏感路径。

在 Management Shell 导出 Service diagnostics，可用于 Project/Room/Runtime/capacity/Registry 问题。该文件不替代 Room diagnostics。

### 1.3 Room 数据

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom diagnostics \
  --data-dir /absolute/path/to/room \
  --output pairroom-diagnostics.tar.gz
```

Diagnostics 设计为省略 transcript 正文和附件 bytes，但仍可能包含版本、结构化事件头、错误和环境路径。阅读 [SECURITY.md](SECURITY.md) 并人工检查归档后再分享。

### 1.4 Mock 对照

尝试在最小测试仓库复现：

```bash
pairroom service --mock
# 或
pairroom serve --repo /absolute/path/to/test-repo --mock
```

Mock 有助于区分 PairRoom 控制面/Room 状态问题与 Vendor CLI 问题，但 Mock 成功不能证明真实 Vendor 一定正常。

## 2. Bug 报告应包含

- 操作系统与架构；
- PairRoom version/commit/build date；
- 实际 binary path 与运行入口；
- Service/Room 的最小相关参数，Token 脱敏；
- 当前官方 Claude Code/Codex 版本；
- Project/Room/Runtime phase 与可见 Delivery/Processing 状态；
- 精确操作步骤、预期结果与实际结果；
- 是否在 `--mock` 复现；
- 是否在最小非敏感 Git repository 复现；
- `verify`/`doctor` 的脱敏结果；
- 发生问题前最近一次升级、daemon reinstall、Binding 或数据迁移。

对 UI 问题补充浏览器版本、viewport、console error 和可公开的最小截图。

## 3. 不要公开提交

- API/Management/Room Token；
- Cookie、CSRF 或完整启动 URL；
- 私有 prompt、Agent answer 或 Event Log；
- 源代码、Diff、命令输出或审批 payload；
- 客户/产品截图与附件；
- Vendor credential 或组织信息；
- 未公开安全漏洞的可直接利用细节。

安全漏洞按 [SECURITY.md](SECURITY.md) 的私有报告方式处理。

## 4. 问题分类

### Service / daemon

典型症状：无法安装/启动、stale lock、日志轮转、不同 CWD 打开不同数据、Runtime capacity/queue、Registry unhealthy。

### Project / Room lifecycle

典型症状：Project unavailable、重复 canonical root、Room provisioning、Existing Binding 冲突、Legacy pending、archive/restore。

### Room data

典型症状：Event sequence、future schema、attachment hash、backup/restore、restart 后状态不收口。

### Browser

典型症状：Management 刷新 401、Room session/CSRF、SSE 断连、历史分页、图片预览、mobile overflow。

### Vendor Runtime

典型症状：`doctor` probe、Claude control initialize、Codex app-server request、Session/Thread resume、permission/sandbox、真实 Turn 卡住。

分类越准确，越容易避免把 Vendor service outage 当作 PairRoom Store bug，或把标签页 Token 丢失当作 daemon 故障。

## 5. 兼容策略

PairRoom 跟随当前稳定公开 Claude Code/Codex 接口，不为过时 CLI 维护永久兼容矩阵。更新任一 Vendor CLI 后：

```bash
pairroom doctor --repo /absolute/path/to/safe-test-repo
```

并在非关键仓库完成真实 smoke。详细策略见 [Runtime 兼容说明](docs/RUNTIME_COMPATIBILITY.md)。

## 6. 当前支持边界

当前支持：

```text
one local Service per data root
multiple canonical Git Projects
multiple durable Rooms
bounded active Room Runtimes
one human + one Claude + one Codex per Room
```

不在当前支持契约内：

- 多用户托管、团队 RBAC、云同步；
- 直接 LAN/公网 listener 或内建 TLS；
- 远程 worker；
- 一个 Room 多个同类型 Agent；
- Project removal / permanent Room deletion；
- Runtime policy 热修改；
- Reviewer 容器级安全保证；
- 额外 Vendor 的稳定插件 API。

## 7. 功能请求

功能请求应说明：

- 具体用户工作流，不只是一句“支持某工具”；
- 为什么现有 Service/Room/Driver/Reviewer 模型不足；
- 事实源、失败恢复与迁移需求；
- 安全、隐私和多 Room 身份/容量影响；
- 是否会削弱官方 Harness 原生能力；
- 最小可验证验收标准。

先阅读 [产品计划](docs/PRODUCT_PLAN.md) 和 [架构](docs/ARCHITECTURE.md)，避免请求已明确的非目标。
