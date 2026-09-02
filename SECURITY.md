# PairRoom 安全策略

> [架构](docs/ARCHITECTURE.md) · [隐私模型](docs/PRIVACY.md) · [运维手册](docs/OPERATIONS.md) · [支持范围](SUPPORT.md)

## 1. 威胁模型

PairRoom 启动具有高权限的本地 Coding Agent。Agent 可能读取文件、修改仓库、执行命令、访问网络并调用用户配置的 Skills、MCP、Hooks 和插件。PairRoom Web UI 是控制与观察面，不替代 Claude Code/Codex 自身的权限、sandbox 或组织策略。

PairRoom 还保存敏感讨论、运行事件和图片，因此重点防范：

- 非授权浏览器访问 Management/Room API；
- DNS rebinding、cross-origin command 与 CSRF；
- Token 经 URL、历史、Web Storage、日志或截图泄漏；
- 恶意图片、伪造媒体类型和资源耗尽；
- 路径穿越、symlink escape 和仓库外文件导入；
- 远程 Markdown 图片造成隐式访问泄漏；
- 未知高权限 Vendor request 被错误允许；
- UI role 与实际 Runtime 权限不一致；
- 崩溃后遗留审批、锁或“幽灵 working”；
- Registry、Event Log 与 Binding ownership 分叉；
- Reviewer 被误认为容器级隔离。

PairRoom 主要面向单用户、受信本机和受信仓库。对恶意本地同用户进程、内核攻陷或不可信 OS 不提供安全边界。

## 2. 网络安全默认值

### 2.1 仅数字 loopback

- `pairroom service`、`pairroom serve` 和每个 Room Runtime 只接受数字 loopback 地址；
- 通配地址、LAN/公网地址、主机名和 `localhost` 在打开 repository 或 Service state 前被拒绝；
- tokenless `serve` 仍执行 loopback Host 与 same-origin 检查；
- PairRoom 没有内建 TLS 或远程 listener；
- 远程访问只使用 SSH 本地端口转发。

Bearer Token 是纵深防护，不是传输加密的替代品。

### 2.2 Management Shell 认证

Service 未显式提供 Token 时生成随机 Management Bearer Token，并把它放在启动 URL fragment 中。

浏览器流程支持两种入口：

1. 完整 Management URL：JavaScript 从 fragment 读取 Token，并立即用 `history.replaceState` 从地址栏移除 fragment；
2. 直接打开 Management origin：没有可恢复 Session 时显示凭证登录页，可输入配置的 Service Token，或粘贴包含 `#token=...` 的完整 Management URL；
3. 两种入口都只使用一次 `Authorization: Bearer <token>` 调用 `POST /api/v1/session`；
4. Service 返回路径限定为 `/api/v1/` 的 12 小时滑动过期 `HttpOnly`、`SameSite=Strict` Session Cookie，以及只保存在页面内存的 CSRF Token；
5. bootstrap/登录 Token 随即从页面内存和输入框清除，后续浏览器请求使用 Session Cookie，mutation 还必须提供 `X-PairRoom-CSRF`；
6. 显式退出调用 `DELETE /api/v1/session`；Session 失效时页面回到登录入口。

Management Token、Session ID 和 CSRF 都不写入 `localStorage`/`sessionStorage`。刷新时页面可通过 `GET /api/v1/session` 从仍有效的 Cookie 恢复 CSRF；Service 重启、会话过期、显式注销或新浏览器上下文需要重新提供 Service Token。命令行/API 客户端可继续直接发送 Bearer Header，query-string token 不授权 Management API。

### 2.3 Room View 认证

Service Room Runtime 自动使用独立 Token；兼容 `serve` 可显式设置 Token。启用 Token 时：

1. 启动凭据只出现在 URL fragment；
2. 浏览器通过 bootstrap endpoint 交换为 12 小时滑动过期的 `HttpOnly`、`SameSite=Strict` Session Cookie；
3. 写操作要求每会话 CSRF Token；
4. 长期 Token 和 CSRF 不进入 URL query 或 Web Storage；
5. CLI/API 客户端可继续使用 Authorization Header；
6. query token 不授权 REST、SSE 或附件 API。

Room A 的 Token、Session、CSRF、SSE cursor 与附件授权不能用于 Room B。

### 2.4 HTTP 防护

- Management API 接受直接 Bearer 或有效 browser session；Session 认证的 mutation 要求 CSRF，所有 mutation 另检查 `Sec-Fetch-Site`/Origin；
- Room API 执行 Host 与 same-origin 检查；启用 browser session 时 mutation 还执行 CSRF 检查；
- Room API 按客户端固定窗口限流，降低本地滥用和意外循环；
- 两个 Web 面都开启 CSP、`frame-ancestors 'none'`、no-referrer 与 no-sniff 等安全响应头；Management 同源 Room surface 仅将自己的响应改为 `frame-ancestors 'self'`，直接 Runtime URL 继续禁止 framing；
- Room 附件响应要求认证，并使用 `nosniff`、ETag 和 inline disposition；
- 启动 fragment 不随 HTTP 请求或 Referer 发送，但仍可能经屏幕共享、日志复制或浏览器扩展泄漏。

## 3. Project、Room 与 Binding

- Project 只接受用户显式输入的绝对路径；
- 服务端解析 symlink、Git worktree root 并 canonicalize；
- 不扫描常用开发目录，也不提供服务器文件系统浏览器；
- Room provisioning 在隐藏目录完成，全部成功后原子发布；
- `(agent, vendor_session_id)` 在 Service 内全局唯一，归档不释放 ownership；
- Existing Binding 必须精确恢复；
- deferred New Binding 只在首个真实输入被接受后 materialize；
- Event append、ownership checkpoint 或唯一性失败时中断执行并 fail closed；
- PairRoom 不导入绑定前 Vendor Transcript。

## 4. 附件安全

- 只接受 PNG、JPEG、GIF、WebP；
- 拒绝 SVG、HTML、脚本和任意二进制；
- 检查真实内容签名，不信任文件扩展名或客户端 MIME；
- 限制单张大小、单消息总大小、张数、边长和总像素；
- 文件和 manifest 使用随机不透明 ID 与保守权限；
- 每次 Resolve 都复核大小、普通文件、非 symlink、维度和 SHA-256；
- Message/API/export 不包含附件本机绝对路径；
- 仓库图片导入经过 canonical path 与 symlink 边界检查；
- 远程 URL 不进入自动导入流程；
- 已进入 durable transcript 的附件不可通过 DELETE API 移除；
- object URL 只存在于当前页面，不作为持久公开链接。

图片仍可能包含肉眼可见的 secrets、客户信息或其他窗口内容；格式校验不能替代发送前人工检查。

## 5. Runtime 与审批

### 5.1 Claude

- 启动必须完成 native control initialize；
- 未知 control request 返回 error；
- `can_use_tool`/`AskUserQuestion` 进入 durable approval lifecycle；
- Reviewer 使用 plan permission mode，并阻止写工具；
- 控制层对到达的写请求再次 fail closed。

### 5.2 Codex

- 未知 app-server request fail closed；
- Reviewer Turn 使用 read-only sandbox；
- 追加权限只能授予原 request 的子集；
- command/file/additional-permission request 进入统一审批生命周期。

### 5.3 审批生命周期

中断、停止、重启、Runtime error/exit、角色切换和 PairRoom restart 会使无法安全复用的 pending approval 过期。UI 不应重放旧决定到新的 Vendor request。

角色变化遵循“先应用 Adapter policy、后持久化 Room role”，避免界面显示 Reviewer 而底层仍按 Driver 权限运行。

## 6. Reviewer 工作区边界

Reviewer 默认在独立 Git snapshot 中运行：

- 包含 HEAD；
- 应用 staged + unstaged tracked diff；
- 复制 untracked regular files；
- 拒绝不安全 symlink 与越界引用；
- 记录 source HEAD、dirty 与 snapshot digest；
- POSIX 上移除写位；
- 再叠加 Claude plan/disallowed tools 或 Codex read-only sandbox。

这不是容器、VM、只读 mount 或恶意代码沙箱。外部 MCP、Vendor Runtime bug、Windows 权限语义或用户自定义配置可能扩大访问范围。对不可信任务使用受控容器/VM/独立 checkout。

Driver 默认是唯一写入者。Reviewer snapshot 不应作为并行实现分支；确需两个写入者时，使用人工管理的独立 Git worktree/branch 和显式合并。

## 7. 持久化与恢复

- data root 使用私有目录权限，Event、prompt、图片和 manifest 使用保守文件权限（平台支持时）；
- append-only event 先同步再发布；
- 只修复损坏的最后半行；
- 中间损坏、sequence 分叉或未来 schema 被拒绝；
- Registry checkpoint 可从默认 Room Event Logs 重建；
- checkpoint 写入失败且无法证明一致时，阻止后续 mutation；
- 一个 data root 只允许一个 Service writer；
- stale lock 不自动猜测，必须人工确认旧进程退出后显式恢复；
- backup/restore 拒绝 traversal、link、duplicate、未声明文件、大小和哈希异常；
- 普通 transcript export 不包含 verbose Inspector event tail；
- diagnostics 设计为省略 transcript 正文和附件 bytes，但仍需人工检查。

不要手工编辑 Event sequence、Store schema、attachment manifest 或 Binding Identity。

## 8. Runtime 容量与关闭

- 活动 Turn 不为容量回收而被中断；
- queued Runtime 可取消；
- active+idle Runtime 可安全 drain；
- busy、starting/stopping 冲突或 cleanup-uncertain failed 不会被假装挂起；
- cleanup uncertain 的 Runtime 继续占用 capacity；
- shutdown 先停止 Management mutation，再等待在途管理请求和 Room Turn，最后释放 lock。

强杀进程可能留下 stale lock、未决审批或需要 replay 收口的 Processing 状态。优先使用正常 daemon/Service 生命周期。

## 9. 敏感本地数据

`events.jsonl` 可能包含：

- 用户提示与 Agent 回答；
- 文件名、Diff、工具参数；
- 命令输出、错误与本机路径；
- 模型/runtime 诊断；
- 审批详情和 Session/Thread ID。

`attachments/` 可能包含错误截图、产品 UI、架构图、数据图和客户材料。整个 Room data、日志、备份和导出都应按私有代码资产处理。

## 10. Vendor 数据路径与自定义配置

使用云模型时，代码、图片和工具结果可能发送给对应供应商。PairRoom 不代理、加密或改变这条路径。

官方 CLI 仍会加载用户/项目配置、Skills、MCP、Hooks 和插件。恶意或过宽配置可能扩大读写、网络和外部服务访问范围；PairRoom 不审计这些配置。

## 11. 远程资源

PairRoom 不自动加载远程 Markdown 图片。普通 `https` 链接由用户主动打开后，浏览器直接访问目标站点；目标站点将看到常规网络请求信息。

## 12. 推荐操作

1. 只对可信仓库运行；
2. 将 secrets 放在 Agent 无需读取的位置；
3. 默认使用一个 Driver、一个 Reviewer；
4. 从保守 Vendor permission/sandbox 开始；
5. 仔细审查命令、路径和权限范围；
6. 图片发送前检查可见敏感信息；
7. listener 保持 numeric loopback，远程只用 SSH 转发；
8. Vendor CLI 升级后运行 `pairroom doctor` 和非关键仓库真实 smoke；
9. 定期 verify/backup，按敏感度保护 Room data；
10. 对强隔离需求使用容器/VM/独立 checkout；
11. Management/Room 完整启动 URL 不进入公开日志或 Issue；
12. diagnostics 分享前人工检查。

## 13. 漏洞报告

不要在公开 Issue 中附带 secrets、私有仓库内容、真实附件、认证 Token、Cookie、完整启动 URL 或可直接利用的敏感 payload。

优先使用仓库的私有安全报告渠道；只提供最小复现、受影响版本、平台、威胁前提和预期边界。若私有渠道不可用，先提交不含利用细节的公开 Issue 请求维护者建立安全沟通渠道。
