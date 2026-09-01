# PairRoom 排障手册

从最小证据开始，不要先删除数据、延长超时或重启两个 Vendor CLI。

```bash
pairroom version --json
pairroom doctor --repo /absolute/path/to/project --json
pairroom daemon status        # 使用 daemon 时
pairroom daemon logs -n 200
```

Room 数据问题再执行：

```bash
pairroom verify --data-dir /absolute/path/to/room --json
pairroom diagnostics --data-dir /absolute/path/to/room --output diagnostics.tar.gz
```

## `pairroom: command not found`

```bash
make install
```

读取输出中的安装路径；`make install` 不修改 `PATH`。也可直接使用 `./dist/pairroom`。Windows 注意当前 shell 与安装 Go toolchain 的 `GOBIN/GOPATH` 是否一致。

## `make check` 提示 CGO / C compiler

Race detector 需要 `CGO_ENABLED=1` 和可用 C 编译器：

```bash
go env CGO_ENABLED
cc --version
```

Windows 可临时使用 MSYS2 UCRT64/MinGW64 GCC，不必修改用于静态发布构建的全局设置。详见 [Contributing](../CONTRIBUTING.md)。

## Doctor 找不到 Claude 或 Codex

先在同一用户、同一 PATH、同一工作目录独立运行：

```bash
claude --version
codex --version
```

若 daemon 找不到而交互 shell 能找到，重新安装 daemon 以固化正确 executable / PATH，或在配置中使用明确 `command`。不要把 alias 或 shell function 当作 daemon 可执行文件。

## Vendor CLI 存在但协议检查失败

记录 Vendor 版本与 `doctor --json`。PairRoom 只依赖公开结构化协议，不解析 TUI 文本；CLI 能交互运行不等于 app-server / stream-json 协议仍兼容。

升级或回退 Vendor CLI，在非关键仓库完成 native smoke。不要简单延长 timeout 掩盖 initialize/schema 错误。见 [Runtime Compatibility](RUNTIME_COMPATIBILITY.md)。

## Service 无法启动

依次检查：

1. `--listen` 是否为 numeric loopback；
2. 端口是否占用；
3. `--data-root` 是否绝对、可写、不是 symlink 文件；
4. JSON 配置是否有未知字段或旧 routing mode；
5. `service.lock` 是否由活跃进程持有；
6. Registry 是否 fail closed。

```bash
# Linux/macOS 示例
lsof -iTCP:7332 -sTCP:LISTEN
```

不要通过改成 `0.0.0.0` 绕过 listener 错误。

## `service.lock` 已存在

先检查 `pairroom daemon status`、进程列表和日志。确认旧进程已经消失后，才使用 `--recover-stale-lock`。若无法确认，先保留 data root 副本；两个写者比暂时无法启动更危险。

## Management Shell 401 或“缺少 Token”

- 使用 Service 打印或 `daemon open` 提供的完整 URL；
- Token 应在 URL fragment，不是 query string；
- 不要把 Room URL 当成 Management URL；
- 浏览器禁用 cookie、跨 origin 代理或旧 Session 都可能导致失败；
- Logout 后重新 bootstrap。

API 客户端直接发送正确作用域的 Bearer token。Management 和 Room token/session 不能互换。

## Room View unauthorized / CSRF

重新从 Management Shell 打开 Room，避免复用另一个 Room 的 URL。Mutating browser request 需要当前 Room Session 的 CSRF；Bearer 客户端不使用浏览器 CSRF。检查代理是否改写 Origin、Host、Cookie 或 `X-PairRoom-CSRF`。

## Project 登记失败

- 路径必须存在且属于 Git worktree；
- 使用绝对路径；
- symlink/子目录会归一到 canonical root，可能已登记；
- 同一仓库的不同 worktree 是不同 Project；
- path 暂时不可用时，已有 Project 保留但显示 unavailable。

恢复挂载后使用 Refresh，不要重复登记等价路径。

## Room 创建或激活显示 pending Binding

`new` Binding 直到首个 native input 接受后才 materialize；`existing` 必须提供可精确 resume 的 ID。若旧 Room 缺少 Binding，在 Management Shell 补全；操作会等待并挂起 Runtime。

检查 Binding ownership conflict：同一 Vendor ID 不能属于两个 Room，归档也不会释放。

## Room 长时间 queued

查看 Management Runtime capacity：

- 是否达到 `runtime-limit`；
- 是否有 busy Turn；
- 是否有 failed/cleanup-retained Runtime 占容量；
- idle timeout 是否尚未到；
- Room 是否 archived / pending Binding / Project unavailable。

不要通过手工杀进程或删除 Runtime 目录“释放容量”。先安全挂起空闲 Room或修复 failed cleanup。

## Agent 显示 working 但没有新事件

`stall_warning_seconds` 只表示一段时间未收到 Runtime event，不证明死锁。区分：

- 正常长命令或模型思考；
- 等待可见 Approval / user decision；
- Vendor 仍运行但只发 generic diagnostic error；
- stdout/JSON-RPC connection 断开；
- process 已退出；
- 真正未响应。

查看 Activity、Approval、进程状态和 Vendor 日志。普通 `RuntimeError` 不是 terminal boundary；只有 completion、明确 cancel/abort 或确认 process exit 才释放 owner。

## Peer 在当前 Agent 完成前启动

这是 single-owner 不变量违规。收集完整 Runtime event 顺序、Turn ID、Message ID 和 Vendor 版本。重点确认是否把中途 generic error 错当 completion。不要只提交 UI 截图。

## FIFO 消息重启后没有继续

这是预期的 fail-closed 行为。Room FIFO 在内存中，不自动持久化/重放。重启后：

1. 检查 Git 状态、外部命令与服务副作用；
2. 查看 Message Processing 结算；
3. 对需要继续的输入显式 Retry。

不要通过直接重写 Event Log 改成 pending。

## 取消一条消息影响了 active Turn

若消息仍在 Room FIFO，只移除该项；若 native Runtime 已接受它，Vendor 中断粒度可能是整个当前 Agent Turn。无关 Room FIFO 应保留。记录取消时的 Delivery/Processing/Turn 状态，用于判断是否是预期扩大或 bug。

## Rename / Suspend 返回冲突

Rename 会等待 safe Turn boundary；普通 Suspend 不抢占 busy Turn。Archive 的语义不同：它会主动 interrupt active Turn、settle 后归档。选择与意图一致的操作，不要把 Archive 当普通页面关闭。

## Archive 或 Delete 失败

Archive 失败时 lifecycle 不应伪装成 archived。Permanent delete 要求：

- Room 已 archived；
- Runtime 可证明停止；
- `acknowledge_data_loss=true`；
- 数据目录满足安全删除条件。

逻辑删除 committed 后物理清理失败会显示 pending cleanup；使用 maintenance retry，不要手工删除 quarantine intent/marker。

## `verify` 失败

先复制整个 Room 目录并停止写入。常见原因：metadata/schema、Event sequence、unterminated/corrupt line、Room ID mismatch、附件 manifest/bytes/hash 缺失。

`verify` 不修复。不要编辑 sequence、删除中间行或修改 hash。仅启动打开路径能安全处理最后半行；其他损坏应从已验证备份恢复或保留 forensic 副本后人工分析。

## `restore` 拒绝归档或目标

Restore 会拒绝 traversal、link、duplicate path、超限、checksum mismatch 和未经允许的非空目标。不要解包后手工移动来绕过检查。恢复到新目录，先 `verify`，再 import。

## daemon 修改配置没有生效

`daemon restart` 只重启已安装定义。若改变 install 参数，执行：

```bash
pairroom daemon install --force -- ...
```

若仅修改被 daemon 引用的 JSON 文件，确认路径和权限后 restart。用 `daemon status` / logs 核对实际 binary 和参数。

## 收集可分享证据

Issue 中提供最小步骤、版本、运行方式、Mock 对照、结构化错误和 diagnostics。删除 Token、Session/Thread ID、私有 path、Transcript、附件 bytes 与 API key。支持范围见 [SUPPORT](../SUPPORT.md)。
