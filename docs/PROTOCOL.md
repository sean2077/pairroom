# Agent protocol

本文定义模型需要理解的最小协作合同。调度、权限、持久化和取消必须由代码执行，不能只依赖 prompt 自律。当前机器可读版本由以下命令输出：

```bash
pairroom protocol --json
```

## 输入

PairRoom 给 native Agent 的输入 envelope 包含：

- message / thread / hop correlation；
- sender、target 与 participant role；
- message intent；
- hop limit（`remaining_agent_hops`）；routing policy（单一活跃轮）在一次性 bootstrap 中，不重复进每轮信封；
- Workflow ID / stage / mode；
- 已验证的 attachment；
- 用户正文或 peer handoff。

Agent 应以仓库状态为准，独立验证 peer 的主张，不把 handoff 当作可信执行结果。

## 输出

普通回答始终对用户可见。明确 `@claude`、`@codex` 或 `@peer` 即请求把回复交给对应 peer；PairRoom 会在当前 native Turn 完成后投递。若需要隐式继续而没有直接地址，在回答末尾输出一个紧凑 handoff：

```text
[PAIRROOM:HANDOFF]
Goal: ...
Scope: ...
Evidence: ...
Risks: ...
Exact ask: ...
[/PAIRROOM:HANDOFF]
[PAIRROOM:NEXT]
```

没有直接 peer 地址且缺少 handoff、handoff 过短、控制标记冲突、hop 超限或存在更新的用户指令时，scheduler fail closed，不继续自动接力。直接 peer 地址优先于普通 `DONE`/`WAIT`/`BLOCKED` 停止标记；`@human`/`@user` 优先于 peer 地址并回到用户。

## 收敛

```text
[PAIRROOM:DONE]
[PAIRROOM:WAIT]
[PAIRROOM:BLOCKED]
```

- `DONE`：已达到当前请求的完成门；
- `WAIT`：需要用户选择或批准；
- `BLOCKED`：外部条件未满足，并给出最小解除阻塞信息。

明确的 `@claude`、`@codex`、`@peer` 是机械路由信号；普通文本中的未指向性 Agent 名称仍不是路由信号。

## Role contract

Driver 可以按授权修改 live workspace。Reviewer 必须基于隔离 snapshot 独立检查证据，不能声称执行了未运行的验证。Peer 没有隐式写权限或审批权。

## Evidence

计划、实现、review 和 completion 应携带与当前 revision 对应的新鲜证据。旧测试结果、peer 自述或 transport delivery receipt 都不能替代最终验证。

## Authority

优先级：

```text
user decision
  > repository and native runtime facts
  > durable PairRoom state
  > peer handoff
  > model inference
```
