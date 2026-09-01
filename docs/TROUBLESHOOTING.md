# PairRoom 排障手册

> [文档首页](README.md) · [CLI 参考](CLI_REFERENCE.md) · [运维](OPERATIONS.md) · [支持范围](../SUPPORT.md)

排障原则是先区分 PairRoom 控制面、Room 数据、Vendor Runtime、浏览器认证和仓库 Workspace。不要一开始就删除状态目录或重建 Binding。

## 先执行这组最小诊断

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/project --json
pairroom daemon status
pairroom daemon logs -n 200
```

已知 Room 数据目录时再执行：

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom diagnostics \
  --data-dir /absolute/path/to/room \
  --output pairroom-diagnostics.tar.gz
```

`doctor` 失败不影响 `--mock`。先用 Mock 判断问题属于 PairRoom 控制面/UI，还是 Vendor CLI/账号/网络。

## `pairroom: command not found`

确认安装位置：

```bash
go env GOBIN
go env GOPATH
find "$(go env GOPATH)/bin" -maxdepth 1 -name 'pairroom*' -print
```

重新安装到已在 `PATH` 的目录：

```bash
make install GOBIN="$HOME/.local/bin"
command -v pairroom
```

Windows PowerShell：

```powershell
Get-Command pairroom -ErrorAction SilentlyContinue
$env:Path += ";$(go env GOPATH)\bin"
```

## `doctor` 找不到 Claude 或 Codex

1. 在同一个 shell 运行 `claude --version`、`codex --version`；
2. 确认官方 CLI 已登录；
3. daemon 场景检查安装时捕获的 PATH 是否包含两者；
4. 使用绝对命令路径验证：

```bash
pairroom doctor \
  --repo /absolute/path/to/project \
  --claude-command /absolute/path/to/claude \
  --codex-command /absolute/path/to/codex
```

前台可用而 daemon 不可用，通常是服务定义的 PATH/代理环境过期。用 `daemon install --force` 重新提交完整定义，而不是只 `restart`。

## Vendor CLI 存在，但协议检查失败

PairRoom 跟随当前官方结构化接口，不解析终端 TUI。按顺序处理：

1. 升级官方 Claude Code/Codex；
2. 直接运行各 CLI，确认登录和基本命令可用；
3. 再运行 `doctor --json`；
4. 在非关键仓库复现一个最小真实 Turn；
5. 同时用 `service --mock` 验证 PairRoom UI/状态机没有同样失败。

不要通过修改 PairRoom 数据或跳过握手来“修复”协议问题。未知高权限请求必须继续 fail closed。

## 浏览器没有自动打开

使用 `--no-browser` 时这是预期行为。后台 daemon 应运行 `pairroom daemon open`；前台模式从终端复制完整 URL：

```bash
pairroom service --no-browser
```

确认本机有可用默认浏览器，并检查 terminal 输出中的 Management URL。不要只复制去掉 fragment 的 URL。

## Management Shell 显示“缺少 Token”或 401

Management Shell 的 bootstrap Token 只在启动 URL fragment 中出现一次，读入后立即换成当前 Service 的 HttpOnly Session Cookie；CSRF Token 只在页面内存中。刷新可恢复仍有效的会话，但 Service 重启、会话过期、注销、Cookie 被清理或新浏览器上下文会导致 401。

处理方式：

1. 已安装 daemon 先运行 `pairroom daemon open`，前台模式回到 `pairroom service` 启动输出或 daemon 日志；
2. 重新打开完整 Management URL；
3. 不要把 URL 发到 Issue 或聊天；
4. 若自定义 `--token`，确认打开的是该进程打印的 URL，而不是旧书签。

Management API 接受 `Authorization: Bearer ...` 或有效 browser session；Session 认证的 mutation 还需要 `X-PairRoom-CSRF`，query-string `?token=` 始终会被拒绝。

## Room View 显示 unauthorized / CSRF 错误

Room View 使用 fragment bootstrap 交换 HttpOnly Session Cookie，写操作需要 CSRF Token。

1. 从 Management Shell 重新点击“打开”获取当前 Room URL；
2. 不要复用另一个 Room 的 URL、Cookie、附件 ID 或 SSE cursor；
3. 清理该 origin 的站点数据后重新打开完整 URL；
4. 确认浏览器没有阻止同站 Cookie；
5. 检查系统时间是否严重错误。

若只读请求成功而写操作 403，优先检查页面是否来自当前进程、CSRF 会话是否过期、Origin/Host 是否被代理改写。

## listener 被拒绝

以下值会被拒绝：

```text
0.0.0.0:7332
192.168.1.20:7332
localhost:7332
my-hostname:7332
```

使用：

```bash
pairroom service --listen 127.0.0.1:7332
pairroom service --listen '[::1]:7332'
```

远程机器通过 SSH：

```bash
ssh -L 7332:127.0.0.1:7332 host-running-pairroom
```

不要为了“方便”把 listener 改成公网或 LAN 地址。

## `address already in use`

查找占用端口的进程：

```bash
lsof -nP -iTCP:7332 -sTCP:LISTEN
# 或
ss -ltnp | grep ':7332'
```

可能原因：

- 已有前台 Service；
- 已安装 daemon 正在运行；
- 另一个应用占用端口；
- 崩溃进程尚未真正退出。

先执行 `pairroom daemon status`。确认不是同一数据根的旧 Service 后，可以选择其他 loopback 端口：

```bash
pairroom service --listen 127.0.0.1:7333
```

## Project 登记失败

Management Shell 只接受绝对 Git worktree 路径。检查：

```bash
cd /absolute/path/to/project
git rev-parse --show-toplevel
git status --short
```

常见原因：

- 使用了相对路径；
- 目录不是 Git worktree；
- symlink 目标不可访问；
- worktree 已经通过等价路径登记；
- daemon 用户/当前用户对路径权限不同；
- 可移动盘或网络盘已离线。

Service 不会自动扫描或替你创建 Git 仓库。

## Room 无法激活，显示 pending Binding

Legacy Room 缺少 Claude Session ID 或 Codex Thread ID 时会阻止激活。打开 Project 详情，使用专门的 Binding completion 操作：

- 选择 `existing`：必须提供可精确恢复且未被其他 Room 占用的 ID；
- 选择 `new`：记录 deferred Binding，在首个真实输入后 materialize。

已持久化的 Binding 不能被替换。不要手工编辑 Event Log 或 Registry 伪造 ID。

## Room 长时间 queued

在 Management Shell 的 Runtimes 页面查看：

- `runtime_limit`；
- `runtime_capacity_used`；
- 哪些 Runtime `busy`；
- queue position；
- 是否有 `failed` 且仍 `occupies_capacity` 的 Runtime。

所有 slot 都有活动 Turn 时，排队是预期行为；PairRoom 不会为释放容量中断 Turn。可以：

1. 等待活动 Turn 完成；
2. 安全挂起 idle Runtime；
3. 停止并以更高 `--runtime-limit` 重启 Service；
4. 后台定义用 `daemon install --force` 重新提交完整参数。

## 手动挂起返回 `409 Conflict`

以下状态不能安全挂起：

- Runtime 正在执行 Turn；
- 正在 starting/stopping；
- failed 但清理结果不确定、实例仍占用容量。

409 是保护行为，不应通过强删目录或 Registry 绕过。先处理活动 Turn 或查看日志中的 cleanup diagnostic。

## `service.lock` 已存在

一个数据根只允许一个 Service owner。普通启动不会猜测 lock 是否 stale。

1. 检查 daemon 和进程；
2. 确认没有旧 Service 仍在运行；
3. 确认使用的是预期 `--data-root`；
4. 只有在旧进程确实消失后执行：

```bash
pairroom service --recover-stale-lock
# 或后台定义
pairroom daemon start --recover-stale-lock
```

在不确定时保留 lock 并先收集日志；错误删除可能让两个进程同时写同一 Registry/Room。

## Reviewer snapshot 创建失败

检查目标仓库：

```bash
git rev-parse HEAD
git diff --binary HEAD
git status --short
```

常见原因：

- 没有可解析 HEAD；
- dirty patch 无法应用到 detached snapshot；
- untracked symlink；
- symlink 逃逸；
- 文件权限或磁盘空间问题；
- 安全边界无法被证明。

PairRoom 不会静默退回 live writable tree。修复仓库状态或使用受控的独立 worktree，不要关闭失败保护。

## Agent 显示 working，但没有新事件

1. 查看 Inspector 的最后 Runtime event；
2. 查看是否有待审批；
3. 检查 daemon 日志；
4. 直接确认供应商 CLI/网络/账号；
5. 只中断有问题的 participant；
6. 必要时在安全边界重启该 Runtime。

`--stall-warning-seconds` 只产生提醒，不会自动判断模型失败或终止 Turn。长命令、网络等待和供应商端延迟都可能暂时没有事件。

## 消息状态看起来矛盾

先区分 Delivery 与 Processing：

- Delivery `started` + Processing `working`：输入已进入新 Turn，仍在处理；
- Delivery `queued` + Processing `waiting`：等待安全边界；
- Delivery 成功 + Processing `failed`：Harness 接受过输入，但执行失败；
- 一个目标 completed、另一个 failed：只重试失败目标；
- `superseded`：被明确新指令取代，不是数据丢失。

不要只看聊天气泡是否出现最终回复。

### PairRoom 重启后，Room FIFO 中的消息没有自动继续

这是预期的 fail-closed 行为。Room 级 Turn FIFO 不做跨进程命令重放；重启时尚未进入原生 Runtime 的消息会显示 `Delivery=skipped`、`Processing=cancelled`，详情包含 restart 原因。这样可以避免在提交边界不确定时重复执行写操作。

处理方式：

1. 检查消息是否确实未完成，以及仓库中是否已有部分副作用；
2. 查看 Inspector、Git diff 和 vendor 原生记录；
3. 使用该消息上的 **重试** 创建新的可审计消息，不要手工把旧状态改回 queued。

取消同样区分边界：`Delivery=pending` 的 Room FIFO 项只取消自身；已经 `started`/`injected`/Adapter `queued` 的输入可能按整个 native Turn 取消，但不会删除尚未提交的 Room FIFO 后续项。

## `verify` 失败

立即：

1. 停止对该 Room 的写入；
2. 保留完整 Room 目录；
3. 复制到只读证据位置；
4. 记录 `version --json`；
5. 生成脱敏 diagnostics（若命令仍能安全完成）；
6. 找到最近一次 verified backup。

不要手工修改 sequence、schema、manifest 或 hash。中间 Event Log 损坏不会被自动跳过；只有损坏的最后半行有受控恢复路径。

## `restore` 拒绝归档或目标

常见拒绝：

- 目标非空且没有 `--force`；
- archive traversal/absolute path/link；
- 重复或未在 manifest 声明的文件；
- 文件数量/大小超限；
- SHA-256 不匹配；
- Room metadata/Event Log 验证失败。

优先恢复到一个全新目录：

```bash
pairroom restore \
  --input room-backup.tar.gz \
  --data-dir /new/empty/path
pairroom verify --data-dir /new/empty/path
```

不要对验证失败的归档使用外部解压后手工拼接数据。

## daemon 配置修改没有生效

`pairroom daemon restart` 只重启现有服务定义。更改 Runtime limit、idle timeout、data root、token、代理、Agent 命令或日志设置时：

```bash
pairroom daemon install --force -- \
  --data-root /absolute/path/to/pairroom-data \
  --runtime-limit 4 \
  --idle-timeout 20m
```

先从已有服务定义/运维记录恢复全部参数。只复制局部示例可能意外切换数据根或丢失 Token、代理和 Agent 配置。

## 收集可分享的 Issue 信息

建议包含：

```text
OS / architecture
pairroom version --json
当前官方 Claude Code/Codex 版本
service 还是 serve；foreground 还是 daemon
是否在 --mock 复现
精确动作与 Delivery/Processing/Runtime phase
最小公开仓库或合成复现
已脱敏日志片段
```

不要公开：启动 URL、Bearer Token、Cookie、CSRF、私有 prompt、源代码、真实附件、完整 Event Log、审批 payload 或含凭据的代理 URL。诊断包默认脱敏，但分享前仍要人工检查。
