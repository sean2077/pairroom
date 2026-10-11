# PairRoom

[English](README.md) · **简体中文**

**两个独立编程 Agent，同一个问题，保留你的原生工作流。** PairRoom 在本机或局域网内连接 [Claude Code](https://code.claude.com/docs/en/overview)、[Codex](https://github.com/openai/codex)、[Grok Build](https://docs.x.ai/build/overview) 和 [Gemini CLI](https://github.com/google-gemini/gemini-cli) 会话，让它们对照仓库证据互相审查，保留各自的模型循环、工具、skills 和 subagents。

<p align="center">
  <img src="docs/images/pairroom-native-room-zh.png" alt="Native Room 在你自己的 Claude Code 与 Codex 会话之间中继">
</p>

## 为什么使用？

平时让 Claude Code 出方案、再把方案复制给 Codex 审，审完再贴回去……这种来回搬运做多了很累。PairRoom 让两个 Agent 直接对话：一个提方案，另一个挑刺、补充、执行，全程你都看得到，也随时可以插话。

- **两个 Agent，自由组合**：每个槽位都能选任一受支持的 Runtime，两边用同一个也可以。任务拆分、工具和 subagents 仍由各自的 harness 管理。
- **职责可以自定义**：默认 Agent 1 规划和审核，Agent 2 实现、验证和补充。自定义指令可以让双方都参与审查，也可以分工实现。简单任务可以直接完成；审查结束本身不代表授权实现。
- **协作过程可见**：已发布的消息在 Room 中可见。Native 下双方的工作仍在各自的终端或客户端中进行，你可以直接叫停或纠正。
- **同事的 Agent 也能加入**：局域网 Room 只有房主需要 Service。各机器保留自己的工作区、凭据和原生权限；通过不同会话，本地 Room 与已加入的 Room 可以同时使用。

| 宿主模式 | 适合的需求 | 边界 |
|---|---|---|
| **Native**（默认并推荐） | 在熟悉的终端（如 [WezTerm](https://wezterm.org/)）或客户端中继续使用已有会话，包括 Codex Desktop | PairRoom 负责绑定、持久中继和审计；配置、权限和执行归原生 harness 管，Room 里的选择只作展示。 |
| **Embedded**（可选） | 使用 PairRoom 桌面端或网页端的对话界面和适配器控制，按槽位选择支持的 Provider、模型、effort 和指令 | 适配器由 PairRoom 管理，同一 Room 同一时间只运行一个原生 Turn；未设置的项继承原生配置。 |

PairRoom 沿用仓库现有的指令、worktree 和 PR/MR 流程。中继不引入协调模型或强制阶段，也不会在每条消息里附上累计的 Room 历史。目前没有 token 或费用节省的对照实测。

[Why PairRoom](docs/WHY_PAIRROOM.md) 讲适用场景和限制，[替代方案](docs/ALTERNATIVES.md) 用注明日期的资料比较 [Orca](https://github.com/stablyai/orca) 等 Agent 工作台和工具，[核心概念](docs/CONCEPTS.md) 解释 Runtime、Provider、槽位、Binding 等术语。现成的提示词见[先审查再执行](docs/GETTING_STARTED.md#review-first-execute-where-it-fits)。

Gemini CLI 支持两种模式，同一 Room 也可以绑定两个 Gemini 会话。Embedded 当前仅支持新建会话；已接受输入的会话在进程退出后会阻止恢复，避免将历史回复重新发布。其 hooks、原生认证和当前能力边界见 [Gemini 接入说明](docs/NATIVE_RELAY.md#gemini-cli)。

## 界面一览

截图使用合成的演示对话，并非真实模型输出。

**Embedded Room**：由 PairRoom 托管两个 Agent，工作检查器中查看 Turn 摘要、Diff 与审批。

![Claude Code 主导、Codex 执行的 Embedded Room](docs/images/pairroom-embedded-room-zh.png)

**管理界面**：在同一个本地控制台里管理项目、两种宿主模式的 Room 与运行时容量。

![列出 Embedded 与 Native Room 的项目详情](docs/images/pairroom-management-zh.png)

**创建 Room**：选择宿主模式、协作方式以及每个槽位的运行时。

![创建 Room 对话框](docs/images/pairroom-create-room-zh.png)

## 安装与体验

从 [Releases](https://github.com/sean2077/pairroom/releases/latest) 下载安装包。Windows 使用桌面版 `setup.exe`；选择 WinGet 前先检查[官方源是否已收录](docs/INSTALLATION.md#winget)。预编译包**不需要 Go**。各平台安装、CLI 可用性检查、升级和卸载见[安装指南](docs/INSTALLATION.md)。

在 Linux、macOS 或 Git Bash 下只装 CLI，先下载安装脚本：

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh -o install-pairroom.sh
```

看过 `install-pairroom.sh` 的内容后再执行：

```bash
sh install-pairroom.sh
pairroom version
```

日常工作中，先安装并登录所需的原生 CLI。在本机托管 Room 时，打开 Desktop 或运行 `pairroom service`，再按 [Native 设置](#native-宿主模式)连接会话。加入同事的 Room 时，按[局域网流程](#与局域网同事协作)操作，本机无需 Service。新 Room 默认使用 Native；已有 Room 保留原来的模式。`pairroom version` 确认当前 shell 运行的 CLI 版本，不检查原生登录状态。

也可以先体验不需要模型账号的演示，用未占用的数据目录在前台启动 Mock Service：

```bash
pairroom service --mock --data-root "$HOME/.pairroom-demo"
```

在 Management 中把一个可丢弃的 Git 仓库注册为 Project，创建 Room 时显式选择 **Embedded**，发一个小任务。Mock 不会启动任何供应商 CLI，也不消耗额度，所以它展示的是流程，不是模型能力。`Ctrl+C` 可停止。启动 URL 带有登录令牌，不要分享。

需要 PairRoom 管理适配器，或使用支持的按槽位 Provider 覆盖时，显式选择 **Embedded**，按 [Embedded 入门步骤](docs/GETTING_STARTED.md#first-embedded-room)操作。

想让编程 Agent 带你完成安装和检查，就让它读 [Agent 协助安装](docs/AGENT_SETUP.md)：

```text
Read https://raw.githubusercontent.com/sean2077/pairroom/main/docs/AGENT_SETUP.md
and help me install PairRoom and check my environment. Ask before each change.
```

## 必须了解的边界

- **新建的 Embedded Room 默认让两位参与者都以 YOLO 模式运行**，也就是最宽松的原生权限设置。需要更严格的权限时要主动选择，可选项见[配置说明](docs/CONFIGURATION.md)。Native Room 沿用各 harness 已有的权限。
- 职责不限制工具，两种模式都不会锁住仓库、阻止其他写入者。Native 下的 Turn 所有权只是约定，不强制。
- 中继轮数和费用没有自动上限。
- 崩溃后，排队的任务继续排队，结果不确定的投递等你检查后再处理，不会盲目重放。
- Room 历史由房主 Service 保存；局域网参与方保留自己的私有客户端状态。模型请求仍会发往各自的 Provider。

详见[安全说明](SECURITY.md)、[核心概念](docs/CONCEPTS.md)和[存储恢复](docs/STORAGE.md)。

## Native 宿主模式

连接本机的两个会话时，确认两个 Agent 的工具 shell 都能运行 `pairroom`，并且你的数据目录有一个正在运行的非 Mock Service。[Agent 协助安装](docs/AGENT_SETUP.md#6-native-bind-two-existing-sessions)包含安装与检查步骤。在项目 worktree 中为实际用到的 Runtime 安装 hooks：

```bash
pairroom relay install --runtime claude,codex
```

在每个 harness 中批准 hooks，并按要求重新加载。本地绑定前，在每个会话里运行 `pairroom relay preflight`，检查 PATH、Service 和 hook 安装情况。阅读 `next_steps`，包括版本警告；它看不到你是否已批准 hooks。

在第一个会话中加载已安装的技能并运行 `/pairroom-relay <topic>`。它会创建 Room 并打印加入命令，让第二个会话的 Agent 原样执行。Bind 读取官方工具调用会话元数据；Gemini 需要先批准 BeforeTool hook。双方绑定成功后，后续轮次复用同一个 Room。每个会话首次完成一轮后，`pairroom relay doctor` 中近期的 `last_hook_at` 表明响应 hook 已运行。完整步骤见[第一个 Native Room](docs/GETTING_STARTED.md#first-native-room)。

回复怎么传递：

- Stop 回复中点名对方的准确 handle（由 `bind` 返回），完整内容会发给对方；点名 `@user` 则发布到 Room 给你看。
- **两个 handle 都没点名的 Stop 回复保持私有，不会复制进 Room。**
- `relay send` / `exchange` 按命令指定的目标投递，不看正文里的点名。同一内容两条路径都走，会产生两条消息。

详见[发布规则](docs/NATIVE_RELAY.md#what-is-published)。

响应 hooks 在有限等待窗口内收取消息。窗口结束后，符合条件的 Claude Code/Codex 会话可以收到内容固定、不含消息正文的唤醒；Grok/Gemini 需要其文档规定的收件或人工路径。CLI 等待期间不调用模型，后续模型工作仍消耗额度。PairRoom 从不启动或中断原生会话。依赖无人值守协作前，请查看[各 Runtime 的续聊限制](docs/NATIVE_RELAY.md#long-unattended-runs-by-runtime)。

[Native relay](docs/NATIVE_RELAY.md) 集中说明日常命令、文件证据、cwd/worktree 发现和恢复。技能也可以通过 `npx skills add sean2077/pairroom` 安装；只装技能不会安装或批准 hooks。

### 与局域网同事协作

只有 Room 的房主需要运行 Service。在**设置 → 局域网协作**开启托管，再创建 Native Room 并选择**通过局域网邀请同事**，或执行 `pairroom relay bind --create --share lan`。把返回的加入命令交给同事；同事在自己的机器上安装并批准 hooks 后，让 Agent 运行 `pairroom relay preflight --join`，再执行加入命令。你通过已有可信渠道收到并接受准确回执后，对方再执行同一条加入命令完成加入。邀请本身不授予 Room 访问权限。

同一台机器可以通过不同的原生会话，同时托管自己的 Room、加入其他房主的 Room；每个绑定保留自己的房主目标。两位用户都能查看共享历史。参与方可选的本机 Service 提供**已加入的 Room**界面、用户消息和支持的本机唤醒观察；Agent 命令和 hooks 不依赖它。没有活动收件路径时，消息保留在房主队列中。证据上传、重连、退出和撤销访问见[局域网 Native 协作](docs/LAN_NATIVE.md)。

### Native 恢复与评审

通过 Native Room 的对话和工作检查器、`pairroom relay doctor` 或 `pairroom relay history --pending` 检查投递情况。`handed_off` 表示 CLI 已把消息写到 stdout，不代表模型读取或完成了任务。显式 Retry 前先检查不确定的副作用。可选的 Git 评审版本记录被审查的证据，不代表批准。[Native 恢复](docs/NATIVE_RELAY.md#recovery-and-review-surface)集中说明回执恢复和各界面的可用控制。

## 桌面端与源码开发

桌面端和浏览器共用同一个 Management Shell 与 Service。桌面端启动时从不安装 daemon：已安装 daemon 就复用，否则运行自己的内嵌 Service。**设置 → 桌面端 → 开机启动** 只改变操作系统的登录启动注册。关闭窗口会隐藏到托盘，退出不会停止外部 daemon。关闭 Room 标签既不归档 Room，也不停止原生任务。详见[运行维护](docs/OPERATIONS.md)。

装好源码开发依赖后：

```bash
make dev            # 停止已安装的 daemon，运行当前源码的 Service
make docs-check
make check
make smoke
```

桌面端源码构建使用 `make desktop-build`、`make desktop-package` 和 `make desktop-update`；使用发布包不需要这些命令。详见[贡献指南](CONTRIBUTING.md)和[桌面开发](desktop/README.md)。

## 文档与支持

[文档地图](docs/README.md) · [配置](docs/CONFIGURATION.md) · [CLI](docs/CLI_REFERENCE.md) · [API](docs/API_REFERENCE.md) · [故障排查](docs/TROUBLESHOOTING.md) · [升级](docs/UPGRADING.md) · [支持范围](SUPPORT.md)

`main` 上的文档跟随开发进度。使用已发布版本时，请看对应 tag 的文档，并以二进制的 `--help` 查询参数。[Changelog](CHANGELOG.md) 记录历史，参考文档描述当前行为。[支持范围](SUPPORT.md)区分兼容性与验证证据；合成演示和注明日期的工作会话报告不代表当前供应商组合已经验收。桌面包未做生产签名或 notarization。界面支持英文和简体中文，技术文档使用英文。

## 友情链接

- [LINUX DO](https://linux.do) — 真诚分享、友好讨论的技术社区，本项目的交流与反馈也发布于此

## License

[MIT](LICENSE) · [第三方许可声明](THIRD_PARTY_NOTICES.md)
