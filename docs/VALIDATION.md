# PairRoom v0.2.0 验证记录

验证日期：2026-08-13

## 1. 验证范围

v0.2.0 的验证重点是：

- Delivery 与 Processing 两层消息生命周期。
- Claude Code / Codex 结构化协议关联与降级路径。
- Codex 同一 Turn 的多次介入与完成结算。
- Runtime 失败、停止、重启和进程退出后的状态收口。
- 可审计重试、导出和搜索所依赖的服务端状态。
- JSONL 崩溃尾行修复、schema 升级与恢复。
- Web/API 安全边界、SSE cursor 与 DNS rebinding 防护。
- Mock 三方讨论的完整端到端运行和强制终止恢复。
- 四个平台的静态交叉编译。

## 2. 静态与自动测试

执行：

```bash
gofmt -w $(find . -name '*.go' -type f)
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
node --check internal/server/assets/app.js
git diff --check
```

结果：全部通过。

当前共有 **56 个 Go 测试**：

```text
agent/claude     4
agent/codex     11
agent/mock       1
agent/probe      5
config           4
prompt           3
room            15
server           7
store            6
```

关键回归覆盖：

- Claude stream-json 输入 envelope、初始化 RuntimeInfo、结果/错误结算和未完成输入退出收口。
- 每次 Claude Submit 只写入一条 stream-json 输入并只进入内部队列一次。
- Codex `turn/start` / `turn/steer` 使用 `clientUserMessageId`，并通过 `userMessage.clientId` 关联通知。
- 同一 Codex Turn 接受多个用户介入后，所有关联输入分别进入 terminal ProcessingState。
- Codex command/file/permission approval 与未知 server request fail-closed。
- Delivery 与 Processing 状态分离、终态保护和 Submit 前失败收口。
- Runtime failure 不覆盖已被 Runtime 接受的 transport delivery。
- retry 创建新的 `retry_of` 消息，不改写原消息。
- stop/restart 取消 in-flight processing 并让连接级审批过期。
- JSONL sequence、metadata schema、未来版本拒绝和半行尾部修复。
- API health/message/snapshot/retry/export/SSE cursor。
- Bearer Token、同源检查和 tokenless loopback Host 限制。

## 3. 测试覆盖率快照

```text
internal/agent   39.5%
internal/config  80.0%
internal/prompt  84.8%
internal/room    70.6%
internal/server  52.5%
internal/store   69.2%
```

CLI 启动壳层、浏览器打开逻辑和少量纯数据辅助包当前主要由端到端测试或调用路径覆盖；后续应继续提高 Adapter 异常分支和命令级测试覆盖率。

## 4. Mock 三方讨论端到端

使用本次源码构建的 Linux 二进制启动临时 Git 仓库和独立 data directory：

```bash
pairroom serve \
  --repo <temporary-repo> \
  --data-dir <temporary-data> \
  --listen 127.0.0.1:<free-port> \
  --mock \
  --no-browser
```

执行步骤：

1. 等待 `/api/v1/health` 返回 200。
2. 向 Claude 与 Codex 同时发送一条中文 `@all` 消息。
3. 等待双方首轮完成并通过 mention 自动接力。
4. 等到两个参与者均回到 `idle`，所有 Agent 目标的 ProcessingState 均进入终态。
5. 导出 Markdown、普通 JSON 和 forensic JSON。
6. 检查 metadata schema、完整事件日志和 Git Inspector。
7. 正常停止 daemon。

观测结果：

```json
{
  "version": "0.2.0",
  "messages": 5,
  "latest_seq": 87,
  "initial_delivery": {
    "claude": "started",
    "codex": "started"
  },
  "initial_processing": {
    "claude": "completed",
    "codex": "completed"
  },
  "participants": {
    "claude": {
      "state": "idle",
      "role": "driver",
      "runtime": "pairroom-mock",
      "runtime_version": "0.2.0"
    },
    "codex": {
      "state": "idle",
      "role": "reviewer",
      "runtime": "pairroom-mock",
      "runtime_version": "0.2.0"
    }
  },
  "open_processing": 0,
  "event_log_lines": 87,
  "metadata_schema": 2,
  "transcript_lines": 44,
  "json_export_messages": 5,
  "normal_json_has_events": false,
  "forensic_json_events": 87,
  "git_status_ok": true
}
```

原始结果：[`validation/v0.2.0-mock-e2e.json`](validation/v0.2.0-mock-e2e.json)。

## 5. 强制终止与恢复端到端

在 Mock Agent 正处于 `working` 时强制终止 PairRoom 进程，然后使用同一 data directory、`--auto-start=false` 重新启动。

结果：

```json
{
  "before": {
    "delivery": "started",
    "processing": "working",
    "participant": "working"
  },
  "after": {
    "delivery": "started",
    "processing": "cancelled",
    "detail": "PairRoom restarted before the native runtime reported completion",
    "participant": "stopped"
  }
}
```

这验证了：已经进入 Harness 的 transport disposition 会保留，但无法确认完成的执行状态会在恢复时明确取消，不会永久显示为 Working。

原始结果：[`validation/v0.2.0-restart-e2e.json`](validation/v0.2.0-restart-e2e.json)。

## 6. Web UI 静态契约

执行 JavaScript 语法检查，并将 `app.js` 中所有静态 `$('<id>')` 引用与 `index.html` 对照：

```json
{
  "html_ids": 36,
  "duplicate_ids": [],
  "js_static_id_refs": 35,
  "missing_js_ids": []
}
```

服务端 API 测试同时覆盖内嵌 asset、snapshot、SSE、消息、重试、导出和参与者操作接口。本轮未把像素级截图比较作为发布门禁；浏览器视觉回归仍应在可运行 Chromium/Playwright 的桌面 CI 中补齐。

## 7. 独立性

```bash
go list -m all
```

输出只有：

```text
github.com/sean2077/pairroom
```

Go 核心没有第三方 module。前端没有 npm dependency 或构建产物依赖。

## 8. Runtime doctor

当前容器执行：

```bash
pairroom doctor --repo .
pairroom doctor --repo . --json
```

观测：

- PairRoom `0.2.0`。
- OS `linux/amd64`。
- Git 可用：`git version 2.47.3`。
- `claude` 不在 `$PATH`。
- `codex` 不在 `$PATH`。
- Doctor 返回 `ok=false` 和非零退出码，并明确提示 Mock 模式仍可用。

因此：

- Room、路由、生命周期、持久化、Web/API、Git Inspector、Mock 和协议解析已经实际执行验证。
- Native Adapter 已按公开结构化协议实现并通过单元、竞态和构建检查。
- 真实账号、真实网络、当前供应商 CLI 的完整 Turn、审批、compaction 和 resume 仍需在安装两个官方 CLI 的开发机验证。

## 9. 跨平台构建

以下目标均以 `CGO_ENABLED=0` 完成交叉编译：

```text
linux/amd64       ELF 64-bit, statically linked
windows/amd64     PE32+ console executable
macOS arm64       Mach-O 64-bit arm64
macOS amd64       Mach-O 64-bit x86_64
```

Linux 产物执行 `pairroom version` 返回：

```text
pairroom 0.2.0
```

二进制 SHA-256：

```text
d01c9df91a30bc7e5d7c6b29dd7fbe5e5d3e14e4bcfd9b333b0ba17fc0fc9d4c  pairroom-darwin-amd64
6541aa3dd54002c056a62e07dabcd80a4cace8d0bfaf0235fc6e4b7da7901ac1  pairroom-darwin-arm64
d5152e179760770de0fda224cc363be8966f2eda4ffe3b1d88e7f0b4818c41a3  pairroom-linux-amd64
8a281fb14adc5fdeac0b386ef35c5558e2f90bd00a2c50a8bcd4196e5a9d87b5  pairroom-windows-amd64.exe
```

## 10. 官方协议对照与可信边界

实现按 2026-08-13 的公开协议面复核：

- Claude Code 使用 print mode 的双向 stream-json、session ID 与 resume；可选 partial/subagent/hook 事件按能力协商。
- Codex App Server 使用 `initialize`、`thread/start|resume`、`turn/start|steer|interrupt`、`item/*` 和 `turn/completed`。
- `clientUserMessageId` / `userMessage.clientId` 用于输入关联；内部队列保留降级回退。
- 未知高权限 server request 默认拒绝。

本次执行环境没有安装并登录真实 Claude Code 与 Codex，因此本记录不声称完成：

- 真实账号的新建 Turn、恢复、压缩和长会话。
- 真实 Claude Skills、MCP、Hooks 与权限规则组合。
- 真实 Codex command/file/permission approval round-trip。
- 不同 CLI 版本的兼容矩阵。
- Windows 原生 sandbox 和文件权限差异。
- 真实模型在多轮自动讨论中的质量、成本和上下文稳定性。

首次实机试用应固定 CLI 版本，先运行 `pairroom doctor`，再在非关键仓库按 [`RUNTIME_COMPATIBILITY.md`](RUNTIME_COMPATIBILITY.md) 的清单验证。
