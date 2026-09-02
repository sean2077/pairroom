# Troubleshooting

## Agent 无法启动

1. 在同一用户和仓库目录直接运行 `claude --version` / `codex --version`；
2. 检查 executable、Provider、cwd、权限与 sandbox；
3. 查看 participant `LastError` 和 Runtime info；
4. 若配置了严格 session resume，确认 Binding 指向的原生 session 仍存在。

PairRoom 不代替供应商 CLI 登录。

## Turn 长时间没有输出

先看 Inspector / Turn card 是否存在长命令、审批或持续 tool activity。stall notice 只表示一段时间没有新 event，不会自动中断 Turn。

若确认卡死，按风险顺序选择 steering、interrupt、cancel 或 restart；不要同时向另一 Agent 强制提交，以免破坏 single-owner 边界。

## Codex 出现 error，但仍显示工作中

generic Codex `error` 可能是 Turn 中途诊断。PairRoom 会记录它，但只有 `turn/completed`、明确 abort / cancellation 或确认 process exit 才释放 owner。若之后一直没有 terminal event，再按卡死流程处理。

## 发给 peer 的消息一直 Waiting

这是当前 Agent 仍持有 native Turn 时的预期行为。跨 Agent message 位于 Room FIFO，必须等待可靠 terminal boundary。明确 `@peer` 会请求交棒；没有直接地址时才需要有效 `HANDOFF` 与 `NEXT`。`@human`/`@user` 会把流程留给用户决策。时间线上方的「当前轮次」条会显示持有 Turn 的 Agent 与队列深度，用于一眼判断是否在排队。

如果看到“without a usable HANDOFF”提示，检查 Agent 是否只输出了 `[PAIRROOM:NEXT]`。在没有直接 `@claude`、`@codex` 或 `@peer` 地址时，补充完整的 `HANDOFF` 区块；直接地址缺少区块时 PairRoom 会使用有界的回复上下文继续投递。

## 重启后排队消息没有继续

Room FIFO 是进程内状态，重启不会自动重放。检查目标仓库是否已发生副作用，然后对明确安全的失败 / 取消消息使用 Retry。不要手工把旧 message ID 改回 pending。

## 取消一条消息影响了整个 Turn

FIFO 中的消息可以精确取消；native runtime 已接受输入后，供应商的 interrupt 往往以整个 active Turn 为粒度。PairRoom 会保留无关 Room FIFO，但当前 Agent 同一 native Turn 内的多个输入可能一起终止。

## Reviewer 看不到 Driver 最新文件

Reviewer 使用隔离 snapshot。确认 review 是在 Driver Turn 完成后的新边界启动；角色切换或 snapshot refresh 失败时查看 system notice。不要让 Reviewer 与 Driver 同时写 live workspace。

## UI 频繁刷新或滚动位置跳动

确认使用当前构建，查看浏览器控制台与 SSE 重连。页面应批量合并高频 telemetry，而不是逐 token 重建全部 DOM；若 snapshot sequence 反复倒退，保存 Room ID 和 event sequence 报告问题。

## Room 无法恢复

常见原因：

- Event Log 损坏；
- Room 含当前版本拒绝的旧 routing event；
- Project path 已移动；
- strict Binding session 不存在；
- 备份不完整。

保留原始数据目录，先验证备份和首个 replay error。旧 routing Room 不自动迁移，应重建。

## 端口或 token 问题

使用当前命令的 `--help` 检查 listen / token 参数，确认没有另一进程占用端口。非 loopback 监听没有 token 应视为配置错误，而不是绕过检查。
