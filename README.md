# PairRoom

PairRoom 是一个面向 **Claude Code + Codex** 的本地协作控制面。它保留两个官方 CLI 的原生会话、工具、Skills、MCP、Hooks、审批与沙箱，只负责把用户、两个 Agent 和 Git 工作区组织成可观察、可中断、可审计的协作过程。

> PairRoom 不代理模型 API，也不重写 Agent loop。真实模式只启动本机已有的 `claude` 与 `codex` 命令；账号、凭据、模型能力和供应商上下文仍由各自官方 Harness 管理。

[快速上手](docs/GETTING_STARTED.md) · [日常使用](docs/USER_GUIDE.md) · [核心概念](docs/CONCEPTS.md) · [CLI](docs/CLI_REFERENCE.md) · [排障](docs/TROUBLESHOOTING.md) · [完整文档](docs/README.md)

## 协作模型

PairRoom 使用确定性的 native Turn 接力，而不是让两个 Agent 像 IM 群聊一样并发互相唤醒：

```text
human input
  -> current Agent completes one native Turn
  -> reliable terminal boundary
  -> optional HANDOFF + NEXT
  -> next Room FIFO item
```

这不是机械的逐条消息 A/B/A/B。当前 Agent 可以在一个 native Turn 内调用工具并接受 steering；只有在可靠结束边界后，另一个 Agent 才能开始。

核心性质：

- **Human authority**：用户决定目标、审批、取消、停止和后续流程；
- **Single owner**：一个 Room 同时最多一个 active native Turn owner；
- **Explicit handoff**：普通 `@peer` 文本不触发投递，自动交棒需要有效 `HANDOFF` 与 `NEXT`；
- **FIFO and fail closed**：跨 Agent / `next_turn` 输入排队；重启不会自动重放进程内队列；
- **Auditable lifecycle**：Delivery 与 Processing 分开记录，重试创建新消息，不改写历史；
- **Native harness first**：PairRoom 不复制供应商内部 Transcript 或推理状态。

详细语义见 [Core Concepts](docs/CONCEPTS.md) 与 [Protocol](docs/PROTOCOL.md)。

## 运行入口

| 命令 | 用途 |
|---|---|
| `pairroom service` | 推荐入口；管理多个 Project、Room 和受容量约束的 Room Runtime |
| `pairroom daemon install` | 将同一个 Service 安装为 systemd、launchd 或 Windows Task Scheduler 后台任务 |
| `pairroom serve --repo ...` | 单仓库、单 Room 快捷入口 |
| 任一入口加 `--mock` | 不启动真实 Agent 的确定性体验与回归验证 |

所有内置 listener 只接受数字 loopback 地址。远程使用应通过 SSH 本地端口转发，而不是绑定 LAN 或公网地址。

## 最短成功路径

```bash
make build
./dist/pairroom service --mock
```

Management Shell 打开后：

1. 登记一个本地 Git worktree 的绝对路径；
2. 在 Project 下创建 Room，Claude/Codex Binding 先选 `new`；
3. 打开 Room，给当前 Driver 发送一个小型只读任务；
4. 在 Timeline 查看结论，在 Inspector 查看 Delivery、Processing、Turn、工具与审批。

切换到真实 Runtime 前：

```bash
pairroom doctor --repo /absolute/path/to/project
pairroom service --config /absolute/path/to/pairroom.json
```

`doctor` 只检查 executable 与必要协议面，不创建模型 Turn，也不能证明账号、网络或供应商服务当前可用。首次真实测试应使用非关键仓库和只读任务。

后台运行：

```bash
pairroom daemon install --runtime-limit 4 --idle-timeout 20m
pairroom daemon open
pairroom daemon status
pairroom daemon logs -f
```

完整步骤见 [Getting Started](docs/GETTING_STARTED.md)。

## 典型工作流

用户可以直接描述阶段顺序：

```text
Claude 规划；Codex 独立审查；等我批准后由 Codex 执行；最后 Claude 验收。
```

PairRoom 将阶段编译为串行 Turn，不会让两个 Runtime 自由并发聊天。计划、审查和验收使用只读边界；执行阶段使用 live Driver workspace。计划修改会使旧批准失效。

Agent 要把下一轮交给 peer 时，需提供有界证据包：

```text
[PAIRROOM:HANDOFF]
Goal: ...
Scope: ...
Evidence: ...
Risks: ...
Exact ask: ...
[/PAIRROOM:HANDOFF]
[PAIRROOM:NEXT]
```

## 数据与边界

- 每个 Room 有 append-only Event Log、附件库和 Claude/Codex Binding；
- Room 是 durable state，native process、active owner 与 Room FIFO 是 process state；
- Existing Binding 只恢复供应商 context，不导入绑定前 Transcript；
- Management Shell 与每个 Room View 使用独立的本地认证作用域；
- Project 注销、Room 归档、永久删除 PairRoom 数据和删除 Git worktree 是不同操作；
- Mock E2E 证明 PairRoom 控制面，不等同于真实 Claude Code/Codex native E2E。

阅读 [Storage](docs/STORAGE.md)、[Security](SECURITY.md)、[Privacy](docs/PRIVACY.md) 与 [Upgrading](docs/UPGRADING.md)。

## 文档导航

| 目标 | 文档 |
|---|---|
| 第一次运行 | [GETTING_STARTED](docs/GETTING_STARTED.md) |
| 页面操作、消息、Workflow 和 Room 生命周期 | [USER_GUIDE](docs/USER_GUIDE.md) |
| 理解 Project、Room、Binding、Runtime、Turn | [CONCEPTS](docs/CONCEPTS.md) |
| 配置 Agent、Provider 与 cc-connect | [CONFIGURATION](docs/CONFIGURATION.md) |
| 查询命令或 HTTP API | [CLI_REFERENCE](docs/CLI_REFERENCE.md) / [API_REFERENCE](docs/API_REFERENCE.md) |
| 理解代码与状态权威 | [ARCHITECTURE](docs/ARCHITECTURE.md) / [STORAGE](docs/STORAGE.md) |
| 部署、后台运行、备份和恢复 | [OPERATIONS](docs/OPERATIONS.md) |
| 参与开发或发布 | [CONTRIBUTING](CONTRIBUTING.md) / [RELEASING](docs/RELEASING.md) |

## 开发验证

```bash
make docs-check
make check
make smoke
```

`docs-check` 校验文档清单、链接、CLI、API、配置字段和已废弃文档引用，避免代码演进后静默漂移。

## License

[MIT](LICENSE)
