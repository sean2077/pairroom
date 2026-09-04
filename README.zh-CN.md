# PairRoom

[English](README.md) · **简体中文**

<p align="center">
  <img src="docs/images/pairroom-runtime-overview.png" alt="PairRoom Room View：两个 Agent 槽位在同一 Room 中按 Turn 协作">
</p>

PairRoom 是一个运行在本机的协作控制面，协调官方 Claude Code、Codex 与 Grok Build。每个持久 Room 有两个独立的 Agent 槽位；任一槽位都可以选择 Claude Code、Codex 或 Grok Build，两个槽位也可以使用相同 runtime。它保留各官方 CLI 的会话、工具、审批与沙箱能力，只负责把用户、两个 Agent 和项目工作区组织成可观察、可中断、可审计的协作过程。

创建 Room 时，可以分别配置两个 Agent 槽位的 Runtime、native 或 CC Switch Provider 引用、可编辑 Model、Effort、附加指令与 Runtime 专属安全策略。配置会随 Room 固化并保持只读。Native Provider 引用继承原生 CLI 的用户/全局配置；PairRoom 只读使用 CC Switch 3.20.1/schema 18 中受支持的 API-key Profile，不改变 CC Switch 当前项，也不保存凭证。

Management、Room View 与 Desktop 启动页共享内嵌的 i18next 26.4.2 `en`/`zh-CN` 词典和持久化语言选择。Management 顶栏、Room tabstrip、Settings 与独立 Room 共用 `system | light | dark` 主题；内嵌 Room 跟随 Management。

## 核心模型

PairRoom 不让两个 Agent 像 IM 群聊一样并发互相唤醒。每个 Room 同一时刻只有一个 **native Turn owner**：

```text
user
  -> current Agent completes one native Turn
  -> reliable terminal boundary
  -> 仅在确实需要另一轮时写出对方当前精确句柄
  -> 完整回复进入 Room FIFO
  -> 不点名即结束接力
```

这不是机械的 A/B/A/B 消息轮换。当前 Agent 可以在一个 native Turn 内执行工具、更新计划并接受 steering；只有在可靠的 Turn 结束边界之后，另一个 Agent 才能开始。

关键性质：

- **Human authority**：用户可以指定目标 Agent、覆盖后续流程、审批、取消或停止；
- **Single owner**：两个 native runtime 不会同时拥有执行权，即使两个槽位选择了相同 runtime；
- **精确动态点名**：唯一 runtime 使用 `@claude`、`@codex` 或 `@grok`；同类双开使用稳定槽位后缀，例如 `@codex0` 与 `@codex1`。只有对方当前精确句柄会在 native Turn 边界后接力完整回复；无点名即结束，`@user` 始终把决定交还用户；
- **持久 FIFO 与 fail closed 提交**：尚未跨过原生边界的排队工作会在重启后恢复；原生提交结果不确定时绝不自动重放；
- **无接力上限**：PairRoom 不计算 Agent hop。Agent 会被要求在不再需要独立响应时停止点名，用户也可以显式取消、打断或改向；
- **Native harness first**：PairRoom 不重写 Claude Code、Codex 或 Grok Build 的工具循环与权限模型。

## 安装

CLI（Linux / macOS / Git Bash）：

```bash
curl -fsSL https://github.com/sean2077/pairroom/releases/latest/download/install.sh | sh
```

Windows PowerShell：

```powershell
$tag = (Invoke-RestMethod https://api.github.com/repos/sean2077/pairroom/releases/latest).tag_name
curl.exe -fsSL -o pairroom.exe "https://github.com/sean2077/pairroom/releases/download/$tag/pairroom-cli-$tag-windows-amd64.exe"
```

Release 资产按前缀区分：`pairroom-cli-vX.Y.Z-…` 是命令行，`pairroom-desktop-vX.Y.Z-…` 是桌面包。Windows 桌面是 `-setup.exe`（与 CLI 的 `.exe` 区分）；Linux 用 `.deb` / `.AppImage`，macOS 用 `.app.zip`。

## 快速体验

关掉遗留 daemon，并打开当前源码的 Management Shell：

```bash
make dev
```

Management Shell 打开后：

1. 注册一个本地 Git Project；
2. 创建 Room；
3. 选择 Driver / Reviewer；
4. 向一个 Agent 发送任务，并让它只在确实需要另一轮时点名对方；
5. 在 Room View 中观察 Turn、工具活动、审批、投递与错误状态。

使用真实 Runtime 前，先分别确认所选 CLI（`claude`、`codex` 和/或 `grok`）已安装、已针对所选 Provider 完成认证，并能在目标仓库独立工作。创建 Room 的 catalog 会显示不可用 Runtime 与不受支持的 CC Switch Profile，但不会通过网络枚举模型。完整步骤见 [Getting Started](docs/GETTING_STARTED.md)。

## 桌面端

`desktop/` 提供基于 **Wails v3** 的 Windows、macOS 与 Linux 原生入口。它不是第二套 PairRoom 后端或前端：桌面 Host 直接复用现有 Management Shell、Room View、Service Registry、Runtime Manager、配置、锁和 native Agent adapters。

启动时，桌面端会按顺序：

1. 验证并复用显式提供的 authenticated numeric-loopback Management URL；
2. 发现已安装的 `pairroom daemon`，停止状态会先由桌面端启动并等待 authenticated Management URL；
3. 只有没有安装 daemon 时，才在当前桌面进程中启动 PairRoom Service；已安装但不可达时 fail closed，不启动第二个 Service。

关闭主窗口只会隐藏到系统托盘，不会中断活动 Agent。显式退出只关闭桌面端拥有的内嵌 Service，并沿现有 native-Turn drain 边界优雅退出；外部 daemon 不受影响。构建、依赖和安装包说明见 [PairRoom Desktop](desktop/README.md)。浏览器和 CLI 入口保持完整可用。

## 文档入口

- [文档地图](docs/README.md)
- [核心概念与接力语义](docs/CONCEPTS.md)
- [配置与 Provider](docs/CONFIGURATION.md)
- [CLI 参考](docs/CLI_REFERENCE.md)
- [架构与不变量](docs/ARCHITECTURE.md)
- [运维、备份与恢复](docs/OPERATIONS.md)
- [升级说明](docs/UPGRADING.md)
- [贡献指南](CONTRIBUTING.md)

## 开发验证

根模块：

```bash
make docs-check
make check
make smoke
```

桌面模块：

```bash
make desktop-build
make desktop-package
```

`make desktop-build` 构建当前平台的桌面 Host 和捆绑的 `pairroom` CLI，`make desktop-package` 构建当前平台的生产安装包或应用包（Windows 为 NSIS 安装包，内含 `PairRoom.exe` 与 `bin\pairroom.exe`），产物位于 `desktop/bin/`。桌面模块测试仍从 `desktop/` 目录运行：`cd desktop && go test -count=1 ./...`。

`docs-check` 会校验文档链接、源码路径、CLI 参数、HTTP 路由和 JSON 配置字段，防止文档在代码继续演进后静默漂移。根模块与桌面模块均使用 Go 1.25；根模块只允许固定的 CGo-free SQLite 依赖闭包，Wails 仍隔离在桌面模块。第三方许可见 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md)。

## 状态与边界

PairRoom 仍在快速演进。CLI、HTTP API、Event Log 和 Agent 协议的 breaking change 会记录在 [CHANGELOG](CHANGELOG.md) 与 [Upgrading](docs/UPGRADING.md)。当前 Mock E2E 可以验证调度、持久化和恢复链路，但不能替代真实 Claude Code / Codex / Grok Build native E2E；桌面 CI 的 unsigned packages 也不能替代生产签名与 macOS notarization。

## License

[MIT](LICENSE)
