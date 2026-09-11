# PairRoom

[English](README.md) · **简体中文**

**两个原生编程 Agent，一个任务，一个本地 Room。** PairRoom 协调 Claude Code、Codex 与 Grok Build，不替换它们的原生编程 harness。让一位参与者规划和审查，另一位实现、验证，并对方案提出有依据的补充或质疑。

<p align="center">
  <img src="docs/images/pairroom-runtime-overview.png" alt="PairRoom 协作界面">
</p>

## 为什么使用？

当你已经在使用两个编程会话，却反复需要搬运回复、确认谁正在执行、转交审查意见，以及排查中断后究竟做到了哪一步时，PairRoom 才有明确的价值。

- **保留原生工具。** 两个 Agent 槽位分别选择受支持的原生运行时（Runtime）、Provider、模型、effort 和附加指令，也可使用两个相同 Runtime。未指定的覆盖项继承原生 CLI 配置；受支持的 CC Switch Profile 仅以只读引用使用。 常用双 Agent 组合可保存为 [Agent 组合配置](docs/CONFIGURATION.md#agent-pair-profiles)，并设为新建 Room 的默认组合。
- **围绕同一项修改协作。** 默认由 Agent 1 担任主导者（Lead），Agent 2 担任执行者（Executor）；自定义模式使用你的自然语言规则。规则在创建 Room 时固定，不会编译成死板的阶段流程。
- **看清并控制 Agent 接力。** 每个 Room 同一时刻只有一位参与者拥有原生 Turn。回复点名对方的精确句柄，才在 Turn 结束后接力完整回复；不点名则结束。你可以查看工具与审批、追加引导或排队输入、取消、打断，并检查持久投递状态。

这不意味着两个 Agent 总比一个更准确或更便宜。简单任务直接使用原生 CLI 往往就够了。Cherry Studio 已有执行型 Agent 和文档化的跨会话协作，原生 harness 也已有多 Agent 能力。选择 PairRoom 的理由应当是它具体的本地双 Agent 工作方式，而不是假设其他工具没有这些功能。

**[Why PairRoom](docs/WHY_PAIRROOM.md)** 说明适用场景、代价、示例和评估方法；**[替代方案比较](docs/ALTERNATIVES.md)** 基于注明日期的一手资料，对照 Cherry Studio、原生 Claude Code/Codex、Aider、Vibe Kanban、Conductor 与手动接力。

## 安装与体验

从 [Releases](https://github.com/sean2077/pairroom/releases/latest) 下载对应安装包。`pairroom-cli-…` 为命令行，`pairroom-desktop-…` 为桌面包。Windows 桌面包以 `-setup.exe` 结尾；Linux 使用 `.deb` / `.AppImage`，macOS 使用 `.app.zip`。

Linux、macOS 或 Git Bash 的 CLI 安装入口：

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
pairroom service --mock
```

执行前检查安装脚本，或直接下载对应 CLI 文件。Windows PowerShell 中可使用 `./pairroom.exe service --mock` 启动下载的可执行文件。

**使用预编译 CLI 或桌面包不需要安装 Go。** 首次使用建议选择可丢弃的 Git 仓库，并创建新的 Mock Room。Mock 不启动供应商 CLI，也不消耗模型额度。避免与已运行的 Service 争用数据目录；[入门指南](docs/GETTING_STARTED.md) 提供独立演示目录的启动方式，以及切换到真实 Agent 的步骤。

在 Management Shell 中注册仓库为 Project，创建 Room，选择两位参与者及其权限，再向 Agent 1 发送一个小任务。可用下面的指令体验协作：

```text
规划能解决问题的最小修改，让另一位参与者负责实现和验证，
然后审查实际 diff 与测试证据。任务完成后停止，不交换纯确认消息。
只有无法从仓库判断的产品决策才询问我。
```

这是期望的任务顺序，不是强制审批闸门。使用真实 Agent 前，应确认所选 CLI（`claude`、`codex` 和/或 `grok`）已独立安装、完成认证，并能在目标仓库正常工作。

## 必须了解的边界

**新 Room 的两位参与者默认均为 YOLO。** 两者使用实时工作区；主导者和执行者只是职责，不会限制工具权限。需要更严格的原生权限时必须明确选择。Room 内的单 Turn 规则不是操作系统沙箱，也不会锁住其他 Room 或外部进程对仓库的写入。

PairRoom 没有自动接力次数或费用上限。重启恢复会区分尚未提交、提交结果不确定和已被原生运行时接受的工作，不会盲目重放执行。本地保存状态也不意味着云端模型请求不会离开本机。详见[安全说明](SECURITY.md)、[核心概念](docs/CONCEPTS.md)与[存储恢复](docs/STORAGE.md)。

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

## Native 宿主模式（实验性）

创建 Room 时选择 **Native**，双方保留在自己的 Claude Code / Codex 原生会话中；PairRoom 只负责绑定、持久化中继和审计，不启动或中断原生进程。安装并批准项目级 Stop hooks，分别绑定槽位，再在各自回复中返回一次性 nonce 完成关联。`pairroom-relay` 技能位于 `skills/`，可经技能安装器分发（`npx skills add sean2077/pairroom`），`relay install` 也写入同一份规范文件；加载后 `/pairroom-relay <topic>` 一步创建 Room 并绑定当前会话、返回对方的加入命令，`pairroom relay bind` 在可识别的原生会话内零参数完成加入。[设置与恢复命令](docs/CLI_REFERENCE.md#native-relay-commands) 说明 park 等待窗口、前台取件与显式 Retry。Provider、模型、effort 和权限仍由原生会话控制。真实认证后多轮互通仍是发布验收门槛，合成 hook 测试不代表模型已接受消息。
