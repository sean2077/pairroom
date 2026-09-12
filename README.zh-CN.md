# PairRoom

[English](README.md) · **简体中文**

**两个独立编程 Agent，同一个问题，保留你的原生工作流。** PairRoom 连接受支持的 Claude Code、Codex 与 Grok Build 会话，让双方基于证据交叉审查，不替换原生编程 harness。一起审好方案，再让选定的 Agent 正常执行；工具、skills 和 subagents 仍由它自己的 harness 决定。

<p align="center">
  <img src="docs/images/pairroom-runtime-overview.png" alt="PairRoom 协作界面">
</p>

## 为什么使用？

当你反复需要在两个已有会话之间搬运方案、异议和修正时，PairRoom 才有明确价值。目标是减少协调负担、改善决策，而不是增加一套必须遵循的 Agent 层级。

- **共同审查一个问题。** 双方都可以是高能力审查者。默认 Lead/Executor 职责可以变通；简单任务由被指定的 Agent 直接完成。自定义自然语言规则无需阶段编译器，审查完成也不自动授权实现。
- **保留需要的交互入口。** Embedded 通过受支持的原生适配器提供 PairRoom 界面和控制。实验性的 Native 保留你自己的 Claude Code / Codex 会话，包括目标中的 Codex Desktop 工作方式；依靠已批准 hooks 和有界接力，不接管原生进程。
- **保留项目工作流。** 沿用仓库规则、agent-scaffold、worktree 和 PR/MR 流程。中继发送完整的定向回复，不追加累计 Room 历史。紧凑的字节预算不等于保证账单更低或准确率更高。

| 宿主模式 | 配置与控制 | 边界 |
|---|---|---|
| **Embedded** | 每个槽位独立选择受支持的 Runtime、Provider、模型、effort 和指令；未指定项继承原生配置。常用组合可保存为 [Agent 组合配置](docs/CONFIGURATION.md#agent-pair-profiles)。 | PairRoom 同时只调度一位参与者的 Turn，提供 Room 控制；这不是独立的 Codex Desktop 界面。 |
| **Native（实验性）** | 原生 harness 各自控制 Provider、模型、effort、工具和权限；PairRoom 负责绑定、持久中继和审计。 | Room 配置字段不会覆盖原生进程。自动继续有窗口限制，真实认证多轮 E2E 仍是发布验收门槛。 |

原生 harness 已有 subagents 和多 Agent 能力。Orca 也能围绕同一问题协作，不只是并行派活，并提供更完整的工作台。选择 PairRoom 应基于它具体的跨会话审查方式，而不是假设其他工具没有这些能力。

**[Why PairRoom](docs/WHY_PAIRROOM.md)** 说明适用场景、成本与限制；**[替代方案比较](docs/ALTERNATIVES.md)** 基于注明日期的一手资料，对照 Orca、原生 Claude Code/Codex、Cherry Studio 等；**[先审查、再选择执行方式](docs/GETTING_STARTED.md#review-first-execute-where-it-fits)** 提供讨论方案与交回原生执行的提示词。

## 安装与体验

从 [Releases](https://github.com/sean2077/pairroom/releases/latest) 下载对应安装包。`pairroom-cli-…` 为命令行，`pairroom-desktop-…` 为桌面包。Windows 桌面包以 `-setup.exe` 结尾；Linux 使用 `.deb` / `.AppImage`，macOS 使用 `.app.zip`。

Linux、macOS 或 Git Bash 的 CLI 安装入口：

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
pairroom service --mock
```

执行前检查安装脚本，或直接下载对应 CLI 文件。Windows PowerShell 中可使用 `./pairroom.exe service --mock` 启动下载的可执行文件。

**使用预编译 CLI 或桌面包不需要 Go。** 首次建议选择可丢弃的 Git 仓库和新的 Mock Room。Mock 不启动供应商 CLI，也不消耗模型额度。避免与已运行的 Service 争用数据目录；[入门指南](docs/GETTING_STARTED.md) 提供独立演示和两种真实宿主模式的步骤。

在 Management 注册仓库为 Project，创建 Embedded Room，选择双方及权限，再发送小任务。测试真实的纯讨论任务前，先选择原生运行时支持的只读限制，然后使用：

```text
和对方基于仓库共同审查这个方案。质疑关键假设，依据证据修订，
只交换有价值的新发现。不要实现。
将审查后的方案、未解决决策和剩余不确定性交回给我。
完成后停止，不交换纯确认消息。
```

这是任务指令，不是强制审批闸门。使用真实 Agent 前，每个所选 CLI 都应已独立安装、完成认证且能正常工作。保留 Codex Desktop 要使用下方 Native 路径，不是将 Embedded 附着到正在运行的 Desktop 会话。

## 必须了解的边界

**新 Embedded Room 的两位参与者默认均为 YOLO。** 需要更严格的原生权限时必须明确选择。Native Room 沿用原生 harness 的权限。职责不会限制工具访问；Embedded 单 Turn 规则不是操作系统沙箱，也不会锁住其他 Room、subagents 或外部进程的写入；Native 的所有权只是建议性的。

PairRoom 没有自动接力次数或费用上限。持久化恢复会区分安全排队与结果不确定的投递，不会在崩溃后盲目重放。本地保存状态不代表云端模型请求不离开本机。详见[安全说明](SECURITY.md)、[核心概念](docs/CONCEPTS.md)与[存储恢复](docs/STORAGE.md)。

## Native 宿主模式（实验性）

创建 Room 时选择 **Native**，双方保留在自己的 Claude Code / Codex 原生会话中；PairRoom 负责绑定、持久化中继和审计，不启动或中断原生进程。安装并批准项目级 Stop hooks，分别绑定槽位，在各自回复中返回一次性 nonce。

`pairroom-relay` 技能位于 `skills/`，可经技能安装器分发（`npx skills add sean2077/pairroom`），`relay install` 也写入同一份文件。加载后 `/pairroom-relay <topic>` 创建 Room、绑定当前会话并返回对方的加入命令；`pairroom relay bind` 可在识别到的原生会话内零参数运行。后续审查复用绑定，不要每轮重建 Room。

[Native 入门](docs/GETTING_STARTED.md#keep-codex-desktop-a-native-room)与[恢复命令](docs/CLI_REFERENCE.md#native-relay-commands)说明有界 park、前台取件和显式 Retry。Provider、模型、effort、权限仍由原生会话控制。真实认证后的多轮互通仍是发布验收门槛，合成测试不代表模型已接受消息。

## 桌面端与源码开发

桌面端和浏览器共用 Management Shell 与 Service。启动桌面端不会安装 daemon：它会复用已安装的 daemon，或自行管理内嵌 Service。**设置 → 桌面端 → 开机启动** 只改变操作系统的登录启动注册。关闭窗口会隐藏到托盘；退出桌面端不会停止外部 daemon。详见[桌面生命周期](docs/OPERATIONS.md#desktop-lifecycle)。

从源码仓库开发，并安装开发依赖后，可运行：

```bash
make dev            # 停止已安装的 daemon，运行当前源码的 Service
make docs-check
make check
make smoke
```

桌面构建、打包及更新现有本地安装分别使用 `make desktop-build`、`make desktop-package`、`make desktop-update`，详见[桌面开发说明](desktop/README.md)。这些是源码开发命令，不是使用 Release 安装包的前提。

## 文档与支持

[文档地图](docs/README.md) · [配置](docs/CONFIGURATION.md) · [CLI](docs/CLI_REFERENCE.md) · [API](docs/API_REFERENCE.md) · [故障排查](docs/TROUBLESHOOTING.md) · [升级](docs/UPGRADING.md) · [贡献指南](CONTRIBUTING.md) · [支持范围](SUPPORT.md)

PairRoom 仍在演进。[Changelog](CHANGELOG.md) 记录发布历史，当前行为以参考文档为准。Mock 与浏览器 fixture 测试不能代替真实供应商 E2E；桌面包也不宣称已完成生产签名或 notarization。界面支持英文和简体中文，维护中的技术文档使用英文。

## License

[MIT](LICENSE) · [第三方许可声明](THIRD_PARTY_NOTICES.md)
