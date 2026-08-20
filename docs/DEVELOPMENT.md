# PairRoom 开发者指南

> [文档首页](README.md) · [架构](ARCHITECTURE.md) · [协议](PROTOCOL.md) · [贡献指南](../CONTRIBUTING.md)

本页说明如何修改 PairRoom 而不破坏原生 Harness、持久化、权限和恢复边界。具体 PR 规范仍以根目录 [`CONTRIBUTING.md`](../CONTRIBUTING.md) 为准。

## 开发环境

必需：

- Go 1.23+；
- Git；
- POSIX Make/Bash；
- Python 3（Agent/release contract）；
- `make check` 的 race gate 需要 `CGO_ENABLED=1`，并且 Go 支持的 C 编译器位于 `PATH`；Windows 可使用 MSYS2 UCRT64/MinGW64 的 GCC；
- Node.js（存在时执行内嵌 JavaScript 语法检查）。

完整 release/smoke 还需要 `curl`、标准 archive/checksum 工具和支持的目标平台工具链。真实 Claude/Codex 只用于可选 native smoke；普通测试不得要求供应商登录。

PairRoom 的 Go module 和浏览器运行时刻意不引入第三方依赖。

## 建议开发循环

```bash
git switch -c feature/short-name
make test
make vet
make check
make smoke
```

开发时运行单 Room Mock：

```bash
make demo
# 等价于 go run ./cmd/pairroom serve --repo . --mock --no-browser
```

运行多 Room Service：

```bash
go run ./cmd/pairroom service --mock
```

覆盖率：

```bash
make cover
```

## Make targets

| Target | 作用 |
|---|---|
| `make build` | 构建 `dist/pairroom`，写入版本/commit/build date |
| `make install` | 安装到 `GOBIN` 或 Go 默认 bin |
| `make test` | `go test -count=1 ./...` |
| `make race` | `go test -race -count=1 ./...` |
| `make vet` | `go vet ./...` |
| `make fmt` | 对全部 Go 文件执行 gofmt |
| `make cover` | 生成 `.coverage` 并输出函数覆盖率 |
| `make check` | test、race、vet、Agent/release contract、格式、JS、依赖、Git whitespace |
| `make smoke` | 完整 Mock 协作、持久化、备份/恢复/诊断 smoke |
| `make release` | 验证并生成多平台 release payload，不发布 tag |
| `make clean` | 删除构建产物 |

提交前至少执行 `make check`；改动用户流程、浏览器或恢复路径时再执行 `make smoke`。

Windows 上若用户级 Go 配置固定了 `CGO_ENABLED=0`，可只为当前验证进程启用 CGO 并加入现有 MSYS2 编译器路径，避免改写用于静态发布构建的全局偏好：

```powershell
$env:CGO_ENABLED='1'
$env:Path='C:\msys64\ucrt64\bin;' + $env:Path
& 'C:\Program Files\Git\bin\bash.exe' -lc 'make check'
```

## CI 多平台产物

普通 CI 在验证任务通过后，为当前触发 commit 构建以下四个 `CGO_ENABLED=0`、`-trimpath` 二进制：

| Workflow artifact | 内容 |
|---|---|
| `pairroom-linux-amd64` | `pairroom-v<VERSION>-linux-amd64` 与对应 `.sha256` |
| `pairroom-windows-amd64` | `pairroom-v<VERSION>-windows-amd64.exe` 与对应 `.sha256` |
| `pairroom-darwin-arm64` | `pairroom-v<VERSION>-darwin-arm64` 与对应 `.sha256` |
| `pairroom-darwin-amd64` | `pairroom-v<VERSION>-darwin-amd64` 与对应 `.sha256` |

构建元数据使用触发 commit 的完整 SHA 和 commit timestamp；每个矩阵任务先验证目标文件格式与 checksum，随后汇总任务重新下载全部产物，拒绝缺失或额外文件，并执行 Linux binary 的 `version --json` 校验。产物保留 14 天，也可通过 `workflow_dispatch` 手工生成。

Workflow artifact 是 CI 证据，不替代 tag 对应的 GitHub Release；正式发布仍由 `.github/workflows/release.yml` 和 `make release` 管理。GitHub artifact 传输会丢失 Unix executable bit，下载 Linux/macOS binary 后可能需要执行 `chmod +x`。

## 包结构

```text
cmd/pairroom/              CLI、service/serve 启动、daemon 管理
internal/agent/            Claude/Codex/Mock Adapter 与 Runtime probe
internal/archive/          verify、backup、restore、diagnostics
internal/attachment/       图片持久化、验证与安全解析
internal/bus/              事件广播
internal/config/           JSON 配置与兼容迁移
internal/daemon/           systemd/launchd/Task Scheduler 投射
internal/model/            领域类型、Event、状态与 schema
internal/prompt/           协作提示与协议追加
internal/room/             Room Engine、路由、生命周期和审批
internal/server/           Room REST/SSE、Session/CSRF 与静态 UI
internal/service/          Registry、Provisioning、Runtime Manager、Management API
internal/store/            append-only Event Store 与 replay
internal/version/          版本和构建元数据
internal/workspace/        Driver/Reviewer Workspace materialization
```

详细依赖与数据流见 [`ARCHITECTURE.md`](ARCHITECTURE.md)。

## 不可破坏的系统不变量

### 原生 Harness 边界

- 不用通用模型 API 替代官方 Claude Code/Codex；
- 不重写供应商 Agent loop、工具执行、上下文或账号系统；
- 不解析 ANSI/TUI 文本作为控制事实；
- 使用官方结构化协议，并在必需能力缺失时明确失败。

### 持久化与恢复

- 领域事件先写入并同步，再发布给 SSE；
- Room Event Log 是 Room 事实源；Registry 是可重建 checkpoint；
- 不静默跳过中间损坏；
- future schema 必须拒绝；
- retry 创建新记录，不改写历史；
- restart 必须收口 orphaned processing 和 approval。

### 身份与并发

- 一个 Vendor Binding 在 Service 内只能归属一个 Room；
- deferred Binding 只有在首个输入被 Runtime 接受并完成 durable materialization 后才成立；
- 同一 Room Event Log 不允许 Engine 与控制面并发写入；
- 容量回收、切换页面和 idle timeout 不得中断活动 Turn；
- cleanup 不确定时占用容量并 fail closed，不能假装已释放。

### 安全

- listener 校验必须在打开 repository/data root 前完成；
- 只允许数字 loopback；
- Management 与 Room bootstrap Token 均不进入 Web Storage；两者的 Session Cookie、CSRF 和作用域不得混用；
- query token 不授权 API；
- 未知高权限请求 fail closed；
- Reviewer UI 状态不能领先于 Adapter/Workspace 实际策略；
- 附件必须验证内容签名、大小、维度、普通文件、symlink 边界和 SHA-256；
- 公共 Event/API/export 不泄漏附件本机绝对路径。

### 隐私与诚实验证

- 无 PairRoom telemetry、hosted credential 或隐藏云依赖；
- Mock/fixture 测试不能声称真实 Vendor E2E；
- diagnostics 默认不含消息正文和附件 bytes；
- Existing Binding 不导入绑定前 Vendor Transcript。

## 变更影响矩阵

| 改动 | 至少检查 |
|---|---|
| CLI 参数/默认值 | CLI tests、`CLI_REFERENCE.md`、README 示例、Changelog |
| Event/model/schema | migration/replay、future-schema rejection、`PROTOCOL.md`、`UPGRADING.md` |
| Room 状态机/路由 | focused unit tests、restart settlement、Mock smoke |
| Service Registry/Binding | uniqueness、atomic provisioning、rebuild、crash window、Multi-Room docs |
| Runtime capacity | busy/queued/failed-retained、shutdown、Management snapshot/UI |
| Browser auth/API | Host/origin/CSRF/session/Bearer tests、Security/Privacy docs |
| Attachment | format/size/hash/symlink/export/backup tests、Rich Conversation docs |
| Reviewer workspace | dirty/untracked/binary/symlink/rollback tests、Security caveat |
| daemon | 三平台定义、PATH/env、graceful stop、rotation、Operations/CLI docs |
| archive | traversal/link/duplicate/size/hash/atomic replacement、Operations/Upgrade docs |
| UI | keyboard/mobile/empty/error/loading、JS syntax、browser smoke |

## 测试层次

### 单元与包级测试

状态转换、协议映射、错误路径、并发和安全边界应有 focused tests。测试名称应描述行为和失败条件，而不是只覆盖函数。

### Race 与静态检查

`make check` 的 race 与 vet 是提交门禁。引入共享 map、waiter、队列、SSE hub 或 shutdown path 时，不能只依赖普通测试。

### Mock smoke

`make smoke` 验证 PairRoom 可控的完整路径：三方消息、Reviewer snapshot、Turn 摘要、图片、分页、关闭、verify、backup、restore 和 diagnostics。

### 浏览器 smoke

Management Shell 使用真实静态 assets 与模拟 API 数据检查主要路由、Dialog、移动布局、console error 和水平溢出。Room UI 的变更同样应覆盖安全渲染、长消息和响应式边界。

### Native smoke

只有在官方 CLI 已安装、登录并实际完成 Turn 时才能标记为 native E2E。报告应记录：

- PairRoom build metadata；
- Claude/Codex 当前版本；
- 目标仓库是否公开/合成；
- 实际验证的协议能力；
- 未验证或降级的部分。

## 文档维护

行为变更应更新最接近事实源的一份文档，而不是把所有说明塞进 README。

- 新用户路径：`README.md` / `GETTING_STARTED.md`；
- 术语/心智模型：`CONCEPTS.md`；
- 命令：`CLI_REFERENCE.md`；
- Service/Room 行为：`MULTI_ROOM_SERVICE.md` / `ARCHITECTURE.md`；
- API/Event：`PROTOCOL.md` / `MANAGEMENT_SHELL.md`；
- 运维与故障：`OPERATIONS.md` / `TROUBLESHOOTING.md`；
- 安全/隐私：`SECURITY.md` / `PRIVACY.md`；
- 当前未发布变化：`CHANGELOG.md` 的 `Unreleased`。

不要把当前 `main` 的功能倒写进历史 Release Notes/Validation Record。历史文档应保持当时真实性。

提交前运行：

```bash
git diff --check
```

并检查 Markdown 相对链接、代码围栏、重复标题锚点、命令换行和 Windows/POSIX 示例。

## PR 说明

PR 至少回答：

1. 用户可见行为是什么；
2. durable state、Event、API 或 Vendor protocol 是否变化；
3. 失败和回滚行为是什么；
4. 安全、隐私和 Workspace 边界是否变化；
5. 运行了哪些测试；
6. 是否真正运行了当前官方 Claude Code/Codex；
7. 更新了哪些文档和 Changelog。

对于事件、持久化、消息生命周期、角色/Workspace、审批、认证或 archive 变更，必须同时说明 migration/recovery。
