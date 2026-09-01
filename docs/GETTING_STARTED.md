# PairRoom 快速上手

本指南只覆盖“从零到完成第一个协作流程”。日常操作、全部配置和运维分别见 [User Guide](USER_GUIDE.md)、[Configuration](CONFIGURATION.md) 与 [Operations](OPERATIONS.md)。

## 1. 前置条件

- 一个本地 Git worktree；
- 与 `go.mod` 匹配的 Go toolchain；
- 浏览器可访问本机 loopback；
- 真实模式下可独立运行并已登录的 `claude` 与 `codex` CLI。

首次使用建议先跑 Mock。它使用同一套 Management、Room、Event Log、调度和恢复路径，但不会调用模型。

## 2. 构建或安装

```bash
make build
./dist/pairroom version
```

安装到 Go binary 目录：

```bash
make install
pairroom version
```

`make install` 不修改 `PATH`；输出会显示实际安装位置。

## 3. 启动 Mock Service

```bash
./dist/pairroom service --mock
```

未自动打开浏览器时，使用终端打印的 Management URL。Service 默认监听 `127.0.0.1:7332`，数据根位于 `os.UserConfigDir()/pairroom`；可用 `--listen` 与绝对 `--data-root` 覆盖。

## 4. 登记 Project

在 Management Shell 的 Projects 页面输入 Git worktree 的绝对路径。Service 会解析 symlink、定位 Git worktree root、canonicalize 并去重。

Project 是注册记录，不是仓库副本。注销空 Project 不会删除 Git worktree。

## 5. 创建 Room

在 Project detail 中创建 Room。Claude 与 Codex 可分别选择：

- `new`：首次 native input 被接受后 materialize Session/Thread ID；
- `existing`：创建时提供并精确验证现有 Session/Thread ID。

首次体验使用 `new/new`。Room 默认 Claude 为 Driver、Codex 为 Reviewer；角色可以在安全 Turn 边界调整。

## 6. 完成第一个 Turn

打开 Room，给当前 Driver 一个小型只读任务：

```text
阅读当前仓库，说明构建与测试入口，不要修改文件。
```

Timeline 显示输入和最终回答；Inspector 显示 Delivery、Processing、native Turn、工具和审批。不要把 `HTTP 202` 或 Delivery `started` 当作任务完成，以 Processing / Turn terminal 状态为准。

## 7. 验证确定性接力

发送：

```text
Claude 先规划；Codex 独立审查；等我批准后再执行。
```

预期：

```text
Claude native Turn
  -> reliable terminal boundary
  -> Codex native Turn
  -> WAIT / human decision
```

两个 Runtime 不会同时拥有 active Turn。普通 `@peer` 文本不会自动投递；Agent 自动交棒需要有效 `HANDOFF` 与 `NEXT`。

## 8. 批准或改变计划

计划审查后可以发送明确批准，例如：

```text
批准，按当前计划执行。
```

若计划内容发生变化，旧批准自动失效，需要重新批准。也可以拒绝、补充约束或用新的多阶段描述替换当前 Workflow。

## 9. Steering、下一 Turn 与取消

- 发给当前 owner 的 `append` 通常进入当前 native Turn；
- 发给另一 Agent或使用 `next_turn` 的输入进入 Room FIFO；
- FIFO 中未提交的消息可精确取消；
- native Runtime 已接受输入后，中断粒度可能扩大到当前 Agent Turn；
- Retry 创建新消息并引用旧消息，不修改历史。

Service 重启不会自动重放 Room FIFO。重启后先检查工作区副作用，再显式 Retry。

## 10. 切换到真实 Runtime

先运行：

```bash
pairroom doctor --repo /absolute/path/to/project
pairroom providers --config /absolute/path/to/pairroom.json
```

然后去掉 `--mock`：

```bash
pairroom service --config /absolute/path/to/pairroom.json
```

`doctor` 不创建模型 Turn，也不能证明账号、网络或供应商服务当前可用。第一次真实协作使用非关键仓库、只读任务和最小权限。

## 11. 后台运行与收尾

```bash
pairroom daemon install --runtime-limit 4 --idle-timeout 20m
pairroom daemon open
```

暂时不用 Room 时可让 idle policy 回收 Runtime；阶段完成后归档 Room。永久删除只处理已归档且 Runtime 可证明停止的 PairRoom 数据，不删除 Git worktree 或 Vendor Session/Thread。

下一步：

- [日常使用](USER_GUIDE.md)
- [核心概念](CONCEPTS.md)
- [配置](CONFIGURATION.md)
- [运维](OPERATIONS.md)
