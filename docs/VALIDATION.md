# PairRoom v0.3.0 验证记录

验证日期：2026-08-13

## 1. 验证范围

v0.3.0 的验证重点从“能同时驱动两个 Agent”扩展到“能长期阅读、介入和审计三方协作”：

- 富 Markdown 与安全渲染；
- 图片选择、拖拽、剪贴板上传、持久化、预览和灯箱；
- 同一图片进入 Claude 原生 image block 与 Codex `localImage` 输入；
- Agent 最终回答引用仓库内图片时的安全导入；
- 搜索、参与者筛选、引用跳转、线程聚焦、长消息折叠和过程关联；
- Claude native control handshake、工具审批与 `AskUserQuestion`；
- Claude Reviewer plan/write deny 与 Codex Reviewer read-only policy；
- Attachment 认证、不可变哈希、路径边界和删除生命周期；
- v0.2 已有的 Delivery/Processing、重试、崩溃恢复和 JSONL 语义回归；
- 桌面与移动端真实浏览器运行；
- 四个平台静态交叉编译。

## 2. 静态与自动测试

执行：

```bash
gofmt -w $(find . -name '*.go' -type f)
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
node --check internal/server/assets/app.js
node --check internal/server/assets/richtext.js
git diff --check
go list -m all
```

结果：全部通过。

当前共有 **90 个顶层 Go 测试函数**：

```text
cmd/pairroom            2
internal/agent         37
internal/attachment     8
internal/config         4
internal/prompt         3
internal/room          19
internal/server        11
internal/store          6
```

重点覆盖：

- Claude control initialize、tool approval、`AskUserQuestion` 结构化答案、Reviewer 写请求 fail closed；
- Claude 文本 + 多图片原生内容块及附件内容变化拒绝；
- Codex text + `localImage`、`turn/start`、多次 `turn/steer`、输入关联和审批；
- Codex 活跃 Turn、队列或未决审批期间拒绝角色切换；
- 图片真实格式检测、解码、大小/边长/像素限制、SHA-256 校验；
- attachment ID/path traversal、symlink content 与 repository escape 拒绝；
- Message 只接受 canonical attachment metadata，Host path 只到 Adapter 边界；
- Agent 最终回答引用仓库图片后投影到公共消息；
- 图片上传、认证读取、ETag、未引用删除和 transcript 引用保护；
- Delivery/Processing、retry、stop/restart、approval expiry；
- Store schema 3、未来 schema 拒绝、损坏尾行修复；
- API health/snapshot/message/attachment/export/SSE/security headers。

## 3. 覆盖率快照

```text
cmd/pairroom          20.2%
internal/agent        48.5%
internal/attachment   62.1%
internal/config       80.0%
internal/prompt       75.9%
internal/room         70.7%
internal/server       62.9%
internal/store        69.2%
全项目 statement      55.8%
```

CLI 启动壳层、浏览器打开逻辑和纯数据类型仍主要由端到端路径覆盖。后续应继续提高真实 Adapter 异常事件、审批断线和 Work Inspector 结构化卡片的覆盖率。

## 4. Mock 三方讨论端到端

使用本次源码构建的 Linux 二进制，在临时 Git 仓库和独立 data directory 中启动：

```bash
pairroom serve \
  --repo <temporary-repo> \
  --data-dir <temporary-data> \
  --listen 127.0.0.1:<free-port> \
  --mock \
  --no-browser
```

执行流程：

1. 等待 health 成功；
2. 向 Claude 与 Codex 同时发送 `@all`；
3. 等待双方首轮和 mention 自动接力结束；
4. 检查所有 Agent processing 进入终态且参与者回到 idle；
5. 导出 Markdown、普通 JSON、取证 JSON；
6. 检查事件日志、Store schema 和 Git Inspector；
7. 正常停止 daemon。

结果：

```json
{
  "version": "0.3.0",
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
  "open_processing": 0,
  "event_log_lines": 87,
  "metadata_schema": 3,
  "normal_json_has_events": false,
  "forensic_json_events": 87,
  "git_status_ok": true
}
```

原始结果：[`validation/v0.3.0-mock-e2e.json`](validation/v0.3.0-mock-e2e.json)。

## 5. 富对话真实浏览器 E2E

使用系统 Chromium + Playwright 连接实际运行的 PairRoom Mock daemon，而不是静态 HTML fixture。测试页面尺寸为桌面 `1440×1000` 和移动端 `430×932`。

实际操作：

1. 通过文件选择器同时上传两张 PNG；
2. 发送包含标题、引用、表格、任务列表、Go 代码块和 Markdown 图片引用的消息；
3. 等待 Claude/Codex 产生多轮公共消息；
4. 验证图片内联、同消息画廊、灯箱前后切换和缩放；
5. 验证引用回复、消息到 Inspector 的关联筛选；
6. 验证全文搜索和 Agent/用户筛选；
7. 发送远程 Markdown 图片，确认浏览器没有自动请求；
8. 发送长消息，验证折叠和展开；
9. 捕获桌面、灯箱和移动端截图；
10. 记录 browser console/page errors 与 snapshot。

结果：

```json
{
  "pairroom_version": "0.3.0",
  "message_count": 10,
  "agent_messages": {
    "claude": 4,
    "codex": 3
  },
  "first_user_attachment_count": 2,
  "first_user_attachment_media_types": [
    "image/png",
    "image/png"
  ],
  "markdown": {
    "heading": true,
    "table": true,
    "code_block": true,
    "task_list": true,
    "inline_image": true
  },
  "lightbox": {
    "gallery_count": 2,
    "zoom_verified": true
  },
  "external_image_auto_fetched": false,
  "console_errors": [],
  "page_errors": []
}
```

证据：

- [桌面时间线](images/pairroom-v0.3-desktop.png)
- [图片灯箱](images/pairroom-v0.3-lightbox.png)
- [移动端布局](images/pairroom-v0.3-mobile.png)
- [机器可读结果](validation/v0.3.0-rich-conversation-e2e.json)

该测试证明 PairRoom 自身的浏览器、API、SSE、媒体库和 Mock Runtime 链路可以共同工作；它不证明真实供应商模型一定会正确理解任意图片。

## 6. 强制终止与恢复

在 Mock Agent 的 ProcessingState 为 `working` 时强制终止 PairRoom，然后使用同一 data directory 和 `--auto-start=false` 重启。

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

这验证了 transport 已接受的事实不会被覆盖，但无法确认完成的运行状态会被明确取消，不会永久显示为 Working。

原始结果：[`validation/v0.3.0-restart-e2e.json`](validation/v0.3.0-restart-e2e.json)。

## 7. 图片安全与生命周期验证

自动测试和浏览器 E2E共同验证：

```text
formats              PNG / JPEG / GIF / WebP
max per image         5 MiB
max per message       8 images / 20 MiB
max side              8000 px
max decoded pixels    64 MP
```

拒绝项：

- 扩展名伪装的非图片；
- 只有签名但无法解码的截断图片；
- 超限边长/像素；
- 无效 attachment ID；
- symlink 替换后的附件文件；
- 同大小内容篡改；
- 仓库外路径和 symlink escape；
- SVG/HTML/任意二进制；
- 自动加载远程 Markdown 图片；
- 删除已被 durable transcript 引用的附件。

浏览器只通过认证 API 获取图片 Blob；本机绝对路径不会进入 Message、Snapshot、Event 或导出。

## 8. Runtime 协议测试边界

当前容器没有安装 `claude` 和 `codex`。`pairroom doctor --json` 正确报告：

```json
{
  "pairroom": "0.3.0",
  "git": {"available": true},
  "runtimes": {
    "claude": {"error": "... executable file not found ..."},
    "codex": {"error": "... executable file not found ..."}
  },
  "ok": false
}
```

完整结果：[`validation/v0.3.0-doctor.json`](validation/v0.3.0-doctor.json)。

因此本次实际证明的是：

- PairRoom 房间、媒体、浏览器、路由、持久化、审批投影和 Mock E2E；
- Claude/Codex 当前结构化输入输出的编码、解析、状态机和 fixture 单测；
- Claude 图片 block、control response、Reviewer policy 的构造；
- Codex `localImage`、Turn steer、审批和 read-only policy 的构造。

尚未在本执行环境实际证明：

- 真实账号登录与供应商网络；
- 当前真实 Claude Code 对 control handshake 的完整 round-trip；
- 真实 Codex App Server 的图片 Turn、steer 和审批 round-trip；
- 长会话 compaction/resume、真实 Skills/MCP/Hooks；
- Windows/macOS 上的真实 sandbox/permission 行为。

这些应在安装最新官方 CLI 的开发机，先用非关键仓库完成 smoke test。PairRoom 不为历史版本维护兼容矩阵。

## 9. 独立性

```bash
go list -m all
```

输出只有：

```text
github.com/sean2077/pairroom
```

Go 核心没有第三方 module；前端没有 npm dependency、bundler 或 CDN runtime dependency。

## 10. 跨平台构建

发行前构建：

```text
linux/amd64
windows/amd64
macOS arm64
macOS amd64
```

统一使用：

```text
CGO_ENABLED=0
-trimpath
-buildvcs=false
-ldflags=-s -w
```

Linux 产物执行 `pairroom version` 返回 `pairroom 0.3.0`。本次构建 SHA-256：

```text
8f7444dfde365ab9bede0bd9461f958250fe79fc31c0a72605c10eddc47fe0f6  pairroom-darwin-amd64
8adf308b8ec2a19d426d1309841a37d3f11463ce3b8e193b82262003a3fedcbc  pairroom-darwin-arm64
a2fd124083e4dd3d0cfa59a149f5ea0924f6aaa637a95ae6f14e20df2c77f5ed  pairroom-linux-amd64
a610130b8b49fae27e15b7ad64295c52aa080abbe6e0a1c81740e3c797511920  pairroom-windows-amd64.exe
```

## 11. 可信结论

v0.3.0 已达到以下可验证状态：

- 富对话和图片预览不是静态原型，而是通过真实 HTTP/SSE/媒体存储路径运行；
- 图片会被投影成两套原生 Harness 的结构化输入，而不只是 UI 附件；
- Reviewer 和 Claude/Codex 审批已经进入 Adapter 层，不再仅依赖 prompt；
- 文件路径、远程图片、主动内容、内容篡改和 transcript 生命周期有明确安全边界；
- 真实供应商 CLI E2E 仍必须在装有最新官方 CLI 的机器上完成，当前环境不具备该条件。
