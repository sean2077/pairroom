# Getting started

本指南只覆盖“从零到完成第一个协作 Turn”。概念、全部配置和运维细节分别放在其他文档中。

## 1. 前置条件

- 一个本地 Git 仓库；
- CLI / browser 模式下，与根 `go.mod` 匹配的 Go toolchain；
- 从源码构建桌面端时，满足 `desktop/README.md` 所列的 Wails v3 toolchain 与平台依赖；
- 真实模式下可独立运行并已登录的 `claude` 与 `codex` CLI；
- 浏览器或 PairRoom Desktop 可以访问本机 loopback Service。

初次使用建议先跑 Mock 模式，它能验证 PairRoom 的调度、UI、Event Log 和恢复行为，而不消耗模型额度。

## 2. 选择启动入口

### PairRoom Desktop

安装对应平台的桌面 package 后直接启动 PairRoom。桌面 Host 会先验证并尝试复用显式 Management URL 或已安装 daemon；没有可复用 Service 时，它会在桌面进程中启动一个内嵌 Service。

从源码构建：

```bash
make desktop-build
make desktop-package
```

两个目标都针对当前主机平台运行；打包产物写入 `desktop/bin/`。如果需要单独运行桌面模块测试，可执行 `cd desktop && go test -count=1 ./...`。

桌面主窗口加载的仍是现有 Management Shell，不存在独立的桌面业务状态。关闭窗口只会隐藏到托盘；使用托盘的 **Quit PairRoom** 才会退出应用。

### CLI + browser

```bash
go run ./cmd/pairroom service --mock
```

PairRoom 默认只监听 loopback。若没有自动打开浏览器，终端会显示 Management Shell 地址。精确选项以命令自身为准：

```bash
go run ./cmd/pairroom service --help
```

两种入口共享同一套 Project、Room、Binding、Event Log、Runtime 和认证语义。

## 3. 创建 Project 与 Room

在 Management Shell 中：

1. 注册目标仓库为 Project；
2. 创建 Room；
3. 确认 Claude 与 Codex 的 Binding；
4. 指定 Driver / Reviewer，或保持 Peer；
5. 打开 Room View。

Project 是仓库级管理记录；Room 是一次长期协作上下文。注销 Project 不等于删除仓库，归档 Room 也不等于永久删除 Room 数据。

## 4. 完成第一个 Turn

先只选择一个 Agent，发送一个可验证的小任务，例如：

```text
阅读当前仓库并说明测试入口，不要修改文件。
```

Room 中应依次出现：

```text
message accepted
  -> native Turn started
  -> tool / text / approval events
  -> native Turn completed
  -> Room owner released
```

若要顺序协作，可以直接描述角色和阶段：

```text
Claude 先规划；Codex 审查方案；等我批准后由 Codex 执行；最后 Claude 验收。
```

PairRoom 会把阶段编译为顺序执行的 Workflow，而不是让两个 runtime 自由群聊。

## 5. Steering、下一 Turn 与取消

- 发给当前 owner 的普通输入可进入当前 Turn 的 steering 路径；
- 显式 `next_turn` 或发给另一 Agent 的输入进入 Room FIFO；
- Agent 回复中明确 `@claude`、`@codex` 或 `@peer` 会在当前 Turn 结束后交给对应 peer；`@human`/`@user` 会把决定留给用户；
- 取消仍在 FIFO 的消息只移除该消息；
- 已提交给 native runtime 的输入可能需要中断整个当前 native Turn。

详细语义见 [Concepts](CONCEPTS.md)。

## 6. 切换到真实 Agent

先在目标仓库中分别验证：

```bash
claude --version
codex --version
```

然后去掉 `--mock`，在配置或 UI 中选择所需模型、Provider、权限与 sandbox。PairRoom 不代替两个 CLI 的登录和凭据管理。

## 7. 正确结束

- 暂时不用：退出 Room，Runtime 可按 idle policy 回收；
- 关闭桌面主窗口：仅隐藏到托盘，活动 Turn 继续运行；
- 退出桌面应用：内嵌 Service 会按 Management → Runtime drain → lock release 的顺序关闭；外部 daemon 保持运行；
- 阶段完成：归档 Room；归档会先停止当前 Agent Turn；
- 确定不再需要：按 UI / API 的永久删除流程处理，并先保留备份。

下一步阅读 [Configuration](CONFIGURATION.md) 和 [Operations](OPERATIONS.md)。
