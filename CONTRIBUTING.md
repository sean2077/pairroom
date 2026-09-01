# 为 PairRoom 贡献代码

> [架构](docs/ARCHITECTURE.md) · [协议](docs/PROTOCOL.md) · [Runtime 兼容](docs/RUNTIME_COMPATIBILITY.md) · [安全策略](SECURITY.md)

PairRoom 的边界有意保持狭窄：协调官方 Claude Code 与 Codex，而不是替换任一 Harness。引入通用 Agent loop、终端文本解析、托管 credential、隐藏云依赖或默认公网 listener 的改动通常不符合项目方向。

## 开发环境

必需：

- Go 1.23+、Git、POSIX Make/Bash 与 Python 3；
- `make check` 的 race gate 需要 `CGO_ENABLED=1` 和 Go 支持的 C 编译器；
- smoke/release 需要 `curl`、标准 archive/checksum 工具；
- Node.js 存在时会执行内嵌 JavaScript syntax / UI contract check。

可选：

- 当前官方 Claude Code 与 Codex，用于真实 native smoke；
- Chromium/Playwright 环境，用于额外 visual smoke。

Go module 和浏览器运行时代码有意不引入第三方依赖。

Windows 若全局固定 `CGO_ENABLED=0`，可只为当前验证进程启用 CGO：

```powershell
$env:CGO_ENABLED='1'
$env:Path='C:\msys64\ucrt64\bin;' + $env:Path
& 'C:\Program Files\Git\bin\bash.exe' -lc 'make check'
```

## 开发循环

```bash
git switch -c feature/short-name
make test
make check
make smoke
```

常用目标：

| Target | 作用 |
|---|---|
| `make build` | 构建 `dist/pairroom` 并写入版本元数据 |
| `make install` | 安装到 `GOBIN` 或 Go 默认 bin |
| `make test` / `make race` / `make vet` | 单元、Race 与静态检查 |
| `make docs-check` | 校验文档清单、链接和源码覆盖 |
| `make check` | test、race、vet、docs、Agent/release contract、格式、JS、依赖和 whitespace |
| `make demo` | 单 Room Mock，不自动打开浏览器 |
| `make smoke` | 完整 Mock 协作、持久化、备份/恢复/诊断 smoke |
| `make release` | 验证并生成多平台发布包，不创建 tag |

多 Room 手动开发：

```bash
go run ./cmd/pairroom service --mock
```

## 不可破坏的系统不变量

1. PairRoom 不拥有 Vendor reasoning/tool loop；
2. 一个 Room Event Store 同时只有一个写者；
3. durable event 先落盘并同步，再发布给观察者；
4. Delivery 与 Processing 不合并；
5. retry 创建新消息，不改写历史；
6. 用户新指令优先于 stale auto-handoff；
7. 一个 `(agent, vendor_session_id)` 在 Service 内只属于一个 Room；
8. unsafe provisioning、workspace、restore 和高权限 request fail closed；
9. active native Turn 不因容量回收、页面切换或 idle timeout 被中断；
10. generic runtime diagnostic error 不是 Turn terminal boundary；
11. Management 与 Room 的 Token、Session 和 CSRF 作用域彼此隔离；
12. listener 仅接受数字 loopback；
13. Mock/fixture/build success 不冒充真实 Vendor E2E。

完整状态权威见 [Architecture](docs/ARCHITECTURE.md) 与 [Storage](docs/STORAGE.md)。

## 变更影响矩阵

| 改动 | 至少验证 / 更新 |
|---|---|
| CLI、默认值、配置 | parsing/range/error tests；`CLI_REFERENCE.md`、`CONFIGURATION.md` |
| Event/model/schema | replay、future-schema rejection、verify/backup/restore；`PROTOCOL.md`、`STORAGE.md`、`UPGRADING.md` |
| Room scheduler/routing | focused unit + race + Mock smoke；terminal/cancel/restart 边界 |
| Registry/Binding | atomic provisioning、duplicate ownership、checkpoint/rebuild/crash tests |
| Runtime capacity/lifecycle | busy/queued/failed-retained、shutdown、archive/suspend tests |
| Adapter | fixture contract + 当前 Vendor smoke；记录实际版本和未验证部分 |
| Role/workspace | dirty/untracked/binary/symlink/rollback 与只读 policy tests |
| Auth/HTTP | Host/origin/session/CSRF/Bearer/rate-limit negative tests；`API_REFERENCE.md`、`SECURITY.md` |
| Attachment | signature/size/dimension/hash/path/symlink/export/backup tests |
| Management/Room UI | asset contract、keyboard、focus、mobile overflow、error/loading states |
| daemon | Linux/macOS/Windows definition、argument preservation、graceful stop |
| docs only | `make docs-check`、命令/route/config 字段对源码、`git diff --check` |

Mock smoke 证明 PairRoom 自己的控制面；只有官方 CLI 已安装、登录并实际完成 Turn 时才能声称 native E2E。

## 持久化或协议变更

修改 Event、Store schema、Room/Binding lifecycle、Delivery/Processing、Turn summary、附件 manifest、Registry rebuild 或 archive format 时，PR 必须回答：

- 旧数据如何 replay；
- 缺失字段的默认值；
- future/unsupported state 如何拒绝；
- 中途失败是否留下半提交；
- verify/diagnostics 如何发现问题；
- rollback 使用哪个备份边界。

不要通过静默跳过事件、降低 schema 或重写历史“兼容”。

## Service 与并发改动

说明控制面与 Room Runtime 的 ownership、per-Room lock、Event append 与 checkpoint 顺序、provisioning staging、busy Turn 非抢占、cleanup uncertain 占用容量、shutdown 排空和 stale lock 恢复。新增“强制”按钮或 API 必须证明不会绕过这些边界。

## Adapter 改动

- 仅使用公开、结构化的官方接口；
- Ready 前完成 initialize/能力检查；
- Session/Thread 必须精确 resume，不能静默创建替代；
- correlation 不能依赖可变 UI 文本；
- diagnostic error 与 terminal boundary 必须分开；
- interrupt/exit 必须收口 waiter、approval 和 Processing；
- Reviewer native policy 必须在 Room role 持久化前成功应用。

PR 中列出实际验证的 Claude/Codex 版本和场景；未运行真实 CLI 时明确写 `not run`。

## Web 与认证改动

Management 与 Room 使用相同安全模式，但凭据和作用域是两套独立合同。任何 Web PR 都应覆盖 query token 拒绝、HttpOnly Session、CSRF、Origin/Host、SSE/attachment auth、刷新/断线、CSP、安全响应头、keyboard/focus/reduced-motion 和移动布局。不要把长期 Token 放入 Web Storage。

## 文档维护

行为变化只更新最接近事实源的文档：

| 变化 | 权威文档 |
|---|---|
| 用户操作 | `docs/USER_GUIDE.md` / `docs/GETTING_STARTED.md` |
| 术语和行为语义 | `docs/CONCEPTS.md` |
| CLI / config / API | 对应 Reference |
| 组件与并发 | `docs/ARCHITECTURE.md` |
| Event、数据与恢复 | `docs/STORAGE.md` / `docs/PROTOCOL.md` |
| Vendor wire | `docs/RUNTIME_COMPATIBILITY.md` |
| 运维与排障 | `docs/OPERATIONS.md` / `docs/TROUBLESHOOTING.md` |
| 用户可见变化 | `CHANGELOG.md` 的 `Unreleased` |

历史 release 与 validation evidence 留在 Git 历史、GitHub Releases 和 CI artifacts；不要复制冻结快照到现行文档树。

## Pull Request

PR 至少说明：

1. 用户可见行为和产品边界；
2. durable state、协议或 API 变化；
3. 并发、失败和回滚行为；
4. 安全、隐私、Workspace 与多 Room 影响；
5. 测试命令、结果及其证明范围；
6. 是否真实运行当前 Claude Code/Codex；
7. 文档与 Changelog 更新。

提交应小而可审查，不包含 `dist/`、本地数据根、Room Event Log、Token、真实私有截图或 Vendor Session。运行：

```bash
git diff --check
make check
```

版本发布流程见 [RELEASING](docs/RELEASING.md)。安全问题不要公开提交，按 [SECURITY.md](SECURITY.md) 私下报告。
