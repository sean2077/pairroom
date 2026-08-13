# Security Policy

## Threat model

PairRoom 启动具有高权限的本地 Coding Agent。Agent 可能读取文件、修改仓库、执行命令、访问网络并调用用户配置的 MCP/Skills/Hooks。PairRoom Web UI 是控制与观察面，不替代 Claude Code/Codex 自身的权限与沙箱。

PairRoom 同时保存敏感讨论、运行事件和用户图片，因此威胁模型还包括：

- 非授权浏览器访问本地 API；
- DNS rebinding / cross-origin command；
- 恶意图片或伪造媒体类型；
- 路径穿越、symlink escape 和仓库外文件导入；
- 远程 Markdown 图片造成访问泄漏；
- 未知高权限供应商请求被错误允许；
- 角色 UI 与实际 Runtime 权限不一致；
- 崩溃后遗留审批或“幽灵 working”。

## Safe defaults

### Network

- 默认绑定 `127.0.0.1:7332`；
- 非 loopback 绑定且未提供 Token 时自动生成 Bearer Token；
- 非 loopback 启动 URL 只在 fragment 中携带一次性启动凭据，fragment 不随 HTTP 请求或 Referer 发送；
- 浏览器将启动凭据交换成 12 小时滑动过期的 `HttpOnly`、`SameSite=Strict` Session Cookie；
- 浏览器写操作要求每会话 CSRF Token，Token 不进入 URL 查询、`sessionStorage` 或 `localStorage`；
- query token 不授权任何 API；命令行 API 客户端继续使用 Authorization Header；
- API 具备按客户端固定窗口限流，降低意外循环和本地滥用风险；
- tokenless server 只接受 loopback Host，降低 DNS rebinding 风险；
- API 请求执行 same-origin 检查；
- CSP、`frame-ancestors 'none'`、no-referrer、no-sniff 和权限策略默认开启。

### Attachments

- 只接受 PNG、JPEG、GIF、WebP；
- 拒绝 SVG、HTML、脚本和任意二进制；
- 检查真实内容签名，而不是信任文件扩展名；
- 限制单张大小、单消息总大小、边长和总像素；
- 文件和 manifest 使用随机不透明 ID 与保守权限；
- 每次 Resolve 都校验大小、普通文件、非 symlink、维度和 SHA-256；
- Message/API/export 不包含附件本机绝对路径；
- 仓库图片导入经过 canonical path 和 symlink 边界检查；
- 远程 Markdown 图片不自动加载；
- 已进入 durable transcript 的附件不可通过 DELETE API 移除；
- Blob fetch 需要 API 认证，响应使用 `nosniff`、ETag 和 inline content disposition。

### Runtime and approvals

- Claude 启动必须完成 native control initialize；
- 未知 Claude control request 返回 error；
- 未知 Codex server request fail closed；
- Codex 追加权限只能授予原 request 的子集；
- 中断、停止、重启、Runtime error/exit 和 PairRoom restart 使未决审批过期；
- Claude Reviewer 使用 plan mode + disallowed write tools；
- Codex Reviewer 使用 readOnly sandbox；
- 角色先应用 Adapter、后写入 durable room state。

### Persistence

- 状态目录使用 `0700`，事件、prompt、图片和 manifest 使用 `0600`（POSIX）；
- append-only 事件先同步再发布；
- 只修复损坏的最后半行；
- 高于当前版本的 Store schema 被拒绝；
- 普通 transcript export 不包含 verbose Inspector events；
- 本机路径不进入图片附件元数据。

## Important caveats

### No built-in TLS

不要直接暴露到公网。远程访问应使用可信 SSH tunnel/VPN，或置于受控 TLS reverse proxy 后。Bearer Token 不能防止明文网络监听。

### Local room data is sensitive

`events.jsonl` 可能包含：

- 用户提示与 Agent 回答；
- 文件名、Diff、工具参数；
- 命令输出、错误、本机路径；
- 模型/runtime 诊断；
- 审批详情。

`attachments/` 可能包含错误截图、产品 UI、架构图、数据图和其他项目敏感材料。整个 data directory 都应按私有代码资产处理。

### Vendor data handling still applies

使用云模型时，代码、图片和工具结果可能发送给对应供应商。PairRoom 不代理、加密或改变 Claude Code/Codex 的供应商数据路径。

### Existing Agent customization remains active

官方 CLI 仍加载用户/项目配置、Skills、MCP、Hooks 和插件。恶意或过宽配置可能扩大访问范围。PairRoom 不审计这些内容。

### Reviewer snapshot is not a container boundary

Reviewer 默认运行在 PairRoom 独立生成的 Git snapshot 中，该 snapshot 包含 HEAD、dirty tracked patch 与 untracked regular files。PairRoom 会拒绝不安全 symlink，并在 POSIX 上移除写位；Claude plan/disallowed tools 与 Codex readOnly sandbox 提供第二层约束。

这仍不等价于容器、VM 或只读 mount。外部 MCP、供应商 Runtime bug、Windows 文件权限语义或用户自定义配置可能扩大访问范围。对不可信任务应使用受控容器/VM。

### Single writer

Driver 使用 live working tree；Reviewer snapshot 用于独立读取和审查，不应作为并行实现分支。若确需两个写入者，使用人工管理的独立 Git worktree/branch，并显式合并。

### Remote links

PairRoom 不自动加载远程 Markdown 图片，但普通 `https` 链接可由用户主动打开。打开后由浏览器直接访问目标站点。

### Browser object URLs

附件通过认证 API 下载为 Blob，并在当前页面创建 object URL。关闭页面或重新渲染清理 object URL；它们不应作为持久共享链接。

## Recommended operation

1. 只对可信仓库运行；
2. 将 secrets 放在 Agent 无需读取的位置；
3. 使用一个 Driver、一个 Reviewer；
4. 从保守 vendor permission/sandbox 开始；
5. 仔细查看审批中的命令、路径和权限范围；
6. 图片发送前确认不含不应上传给供应商的数据；
7. 保持 listener 在 loopback；
8. 升级 Claude/Codex 后运行 `pairroom doctor` 和非关键仓库 smoke test；
9. 按项目敏感度备份或删除整个 room data directory；
10. 对强隔离需求使用容器/VM/独立 checkout，而不是只依赖 UI role。

## Reporting vulnerabilities

不要在公开 Issue 中附带 secrets、私有仓库内容、真实附件、认证 Token 或可直接利用的敏感 payload。仓库发布后应使用私有安全报告渠道，并只提供最小复现信息。
