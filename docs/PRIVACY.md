# PairRoom 隐私模型

PairRoom 是本地进程，但会把用户选择的输入、附件和代码上下文交给官方 Claude Code/Codex。隐私边界由 PairRoom 本地存储、浏览器、Vendor CLI 配置和供应商政策共同决定。

## 数据流

```text
Browser / CLI
  -> local PairRoom Service / Room
  -> local Event Log / attachments
  -> official Claude Code / Codex process
  -> configured model provider
```

PairRoom 不提供自有 telemetry、托管 credential 或隐藏云服务。

## 本机持久化

可能持久化：

- Project canonical path、Room metadata 与 lifecycle；
- Claude Session ID、Codex Thread ID；
- Message 正文、handoff、Delivery/Processing、Workflow、Approval history；
- durable Turn summary；
- 用户上传和安全导入的图片及 manifest；
- Service Registry、daemon 配置、日志、备份和 diagnostics。

这些数据可能泄露源码结构、任务内容、供应商 identity 和本机路径。data root、配置和备份应使用当前用户权限保护。

## 不由 PairRoom 复制的内容

PairRoom 不持久化 Vendor 内部推理状态，也不导入 Existing Binding 在 PairRoom 绑定前的 Transcript。官方 CLI 可能在自己的目录保存会话、缓存、日志和 credential；其生命周期由 Vendor 管理。

## 发往供应商的数据

根据用户操作和当前角色，官方 CLI 可能接收：

- Message / handoff / Workflow 指令；
- 仓库文件、Git diff、工具输出；
- Reviewer snapshot；
- 图片附件；
- Provider endpoint、model 和 process environment。

PairRoom 的本地 UI 不改变供应商的数据保留、训练、地区和组织策略。使用自定义 Provider 前审查 endpoint 与 policy。

## Provider secret

配置可直接包含 `api_key`，但推荐 `env:NAME` 或 `${NAME}`。PairRoom 将 secret 保留在内存并通过环境传给 Vendor CLI；Codex custom header 值也经临时环境投影，避免出现在命令参数中。

`pairroom providers`、diagnostics 和管理页面会脱敏，但不能保护 shell history、外部进程监控、Vendor 日志或用户自行打印的环境变量。

## 浏览器

Management 与 Room 使用不同的 HttpOnly Session 与 CSRF scope。bootstrap token 位于 URL fragment，不进入服务器 access log 或 query；PairRoom 不把长期 Token 写入 Web Storage。

浏览器仍可能保存 history、下载 export/backup、截图、DevTools 内容和操作系统缓存。共享设备上结束后 Logout，并清理下载文件。

## 日志与 transient telemetry

Service/daemon 日志可能包含版本、错误、Project path、Room ID、Vendor stderr 或命令摘要。Room Activity 的 transient tool/command output 默认不作为完整 Transcript 持久化，但可能出现在当前浏览器、日志或 forensic JSON export。

不要在公开 Issue 直接粘贴完整日志。

## Export、Backup 与 Diagnostics

- Transcript export 包含 Message 正文和附件 metadata；JSON 可显式包含 event tail；
- Backup 包含完整 Room 数据与附件 bytes；
- Diagnostics 默认省略 Message 正文和附件 bytes，但可包含路径、Event header 与错误。

三者都应视为敏感文件。分享前人工检查，传输后按需要删除。

## 图片与远程资源

上传图片会复制到 Room Attachment Store。Agent 引用的仓库内图片只有在 canonical path 无 symlink escape 时导入。PairRoom 不应把任意远程 URL 下载为可信本地附件，也不在公共 Event/API 暴露本机绝对附件路径。

## 数据保留与删除

- Archive 保留全部 Room 数据和 Binding ownership；
- Permanent Room delete 删除 PairRoom 管理的 Room 数据和 ownership，要求 archived + 显式确认；
- Project unregister 只删除空 Project 注册；
- 删除 Room/Project 不删除 Git worktree、Vendor Session/Thread、Vendor 缓存或外部 legacy import 目录；
- 备份、export、diagnostics、浏览器下载和系统日志需要分别删除；
- committed deletion 的物理 cleanup 失败时，quarantine 会保留到 maintenance retry 成功。

## 远程使用

只通过 SSH 本地端口转发访问。直接暴露 listener、使用不受支持的反向代理或同步 data root 到第三方云盘会改变威胁与隐私模型，项目不为该拓扑提供保证。

## 使用前检查

- [ ] Project/Room 不含不应发送给供应商的资料；
- [ ] Vendor 账号、组织、Provider endpoint 和政策符合要求；
- [ ] data root、配置、日志和备份权限正确；
- [ ] Token/API key 不在仓库、命令行或公开日志；
- [ ] diagnostics/export 已人工脱敏；
- [ ] 删除操作覆盖了 PairRoom、Vendor、Git 与备份等各自独立的数据位置。
