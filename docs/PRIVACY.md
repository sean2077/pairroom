# PairRoom 隐私模型

> [文档首页](README.md) · [安全策略](../SECURITY.md) · [运维手册](OPERATIONS.md) · [支持范围](../SUPPORT.md)

PairRoom 是本地优先软件：没有 PairRoom 运营的云服务、账号系统、遥测收集器或模型代理。但“本地优先”不等于“数据永不离开机器”——官方 Claude Code/Codex 仍可能按各自配置把代码、图片和工具结果发送给模型供应商。

## 1. 数据流总览

```text
Browser
  │ messages / images / decisions
  ▼
Local PairRoom Service + Room Stores
  │ structured prompts, context, tools and images
  ├──────────────────────► official Claude Code ─► Claude provider path
  └──────────────────────► official Codex       ─► OpenAI provider path
```

PairRoom 不插入自己的云端中转，不托管 Vendor credential，也不改变供应商的数据保留、训练或组织政策。

## 2. 本机持久化数据

一个 Room 可能保存：

- 用户提示与 Agent 最终回答；
- 路由、引用、线程、重试、Delivery/Processing 状态；
- 有界工具、命令、计划、Diff、用量、错误和 Turn 摘要；
- 审批请求、决定和范围；
- Project/Repository 路径与 Git metadata；
- 上传或 Agent 生成的 raster 图片；
- Claude Session ID 与 Codex Thread ID；
- Room role、workspace boundary 和 runtime diagnostic。

Service Registry 还保存 Project、Room、生命周期与 Binding ownership 的索引。默认权限在支持的平台上使用私有目录/文件模式，但同一 OS 用户下的恶意进程仍可能读取这些文件。

## 3. 不由 PairRoom 持久化的内容

PairRoom 不保存：

- Vendor 账号密码、API key 或登录 cookie；
- Claude/Codex 完整原生 Transcript 数据库；
- 模型隐藏推理过程；
- 远程 Markdown 图片的自动下载副本；
- PairRoom 云端副本或遥测事件。

PairRoom 会保存结构化 Runtime 摘要与最近事件用于可观察性，但这不等同于复制供应商的完整内部会话。

## 4. 发往供应商的数据

PairRoom 启动用户本机官方 Harness。实际出站内容由下列因素共同决定：

- 发送给某个 Agent 的用户消息；
- 该 Room 的历史与原生 Session/Thread context；
- Agent 读取的仓库文件；
- 工具、命令、MCP、Skill、Hook 或插件的输出；
- 上传给该 Agent 的图片；
- 供应商 CLI 的配置、权限、sandbox 与组织策略。

PairRoom 不拦截、重新加密、匿名化或重写这些内容。处理私有代码前，应同时检查供应商条款、企业策略和 CLI 自定义配置。

Reviewer snapshot 降低默认写入风险，但不会阻止只读内容被模型或外部 MCP 读取和发送。

## 5. 浏览器数据

Management Shell 与 Room View 的浏览器状态不同。

### 5.1 Management Shell

只保存在当前标签页内存，或在一次性 bootstrap 完成前短暂存在：

- Management bootstrap Bearer Token（交换后立即清除）；
- Management 和 Room 的 CSRF Token；
- 当前路由、搜索与筛选；
- theme、density、refresh interval；
- include archived 与打开 Room 行为。

启动 Token 从 URL fragment 读取后立即移除，不写 `localStorage` 或 `sessionStorage`。Management bootstrap 后由当前 Service 签发 `HttpOnly`、`SameSite=Strict` Cookie，刷新可恢复仍有效的会话；Service 重启、会话过期或注销后需要重新打开完整启动 URL。页面偏好仍在刷新后重置。

### 5.2 Room View

Room 可在浏览器本地保存非秘密界面状态，例如：

- theme；
- composer draft；
- unread cursor；
- 当前 target/routing intent；
- 部分查看偏好。

启用 Token 认证时，长期 bootstrap Token 不进入 Web Storage：它从 fragment 交换为 HttpOnly、SameSite=Strict Session Cookie；写操作使用内存中的 CSRF Token。

### 5.3 浏览器缓存与 object URL

附件通过认证 API 获取 Blob，并创建当前页面的 object URL。页面关闭或重新渲染时清理；它们不是可持久分享的公开 URL。浏览器、崩溃报告软件或扩展自身的缓存/记录行为不由 PairRoom 控制。

## 6. 日志

前台输出和 daemon 日志可能包含：

- build 与模式；
- data root、Project/Room 路径；
- Runtime phase、错误与协议诊断；
- 启动 URL。

Management 启动 URL 的 fragment 是凭据，不应进入公开日志、截图或工单。Room/Agent 错误也可能携带环境路径或命令摘要；日志应按私有开发资产处理。

## 7. 导出、备份与诊断

| 产物 | 典型内容 | 重要边界 |
|---|---|---|
| Transcript export | 消息正文、附件安全元数据 | 不是脱敏报告，通常仍含敏感讨论 |
| Forensic JSON export | 结构化事件与 Inspector tail | 不是面向公开分享的自动脱敏产物 |
| Room backup | 完整 Event Log、metadata、附件 bytes | 是完整敏感数据副本 |
| Room diagnostics | 结构化头、版本、校验和错误；设计上省略正文/图片 | 仍可能包含环境路径和诊断信息 |
| Service diagnostics | Project/Room/Runtime 结构、policy、build、registry diagnostic | 不含 Room transcript/附件，但路径和错误仍需检查 |

所有导出在分享前都应人工检查。备份应按源代码、提示与截图的最高敏感等级保护。

## 8. 图片与远程资源

- 上传仅接受 PNG/JPEG/GIF/WebP；
- 本机绝对路径不进入公共附件 metadata；
- Agent 回答只可自动导入当前仓库内、通过 canonical/symlink 检查的图片；
- 远程 Markdown 图片不会自动抓取，避免页面打开即泄漏 IP、Referer 或访问时机；
- 普通 `https` 链接仅在用户主动打开后由浏览器访问；
- 图片发送给 Agent 前应检查截图中是否有密钥、客户数据、通知或其他窗口内容。

## 9. 数据保留与删除

PairRoom 没有远程副本可请求删除。删除一个项目相关数据时需要分别处理：

1. 停止相关 Room Runtime 或整个 Service；
2. 删除 Room 数据目录；
3. 删除独立备份、诊断与 transcript export；
4. 评估浏览器本地状态和日志；
5. 按存储介质与组织策略执行安全删除；
6. 单独遵循 Vendor 对原生 Session/Thread 和云端数据的删除流程。

删除 Git 仓库不会自动删除 PairRoom Room；归档 Room 也不会删除 Event Log、附件或 Binding。

当前 Management API 不提供永久 Room deletion。需要销毁数据时应先停服、备份必要证据，再在文件系统层面处理，并避免留下 Registry 与目录不一致的运行中状态。

## 10. 远程使用

PairRoom 只监听数字 loopback 地址，不内建 TLS。SSH 本地端口转发会保护网络传输，但不会改变：

- 远端主机上的本地数据风险；
- 浏览器所在机器的扩展/缓存风险；
- Vendor Harness 的出站数据路径；
- 完整启动 URL 泄漏后的凭据风险。

不要以 Bearer Token 代替传输加密，也不要把 listener 直接暴露到局域网或公网。

## 11. 使用前隐私检查

- 仓库是否允许交给两个对应供应商处理；
- 图片是否含屏幕外泄漏信息；
- 用户/项目 Skills、MCP、Hooks、插件是否可信；
- Reviewer 只读是否足够，还是需要容器/VM；
- Room 数据根、日志和备份是否位于受控磁盘；
- diagnostics/export 是否经过人工审查；
- 项目结束后的保留与删除责任人是否明确。
