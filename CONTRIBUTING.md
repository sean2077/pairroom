# 为 PairRoom 贡献代码

> [开发者指南](docs/DEVELOPMENT.md) · [架构](docs/ARCHITECTURE.md) · [协议](docs/PROTOCOL.md) · [安全策略](SECURITY.md) · [产品计划](docs/PRODUCT_PLAN.md)

PairRoom 的边界有意保持狭窄：协调官方 Claude Code 与 Codex，而不是替换任一 Harness。引入模型代理、通用 Agent loop、终端输出解析、托管 credential、隐藏云依赖或默认公网 listener 的改动通常不符合项目方向。

## 1. 开发环境

必需：

- Go 1.23+；
- Git；
- Bash、curl、Python 3 与标准 archive 工具（smoke/release）；
- Node.js（存在时执行内嵌 JavaScript syntax check）。

可选：

- 当前官方 Claude Code 与 Codex，用于真实 native smoke；
- Chromium/Playwright 环境，用于 Management visual smoke。

Go module 和浏览器运行时代码有意不引入第三方依赖。

## 2. 先理解系统不变量

任何改动都必须保持：

1. PairRoom 不拥有 Vendor reasoning/tool loop；
2. 一个 Room Event Store 只有一个写者；
3. durable event 先落盘再发布；
4. Delivery 与 Processing 不合并；
5. 重试创建新消息，不重写历史；
6. 用户新指令优先于 stale auto-handoff；
7. Binding Identity 在 Service 内全局唯一；
8. unsafe provisioning/workspace/restore fail closed；
9. active Turn 不因容量回收被 interrupt；
10. Management 与 Room 的 Token、browser session 和 CSRF 各自隔离，不跨控制面复用；
11. listener 仅 numeric loopback；
12. Mock/fixture 不冒充真实 Vendor E2E。

更完整说明见 [开发者指南](docs/DEVELOPMENT.md)。

## 3. 本地工作流

```bash
git switch -c feature/short-name
make check
make smoke
```

常用目标：

```bash
make build
make test
make race
make vet
make cover
make demo       # single Room Mock, no browser
make run        # single Room native
```

多 Room 手动开发：

```bash
go run ./cmd/pairroom service --mock
```

`make check` 包括 unit、race、vet、Agent/release contract、gofmt cleanliness、可用时的 JS syntax、无第三方 Go module 和 `git diff --check`。

## 4. 按改动类型验证

| 改动 | 至少需要 |
|---|---|
| Engine/message/routing | focused unit + Mock collaboration/recovery smoke |
| Event/model/schema | replay/migration/verify/backup/restore test + upgrade说明 |
| Service Registry/Binding | atomic failure、duplicate ownership、restart rebuild test |
| Runtime Manager | capacity、FIFO、idle、busy conflict、cleanup uncertain test |
| Adapter | fixture contract + current real Vendor smoke；说明版本 |
| Role/workspace | unsafe symlink、dirty/untracked、rollback、read-only metadata test |
| Auth/HTTP | Host/origin/session/CSRF/Bearer/rate-limit negative tests |
| Attachment | signature/size/dimension/hash/path/symlink negative tests |
| Management UI | asset contract + desktop/mobile visual smoke + keyboard/accessibility |
| daemon | Linux/macOS/Windows definition、argument preservation、graceful stop test |
| CLI | parsing/default/range/error test + `docs/CLI_REFERENCE.md` |
| docs only | relative links、commands、facts against current source、`git diff --check` |

Mock tests证明 PairRoom 自己的状态机；只有真实 CLI smoke 才能证明当前 Vendor 集成。

## 5. 变更持久化协议前

修改以下任一内容时，PR 必须包含迁移/恢复说明：

- Event type 或字段；
- Store schema；
- Project/Room/Binding lifecycle；
- Message Delivery/Processing；
- durable Turn summary；
- attachment manifest；
- Registry rebuild；
- archive/backup/restore format。

说明至少回答：

- 旧数据如何 replay；
- 缺失字段默认值；
- 新版本写入后旧版本如何拒绝；
- 中途失败是否留下半提交；
- verify/diagnostics 能否发现问题；
- rollback 使用什么备份边界。

不要通过静默跳过事件、降低 schema 或重写历史“兼容”。

## 6. Service 与并发改动

Service 相关 PR 应明确：

- 控制面与 Room Runtime 的 ownership；
- per-Room mutex/单写者边界；
- Event append 与 Registry checkpoint 顺序；
- provisioning 的 staging/atomic publish；
- busy Runtime 是否会被 interrupt；
- cleanup uncertain 是否仍占 capacity；
- shutdown 时 in-flight handler/Turn 如何排空；
- stale lock 是否需要显式人工授权。

新增“强制”按钮或 API 必须证明不会绕过这些边界。

## 7. Adapter 改动

- 仅使用公开、结构化的官方接口；
- 未知高权限 request fail closed；
- protocol initialize 在 Ready 前完成；
- Session/Thread resume 必须精确，不能静默新建替代；
- correlation 不能依赖可变 UI 文本；
- interrupt/exit 必须收口 waiter、approval 和 Processing；
- Reviewer 原生 policy 必须在 Room role 持久化前成功应用。

PR 中列出实际验证的 Claude/Codex 版本和场景。未运行真实 CLI 时明确写 “not run”。

## 8. Web 与认证改动

Management 与 Room 使用相同的安全模式，但凭据和作用域是两套独立契约：

- Management：fragment Bearer bootstrap → Service-scoped HttpOnly Session + in-memory CSRF；
- Room：fragment Bearer bootstrap → Room-scoped HttpOnly Session + in-memory CSRF（启用 Token 时）；
- 非浏览器 API 客户端可直接使用各自的 Bearer Token。

任何 Web PR 都应覆盖：

- query token 拒绝；
- Web Storage 不保存长期 secret；
- Host/same-origin/CSRF；
- SSE/attachment auth；
- 页面刷新/断线行为；
- CSP/security headers；
- keyboard、focus、reduced motion、mobile overflow。

不要为了“记住登录”把 Management 或 Room Token 放入 Web Storage；浏览器持久性只能来自服务端签发的 HttpOnly Session Cookie。

## 9. 文档要求

行为变化必须同步最接近用户的文档：

| 变化 | 文档 |
|---|---|
| CLI/默认值/范围 | `docs/CLI_REFERENCE.md`、必要时 README/Quick Start |
| Project/Room/Runtime | `docs/MULTI_ROOM_SERVICE.md`、`docs/CONCEPTS.md` |
| Management UI/API | `docs/MANAGEMENT_SHELL.md` |
| Store/Adapter/架构 | `docs/ARCHITECTURE.md`、`docs/PROTOCOL.md` |
| 安全/隐私 | `SECURITY.md`、`docs/PRIVACY.md` |
| 运维/daemon/data | `docs/OPERATIONS.md`、`docs/TROUBLESHOOTING.md` |
| schema/migration | `docs/UPGRADING.md` |
| 用户可见变化 | `CHANGELOG.md` 的 `Unreleased` |

历史 Release Notes/Validation Record 保持历史真实性，不回填当前 `main` 行为。

## 10. Pull Request 说明

PR 至少说明：

1. 用户可见行为；
2. 为什么符合产品边界；
3. durable state/protocol/schema 变化；
4. 并发、失败和回滚行为；
5. 安全与隐私影响；
6. 多 Project/Room/Binding/capacity 影响；
7. 测试命令与结果；
8. 是否真实运行当前 Claude Code/Codex；
9. 文档与 Changelog 更新。

避免只写“tests pass”。列出哪些测试证明了什么，以及未验证的边界。

## 11. 提交与 Git 历史

- 提交应小而可审查，每个提交保持可构建/可测试；
- 不提交 `dist/`、本地数据根、Room Event Log、Token、真实截图或 Vendor Session；
- 运行 `git diff --check`；
- 不重写已发布历史；
- v0.1–v0.3 保留提交来自 release snapshot 重建，v0.4 起为原生保留历史；重排 milestone 前阅读 `HISTORY_PROVENANCE.md`。

建议 commit message：

```text
<type>: <imperative summary>
```

例如 `docs: align service and room authentication model`。

## 12. Release 工作

`docs/RELEASE_CHECKLIST.md` 是发布门禁，`CHANGELOG.md` 是 release-note authority。每个 release 需要唯一、非空、与 `VERSION`/tag 一致的章节。

```bash
make release
```

它执行 release contract、unit/race/vet/static checks、完整 Mock collaboration/recovery smoke，并生成多平台 binaries、source archive、SBOM/provenance、checksums 与 version evidence；不自动创建或发布 tag。

Tag workflow 会重新验证 tag/version/changelog identity，并校验发布资产。不要在未完成真实 smoke 和回滚证据时只因为 CI 绿色就发布。

## 13. 安全问题

不要在公开 PR/Issue 中提交真实 Token、私有代码、Event Log、附件或可利用 payload。安全问题按 [SECURITY.md](SECURITY.md) 私下报告。
