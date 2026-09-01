# PairRoom Runtime 跟随与兼容策略

PairRoom 不是 Claude Code/Codex 的替代实现。兼容目标是：在不解析 TUI 文本、不复制 Vendor loop 的前提下，使用官方结构化协议完成启动、恢复、输入、工具/审批观察、取消与可靠 Turn 结束。

## 原则

1. **公开结构化接口优先**：JSON/JSON-RPC/stream event，而不是 ANSI/TUI scraping；
2. **必需能力 fail closed**：缺失 initialize、resume、input、completion 或 interrupt 时明确失败；
3. **可选能力显式降级**：不支持的 telemetry/usage/image 能力要暴露 warning；
4. **精确恢复**：Existing Binding 必须恢复指定 Session/Thread，不能静默新建；
5. **诊断不等于终止**：generic error/warning 不是自动 terminal boundary；
6. **真实验证诚实标注**：Mock、fixture、doctor、build 与 native E2E 分层报告。

## 能力层次

| 层次 | 证明 |
|---|---|
| 编译 / cross-build | PairRoom 源码与目标平台可构建 |
| Unit / fixture | 已知 Vendor event/request 样本能正确映射 |
| Mock smoke | PairRoom 控制面、Store、scheduler、UI 与恢复路径 |
| `doctor` | 本机 executable 存在且必需协议面可探测 |
| Native smoke | 当前 CLI、账号、网络、模型与真实 Turn 可用 |

低层通过不能替代高层。

## Claude Code

PairRoom 使用 Claude Code 的结构化输入/输出与 control request 边界。必需能力包括：

- 可启动官方 CLI；
- 新 Session 与指定 Session resume；
- 结构化 user input / result / error / tool activity；
- control request 与 permission resolution；
- interrupt / process exit 收口；
- Session ID 可关联到 Room Binding。

### Session resume

Existing Binding 要求 resume 返回同一 Session identity。若 Vendor 拒绝或返回其他 identity，Room Runtime 启动失败，不创建替代 Session。

### Reviewer

Reviewer mode 需要 Vendor 侧只读/权限 policy 可应用。若 policy 失败，PairRoom 不先把角色记录为 Reviewer。只读 policy 与 snapshot 是 defense in depth，不能等同于容器隔离。

### 图片

用户/Room 图片投影为 Claude 支持的多模态 content。若当前 Claude 版本缺失所需 image input，PairRoom 应显式报告，而不是把本机路径作为普通文本悄悄发送。

## Codex

PairRoom 使用 `codex app-server` JSON-RPC。必需能力包括：

- `initialize` / `initialized`；
- `thread/start` 与 `thread/resume`；
- `turn/start`、可用时 `turn/steer`；
- `turn/started`、`turn/completed` 与 item/approval notifications；
- interrupt / process exit 收口；
- Thread / Turn / client message correlation。

### Generic error

Codex 可以在 Turn 中途发 `error`，稍后仍发 `turn/completed`。因此：

```text
error notification -> diagnostic RuntimeError only
turn/completed      -> reliable native terminal boundary
confirmed exit      -> synthesized process-exit boundary
```

不得在 generic error 上释放 owner 或启动 peer。

### Multiple steer

一个 active Codex Turn 可以接收多个 PairRoom input。每个 input 保留独立 Processing，Turn completion 再统一提供可靠执行边界。若 steer 被当前 review/compaction 状态拒绝，输入进入下一 Turn 队列而不是丢失。

### Sandbox / Provider wire

Codex sandbox、approval、model provider 和 HTTP header 使用当前官方 CLI 支持的结构化 config override。PairRoom 不依赖未声明的 shell profile 或 TUI 文本。Vendor 改名或 wire schema 变化时，应更新 fixture、doctor 和文档。

### 图片

Room 图片投影为 Codex `localImage`。只有经 Attachment Store resolve 的受控本机文件可进入 wire；任意用户路径不能直接透传。

## New 与 Existing Binding

| 模式 | 兼容要求 |
|---|---|
| `existing` | 创建/启动前严格验证指定 ID，可恢复同一 Vendor context |
| `new` | Room 先持久化 pending Binding；首个 accepted native input 后 materialize 实际 ID |

Codex 需要再区分“Thread ID 已返回”和“Thread rollout 已持久化”。`thread/start` 可以先返回一个仅由当前 app-server 进程认识的 ID；在首个 Turn 被接受前退出时，磁盘上可能还没有对应 rollout。此时 PairRoom 不把该 ID materialize，也不在下次 activation 做 strict resume，而是清除未 engaged 候选 ID并重新 `thread/start`。一旦首个 Turn 已接受，或 Room 原本就是 existing/materialized Binding，则继续要求精确恢复同一 Thread，失败时不得静默替换。

New Binding 的外部 ID 产生早于 PairRoom durable ownership，是不可避免的外部副作用窗口；实现必须在 materialization 失败时 fail closed 并提供诊断，不能让两个 Room 争用同一 ID。

## `pairroom doctor`

Doctor 用于快速发现：

- executable / version 不可用；
- 必需启动参数或协议面缺失；
- 当前配置选中的 command 不可执行；
- 明显的 initialize / capability incompatibility。

Doctor 不验证：账号登录、供应商网络、模型权限、长 Turn、真实工具、审批 UI、Existing context 内容或端到端交棒。

## Vendor 升级后的 smoke

在非关键仓库至少验证：

1. `doctor --json`；
2. Claude new + existing 只读 Turn；
3. Codex new + existing 只读 Turn；
4. steering 与 terminal boundary；
5. generic error 不提前交棒；
6. interrupt / restart / process exit；
7. Reviewer policy 与 snapshot；
8. 审批和图片（使用到时）；
9. Provider endpoint/model（配置到时）。

记录 PairRoom build metadata、Vendor 版本、实际场景和未验证能力。

## 不兼容时

- 保留失败 event 顺序和 diagnostics；
- 在 Changelog 标出受影响 Vendor 版本/能力；
- 优先更新结构化 Adapter 与 fixture；
- 缺失必需能力时拒绝 Ready；
- 不通过增加 sleep、延长 stall warning、吞 error 或解析 TUI 伪装兼容；
- 必要时建议固定已验证 Vendor 版本，直到兼容修复合入。

提交 Adapter 修复时同时更新 [Protocol](PROTOCOL.md)、[Architecture](ARCHITECTURE.md)、测试与真实验证说明。
