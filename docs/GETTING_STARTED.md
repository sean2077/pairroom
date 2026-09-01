# Getting started

本指南只覆盖“从零到完成第一个协作 Turn”。概念、全部配置和运维细节分别放在其他文档中。

## 1. 前置条件

- 一个本地 Git 仓库；
- 与 `go.mod` 匹配的 Go toolchain；
- 真实模式下可独立运行并已登录的 `claude` 与 `codex` CLI；
- 浏览器可以访问 PairRoom 监听地址。

初次使用建议先跑 Mock 模式，它能验证 PairRoom 的调度、UI、Event Log 和恢复行为，而不消耗模型额度。

## 2. 启动 Management Service

```bash
go run ./cmd/pairroom service --mock
```

PairRoom 默认只监听 loopback。若没有自动打开浏览器，终端会显示 Management Shell 地址。精确选项以命令自身为准：

```bash
go run ./cmd/pairroom service --help
```

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
- 阶段完成：归档 Room；归档会先停止当前 Agent Turn；
- 确定不再需要：按 UI / API 的永久删除流程处理，并先保留备份。

下一步阅读 [Configuration](CONFIGURATION.md) 和 [Operations](OPERATIONS.md)。
