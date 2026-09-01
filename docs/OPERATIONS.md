# Operations

## 运行形态

- `pairroom service`：正常的多 Project / 多 Room 管理入口；
- `pairroom serve`：单仓库兼容入口；
- `pairroom daemon`：把 Service 作为本机后台进程管理；
- `--mock`：不启动供应商 CLI 的确定性验证模式。

默认使用 loopback。暴露到其他接口前必须配置 token，并评估本机仓库、Agent 凭据和附件所带来的风险。

## 日常检查

观察四层状态，不要只看聊天文本：

1. Service / Room runtime 是否激活；
2. participant state 与 native session binding；
3. message delivery / processing；
4. Turn summary、tool activity、approval 和 system notice。

“一段时间没有 Runtime event”只是提醒。长命令、压缩上下文或未暴露的供应商步骤都可能暂时静默；普通 diagnostic error 也不一定是 terminal boundary。

## Project、Archive、Delete

- **Unregister Project**：移除 Management Service 的注册记录，不删除用户 Git 仓库；执行前处理仍归属该 Project 的 Room；
- **Archive Room**：停止当前 Agent Turn并挂起 Runtime，保留 Room 数据以供审计或恢复；
- **Permanent delete**：删除 PairRoom 管理的数据，应先确认归档、备份、Binding 和 active runtime 前置条件；
- **删除仓库**：永远不是 PairRoom Project unregister / Room delete 的隐含副作用。

具体可用动作由当前 UI / CLI / API 返回的 precondition 决定，自动化脚本不得忽略冲突响应。

## Capacity 与 idle 回收

Service 可以限制同时 active Room 数，并按 idle policy 回收 Runtime。回收只停止进程和释放资源，不删除 durable Room。下次激活会重新建立 adapter，但 Room FIFO 不跨进程恢复。

## Backup

建议在以下操作前创建并验证备份：

- 升级跨 breaking release；
- 永久删除 Room；
- 移动 PairRoom data root；
- 手工修复 Event Log；
- 更换 session / Binding 策略。

备份成功必须以 manifest / checksum 验证为准，而不是仅看压缩命令退出。

## Graceful shutdown

正常退出应停止 active adapter、结算在途 projection、关闭 store，并清理 reviewer workspace。强制杀进程后，下一次启动会 fail closed；它不会推测上一次 native operation 是否执行完成。

## 日志与诊断

日志不得包含 API key、Authorization header 或附件绝对路径。报告问题时提供：

- PairRoom 构建版本；
- OS 与两个 CLI 版本；
- Room / message / Turn ID；
- 相关 system notice 和 terminal event；
- 已脱敏的配置；
- 最小 Mock 或只读复现步骤。

常见处置见 [Troubleshooting](TROUBLESHOOTING.md)。
