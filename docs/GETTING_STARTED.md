# PairRoom 快速上手

> [文档首页](README.md) · [核心概念](CONCEPTS.md) · [CLI 参考](CLI_REFERENCE.md) · [排障](TROUBLESHOOTING.md)

本教程先用 Mock Agent 跑通完整产品路径，再切换到真实 Claude Code 与 Codex。完成后你会拥有一个可重复打开的 Project、一个持久化 Room，以及清晰的 Driver/Reviewer 协作方式。

## 1. 选择运行方式

| 目标 | 命令 |
|---|---|
| 体验多 Project / 多 Room | `pairroom service --mock` |
| 日常真实协作 | `pairroom service` |
| 后台常驻 | `pairroom daemon install ...` |
| 只为一个仓库快速开房间 | `pairroom serve --repo ...` |

新用户优先使用 `service`。`serve` 是兼容快捷入口，不提供 Project Registry、多个 Room、Runtime 容量和 Management Shell。

## 2. 前置条件

### 仅使用 Mock

- Git；
- 一个可访问的 Git worktree；
- 从源码构建时需要 Go 1.23+；
- 源码构建需要 `make`；Windows 请使用已安装 `make` 的 POSIX shell（例如 MSYS2），或直接执行下文的 `go build` 命令。

### 使用真实 Agent

除上述条件外，还需要：

- 官方 Claude Code CLI，可执行命令默认是 `claude`；
- 官方 Codex CLI，可执行命令默认是 `codex`，并提供 `codex app-server`；
- 两个 CLI 已分别按供应商官方流程完成登录；
- 当前网络和账号可以完成真实 Turn。

PairRoom 不保存供应商 API Key，也不替代官方登录流程。

## 3. 构建与安装

### 构建仓库内二进制

```bash
git clone https://github.com/sean2077/pairroom.git
cd pairroom
make build
./dist/pairroom version --json
```

### 安装为全局命令

```bash
make install
pairroom version
```

安装目录优先使用显式 `GOBIN`，否则使用 Go 默认的 `$(go env GOPATH)/bin`。目标目录不在 `PATH` 时，可以直接指定：

```bash
make install GOBIN="$HOME/.local/bin"
```

Windows PowerShell 可通过 Git Bash 调用 Makefile：

```powershell
& 'C:\Program Files\Git\bin\bash.exe' -lc 'make build'
.\dist\pairroom.exe version --json
```

## 4. 第一次 Mock Service

从任意目录启动：

```bash
pairroom service --mock
```

没有安装全局命令时使用：

```bash
./dist/pairroom service --mock
```

终端会打印 Management Shell URL、数据根、Runtime 容量和运行模式，并尝试打开浏览器。使用 `--no-browser` 可只打印 URL。

### 4.1 登记 Project

在 Management Shell 选择“登记 Project”，输入 Git worktree 的绝对路径。

Service 会解析符号链接、定位 Git worktree root、canonicalize 并去重。以下输入最终指向同一个 worktree 时只会登记一次：

```text
/path/to/repo
/path/to/repo/subdirectory
/symlink/to/repo
```

Management Shell 不扫描磁盘，也不提供服务器路径浏览器；Project 必须由用户显式登记。

### 4.2 创建 Room

在 Project 详情页创建 Room：

- Room name：例如 `auth-refactor`；
- Claude Binding：选择 `new`；
- Codex Binding：选择 `new`。

`new` Binding 在第一个被对应官方 Runtime 接受的真实输入上 materialize。Mock 模式会模拟同一生命周期，因此适合验证页面、队列和状态，而不是证明 Vendor 协议可用。

### 4.3 打开 Room

点击“打开”。Service 会按 Runtime 容量惰性启动 Room，并返回一个独立的 loopback Room URL。浏览器切换页面或关闭 Management Shell 不会中断正在运行的 Turn。

### 4.4 发送第一条消息

```text
@all 请分别独立分析这个仓库的结构、风险和可改进点。先讨论，不修改文件。
```

观察三个区域：

1. **公共时间线**：你和两个 Agent 的公开结论；
2. **参与者状态**：Runtime、Session/Thread、角色与当前 Turn；
3. **Work Inspector**：工具、命令、计划、Diff、审批、错误和消息关联。

尝试引用回复、线程聚焦、图片上传和 `@claude` / `@codex` 单独指令。

### 4.5 验证路由

默认 `mentions` 模式下，只有 Agent 最终回复显式提到 Peer 才自动继续。可以在 Room 设置或启动参数中尝试：

```bash
pairroom service --mock --routing manual
pairroom service --mock --routing roundtable --max-hops 6
```

用户新消息总是比旧自动接力优先。

## 5. 切换到真实 Runtime

先在目标仓库执行：

```bash
pairroom doctor --repo /absolute/path/to/project
```

机器可读输出：

```bash
pairroom doctor --repo /absolute/path/to/project --json
```

检查通过后停止 Mock Service，再启动真实模式：

```bash
pairroom service
```

创建新的真实 Room，或为 Claude/Codex 分别选择已有 Session/Thread ID。`existing` 只恢复供应商原生上下文；PairRoom 不读取或导入绑定前 Transcript，公共时间线从绑定成功后开始。

### 建议的首次真实协作

让 Reviewer 保持只读，先独立分析：

```text
@all 请独立阅读任务和相关代码。Claude 负责提出最小实现方案；Codex 负责查找并发、恢复、安全和测试遗漏。没有共识前不要修改代码。
```

确认后：

```text
@claude 作为 Driver 实现已确认方案并运行测试。完成后 @codex 只读审查完整 diff，并列出必须修复项和建议项。
```

真实模式首次使用应选择非关键仓库，并人工检查审批、命令、文件范围和 Diff。

## 6. 后台常驻

安装并启动同一个多 Room Service：

```bash
pairroom daemon install --runtime-limit 4 --idle-timeout 20m
pairroom daemon open
pairroom daemon status
pairroom daemon logs -f
```

常用生命周期：

```bash
pairroom daemon stop
pairroom daemon start
pairroom daemon restart
pairroom daemon uninstall
```

`daemon install` 将未知安装参数转发给 `pairroom service`，自动加入 `--no-browser`，并固定二进制路径、工作目录、PATH、代理环境和日志路径。`daemon open` 会从当前/轮转日志验证 Management URL 后打开默认浏览器。使用 `--` 可显式分隔 daemon 与 Service 参数：

```bash
pairroom daemon install \
  --log-max-size 20M \
  --log-max-backups 5 \
  -- \
  --runtime-limit 4 \
  --data-root /absolute/path/to/pairroom-data
```

修改定义要使用 `--force` 并重新提供完整参数；`daemon restart` 不修改定义。

## 7. 单 Room 快捷模式

不需要 Registry 和 Management Shell 时：

```bash
pairroom serve --repo /absolute/path/to/project --mock
```

真实模式：

```bash
pairroom doctor --repo /absolute/path/to/project
pairroom serve --repo /absolute/path/to/project
```

单 Room 默认数据目录由清理后的仓库绝对路径计算。可用 `--data-dir` 显式指定。

## 8. 安全检查

首次使用至少确认：

- URL 监听地址是 `127.0.0.1` 或 `[::1]`；
- 没有把启动 URL、Bearer Token、Cookie 或 CSRF Token 发到 Issue/聊天；
- Reviewer 的 Workspace Boundary 显示 snapshot，而不是 live writable tree；
- 真实 Agent 的命令、写入和额外权限审批符合预期；
- 私有截图和代码可以按供应商政策发送；
- 远程访问通过 SSH tunnel，不直接暴露 PairRoom HTTP。

## 9. 下一步

- [核心概念](CONCEPTS.md)：先建立正确心智模型；
- [Multi-Room Service](MULTI_ROOM_SERVICE.md)：理解 Binding、容量、归档和恢复；
- [CLI 参考](CLI_REFERENCE.md)：查看全部选项；
- [运维手册](OPERATIONS.md)：后台服务、备份与升级；
- [排障手册](TROUBLESHOOTING.md)：解决启动、认证、Runtime 和数据问题。
