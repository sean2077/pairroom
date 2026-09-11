# Native 宿主模式（原生焦点协作）设计规格

- **状态**：已批准（v10，2026-09-10，项目所有者明确批准）；实现进行中；Phase 0 文档通道核验通过，真实双端 E2E 仍为发布门禁。
- **评审史**：经 gpt-6-astra 七轮只读技术评审收敛；全部成立意见已合并，无悬置决策、无已知逻辑矛盾。
- **定位与存放**：本文档是在途功能的权威设计规格，**不是当前契约**。按 `docs/README.md` 的文档政策（提案在被接受并实现之前归属 Issue/PR，不得把未来提案静默转换为已实现能力），本规格在实现落地前只存在于本 PR 分支，不合入 main。本文以中文保存以逐字保留批准措辞；实现落地时，耐久契约必须以**英文**同步迁入 `ARCHITECTURE.md` / `PROTOCOL.md` / `STORAGE.md` / `UPGRADING.md` / `SUPPORT.md` 与 `CLAUDE.md` invariant（同一 change 内），本文档届时随实现 PR 合入或关闭，新概念术语按术语硬规则进 `CONTEXT.md`。

## 1. 目标与用户

双 Agent 协作焦点移入原生 harness（Claude Code 终端 / Codex Desktop）。用户对任一 agent 说「规划 xxx，和对方讨论」，该侧响应结束后回复经 PairRoom 自动中继，对方在 park 窗口内自动唤醒回应，多轮交流无需人工干预。PairRoom 在 native 模式 = 中继总线 + append-only 事件日志 + 绑定/审计，不拥有长驻原生进程，无按消息散落的中间文件。用户 = 同时使用两家官方 harness、要求原生交互体验与可审计中继的开发者。

## 2. 已锁定决策

| # | 决策 | 结论 |
|---|---|---|
| D1 | 回显范围 | Room 只记录中继消息、`@user` 升级、绑定/生命周期事件；PairRoom 不解析厂商私有 transcript；全量镜像为 non-goal |
| D2 | 模式粒度 | Room 级宿主模式，创建时选定 `embedded`/`native`，持久化不可变不互转；同一 CLI 可跨房混用；live-attach 运行中会话仅列研究项 |
| D3 | 唤醒分层（收窄项，所有者已确认） | W2 同意制 hooks 为主路径，且是**绑定关联的前提**；W1 前台 wait / W4 nudge 限定为**关联完成后**的降级模式；**零 hook 环境一期不支持绑定**（bind 显式拒绝并给指引）；Claude 侧项目级 `.claude/settings.json` 一次性同意；Codex 侧关联载荷与粒度 gate 在 Phase 0 ① 结论；W3（resume 注入 sibling）仅研究原型 |
| D4 | A1 条件化 | park 窗口内自动唤醒；窗口外（超时/再武装耗尽 8-block 上限/禁用 park）降级 queued + 待取件 + nudge。两条都是合格行为 |
| D5 | 所有权 | native 房单 Owner Turn 建议性；强制面 = 中继 FIFO + append-only 审计 + 绑定唯一性；权威文档同 change 分述 |
| D6 | Room UI 输入 | 开放，`@user` 进目标 slot inbox；UI 显示待取件/已交付/未知，不冒充原生接受 |
| D7 | bind 与最小落盘 | 一次性安装 relay 技能/CLI；每会话一次 `pairroom relay bind`；落盘足迹穷尽 = slot 状态文件（bind id、generation、关联 session_id、`last_confirmed_seq`、单条有界 pending 发布 {seq, 正文, 时间戳}，确认即清）+ 凭据文件(0600) + 可选 bootstrap + `.gitignore` 一行；零按消息散落文件 |
| D8 | schema S3' | 新房（含 embedded）一律 Store 11 / provisioning 4，`host_mode` 在 `room.service.provisioned` 事件内显式；存量 10/prov3 房零字节变化、按原 schema 追加；新二进制读 10+11，旧二进制门禁拒 11；prov-3 ⇒ embedded 为版本化时代语义；registry checkpoint 结构/schema 2 完全不动，host_mode 从 Room 事件重建 |
| D9 | 一期范围 | Claude Code + Codex（Codex 以 Phase 0 ① 为硬 gate）；Grok Build 二期 |
| D10 | 内容介质 | 对话内容存续于两边原生 transcript + Room Event Log；对方会话引用（session_id/transcript_path）存于绑定元数据，经 `pairroom relay peer` 按需查询，**不进 envelope**；agent 读取对方历史为尽力而为（允许缺失/延迟/不可访问），不影响接力 |
| D11 | 双发布路径 | 主路径 = Stop hook 上报响应结束时的完整可见回复，自动路由（v6 同构语义）；辅路径 = 显式 `relay send`，固定默认目标 = 对方 slot inbox（可选 `--to @user`），与主路径同 inbox、同等唤醒 park，**正文 mention 不参与路由**、不声称完整性。**发布幂等**：自动接力以 (bind, generation, report_seq) 为幂等键；显式 send 以客户端消息 ID 为幂等键；**永不按正文内容去重**。**诚实声明：幂等不防两条路径重复表达同一任务；同轮双发布 = 可见重复，由 bootstrap 纪律收敛，不做语义级去重** |

## 3. 拓扑

```text
原生会话 A (Codex Desktop)                原生会话 B (Claude Code 终端)
  │ 响应结束 → hook 上报完整回复            ▲ park 中 Stop hook 被唤醒，block+envelope 续跑
  │ （或显式 relay send → 同一对方 inbox）   │ （或降级：前台 relay wait / nudge）
  ▼                                        │
Management Service ──► Room Engine（native 房，不 spawn adapter）
   ├─ per-slot inbox（FIFO，durable，跨重启存活）
   ├─ 路由（仅主路径回复参与）：exact peer handle → 对方 inbox；
   │   peer 与 @user 同现 → peer 优先；仅 @user → Room UI + 接力结束；无 handle → 结束
   └─ append-only Event Log（中继/绑定/gap/生命周期）
```

## 4. 投递状态机与发布可靠性

```text
queued ──claim（先 durable append delivering，再放行 envelope）──► delivering
delivering ──CLI stdout 写出 + 显式 ack 送达──► handed_off（终态）
delivering ──ack 缺失（CLI 死亡/通知丢失/持久化失败，reaper 判定）──► unknown
```

- envelope 永不在 delivering 持久化前离开 Service；ack 永不在 stdout 写出前发送。
- park 长轮询期零认领：超时杀死等待 → 消息仍 queued；认领→ack 终段被命中 → unknown，不自动回队。
- `handed_off` 只声称 CLI 写出 stdout，模型注入永不被声称。`unknown` = 结果不确定，仅显式 Retry（检查 Event Log 与工作区后决定），生成新消息 ID，永不自动重放——与 embedded `submitting` 契约同构。
- **主路径发布可靠性（三层）**：
  1. hook 认领 seq 与 pending{seq, 正文} 以**同一次原子写**落入 slot 状态文件，然后上报；Service 确认 → 清除 pending、推进 `last_confirmed_seq`。原子写未发生则 seq 不消耗、上报不发出；Service 收到某 seq 蕴含本地已持久化该 seq。
  2. 持久化后崩溃（含 Service 已收/未收、响应丢失全窗口）→ 下次 hook 触发或 pending 超龄时，**先按原 `(bind, generation, seq)` 向 Service 对账查询**（查询本身不重新入队），三分支为唯一行为：已接受 → 清除 pending；**明确未接受 → 同 seq 自动补报**（Service 幂等返回原结果）；无法确认 → 状态显示「结果未知」，仅显式人工/agent 决定，**永不自动以新 ID 重发**。（「不自动重发」的对象是新 ID 重发——它会绕开幂等键造成重复执行；同 seq 补报在对账确认未接受时是规定动作。）
  3. 原子写前崩溃 → 该回复未发布、无本地痕迹、**seq 未被消耗**——后续轮次正常占用该序号，Service 视角完全连续。此类丢失**无论有无后续上报都可能无法检测**，是诚实限制，写入文档与 UI 措辞；硬保证仅一条：**不产生任何假发布/假送达记录**。`publication gap` 事件仅在确实观察到跳号证据时产生（即原子写后崩溃、seq 已本地消耗而 Service 未收到、且单条 pending 槽被后续轮次覆写的情形）。缓解（非保证）：bootstrap 指示 agent 在带 peer handle 的回复后，下次行动机会时以 `relay status` 自查送达。
- 显式 send 幂等：客户端消息 ID 为幂等键，同 ID 重复提交返回原结果、不重复入队；**永不按正文内容去重**（不同轮次可合法产生相同文字）。

## 5. 绑定、会话关联与凭据生命周期

- **bind**：`pairroom relay bind --room <id> --slot <slot>`。空槽才能普通 bind；已有活跃绑定时同一身份（关联 session_id 匹配，含 `--continue` 重启）幂等恢复，不同身份拒绝并提示显式 replace；generation 仅在 replace/unbind 后重绑时轮换。
- **会话关联（需 hook 通道，D3 收窄的原因）**：bind 输出仅含非秘密材料（room/slot/bind id、一次性 `bind_nonce`、后续指令）。agent 在可见回复中带出 nonce；自己的 Stop hook 上报 `last_assistant_message` 携回 → Service 建立 session_id ↔ bind 关联。未携 nonce 的上报不产生关联；nonce 单次有效，重放拒绝。**零 hook 环境无法完成关联，bind 显式拒绝并给出文档指引（不静默、不承诺）**。Codex 侧等价载荷 Phase 0 ① 验证，无通道则 Codex native 不发布。SessionStart hook 先行关联列为 Phase 0 优化项。
- **凭据生命周期**：长期 relay 秘密由 bind 时的 CLI 进程生成写入 `.pairroom/rooms/<room>/slots/<slot>/credentials`（0600），**永不输出给模型**；relay 调用与每次 hook 进程从文件读秘密 + 呈现身份（hook 用官方输入的 session_id）向 Service 认证；Service 校验凭据 + generation + 关联三者一致；第二次及后续 hook 取件零模型参与。
- **威胁边界（诚实声明）**：同一用户、同一工作区内的隔离目标 = 防误绑与撤销卫生，不声称抵御拥有任意文件读取能力的另一会话。bind 确保 `.gitignore` 含 `.pairroom/`（唯一的工作区卫生写入，bind 时披露）。
- replace：撤销旧 generation 与旧凭据（文件原子覆写 + Service 拒旧代次）；「inbox 空」不证明旧会话 idle；replace 不能停止已开始的原生工作，UI 如实陈述。归档不释放绑定所有权；断连 ≠ 解绑；上报 session_id 参与全局 `(agent, vendor_session_id)` 唯一性检查，冲突 fail-closed；不复用 `internal/service/native.go` 的 existing 验证路径（其会 spawn 临时 adapter）。

## 6. Turn 监听、park 与自动接力

- **Stop 是「本次响应结束的检查点」，不是不可撤销的任务完成证明**。覆盖与缺口如实枚举：正常响应结束 → Stop 触发，hook 认证后上报 `last_assistant_message`，Service 扫描 exact peer handle 命中即中继完整回复（主路径）；**用户中断 → 不触发 Stop → 该轮内容不发布**（诚实缺口）；**API 错误 → StopFailure**（无决策控制）→ 仅上报失败事件供 Room 可见性，不中继内容；`stop_hook_active` 为真且 inbox 无新消息 → 不空转 block（保护 8-block 预算）。
- **上报（发送侧）与 park（接收侧）在同一次 hook 调用内解耦**：发送侧可靠性不依赖接收侧 park 超时。
- park：上报后阻塞长轮询；对方消息到达 → block + envelope（或 additionalContext）→ 自动续跑。单次长 park 不计入 block 次数；8 次连续 block 上限约束再武装循环；超时 hook 输出被 harness 丢弃（queued 不受影响，见 §4）；每轮再武装的 token 成本如实声明；park 时长/再武装策略参数由 Phase 0 定。
- 逃生门：relay 状态位一键禁用 park（降级 W1/W4，关联保留）；原生打断路径保持可用；park 期间会话显示为忙是已接受的 UX 代价。
- Claude `session_crons`/`ScheduleWakeup` 免 hook 自唤醒列 Phase 0 验证，通过前不入任何承诺。

## 7. 协议 v7

- native 房发布 `pairroom-protocol/v7`：主路径与 embedded v6 同构（响应边界转发完整可见回复，经 hook 上报实现）；辅路径显式 `relay send` 默认目标对方 inbox（可选 `--to @user`），同等唤醒接收者，正文 mention 不参与路由，不声称完整性，为附件唯一通道。**协议文本明载双发布诚实声明**：bootstrap 指令要求「同轮已显式 send 时，最终回复省略 peer handle（结束接力）或有意再带（接受完整回复作为权威边界发布另行送达，可见重复）」。embedded 房保持 v6 字节不变。
- envelope 格式、mention 解析、大小写不敏感、代码块/URL 排除、重复 runtime 槽位后缀、peer handle 与 `@user` 同现时 peer 优先——复用 v6 实现；envelope 不携带会话引用（D10）；128B envelope / 1800B bootstrap byte-budget 静态测试覆盖 native 变体且不被新增字段突破。

## 8. schema 兼容矩阵与 checkpoint

| 二进制 \ 数据 | schema 10 / prov 3（存量房） | schema 11 / prov 4（新房，含 embedded） |
|---|---|---|
| 旧二进制 | 照常读写 | replay 前门禁拒绝；混入数据目录 → 整个 Service fail-closed 拒启（现有 poison 行为，§13 给隔离步骤） |
| 新二进制 | 照常读写激活，按原 schema 追加；prov-3 ⇒ embedded 时代语义 | 按显式 host_mode 激活；写 11/prov4 |

- Event Log append-only 零破坏：host_mode 只在新房自己的 provisioning 事件内；存量房永不改写、无升级命令、无迁移。
- Registry checkpoint 结构与 schema 2 完全不动（`trustedCheckpointRooms()` 的 `DisallowUnknownFields()` 严格路径不受影响）；host_mode 需要时从 Room 权威事件重建。
- `CLAUDE.md` schema 条款更新为：读 10+11；存量房按原 schema 追加；新房写 11/prov4。

## 9. native 房控制面

Cancel 仅移除 inbox queued（对 delivering/unknown 无效，走 Retry 路径）；**Interrupt 不存在，UI 不得提供**（PairRoom 无进程可中断）；归档 fail-closed、不释放绑定所有权；「在线」= 绑定活跃 + 最近 N 分钟 hook/wait/send 活动（措辞为绑定状态与最后活动，不冒充 live presence）；AgentSelection 照常持久化，但 Provider/model/effort/权限 profile 在 native 房仅展示不生效（PairRoom 无子进程环境可注入），bind 时 CLI 与 UI 明示。

## 10. Phase 0 原型门（实现前置，结论写回本文档）

1. **Codex 通道成立性（硬 gate）**：hooks per-project 支持度、Stop 等价事件载荷（最终回复文本/会话 ID/nonce 回传通道）；无关联通道则 Codex native 不发布。
2. **hook 发送侧崩溃窗口全矩阵**：原子写（认领+pending）后、Service 收到正文前 kill；Service 收到后响应丢失；原子写前 kill 且此后不再产生下一轮（断言无假送达、丢失不可检测如实呈现）；原子写后 kill 且 pending 被后续轮次覆写 → 跳号 → gap 事件；超龄对账三分支（已接受/明确未接受/未知）行为。
3. 完整 Codex Desktop → Room → Claude Code → Room → Codex 自动往返（多轮，非仅首条）。
4. park 全周期：长 timeout 单次 park、超时丢弃后 queued 完好、用户打断、再武装至 8-block 上限、token 成本实测。
5. 凭据与关联全链路：首次 bind 后第二次 Stop hook 零模型秘密安全取件；双会话抢绑（B 先触发 hook 无 nonce → 不关联不取件）；replace 后旧代次全拒；nonce 重放拒绝。
6. 两阶段认领崩溃窗口（投递侧）：kill CLI/Service 于认领前后、ack 前后，状态落点与 §4 一致。
7. 同轮「先显式 send 后 Stop」双发布行为：独立审计、路由仅由主路径驱动、可见重复不静默合并。
8. Claude `session_crons` 空闲自唤醒可行性；Stop 缺口路径（中断无发布、StopFailure 仅可见性）实测。
9. W3 研究项（不承诺）：原生 app 运行期间并发 resume 冲突；SessionStart 先行关联优化。
10. Mock 与真实 CLI 验证分开报告；不声称未实测的 E2E。

## 11. 验收标准（完整）

1. 创建 native 房，双方 bind + 关联成功；UI 显示绑定状态、generation、最后活动；bind stdout 与模型可见上下文零长期秘密（仅 nonce 与非秘密引用）。
2. A1-park：双方 park 中，Codex 发起讨论 → Stop 自动上报 → Claude 自动唤醒回应 → 主路径自动中继返回 → 多轮无人干预；Event Log 仅中继/绑定/gap/生命周期事件。
3. A1-降级：park 超时/禁用后同场景 → queued、UI 待取件、nudge 后送达；关联仍有效，前台 wait 可取件。
4. 路由：仅 `@user` → UI + 接力结束；peer 与 `@user` 同现 → peer 优先（独立用例）；无 handle → 结束；代码块/URL 内 handle 不路由；显式 send 正文的 handle 不驱动路由；显式 send 默认进对方 inbox 并唤醒 park，`--to @user` 进 UI（独立用例）。
5. Room UI → native slot：待取件 → handed_off；UI 不显示「原生已接受」。
6. 投递状态机：park 超时零认领 → 仍 queued；认领后 kill CLI → unknown → 显式 Retry 不自动重放；ack 丢失 → reaper 归 unknown。
7. **发布可靠性**：pending 持久化后 kill hook → 下次 hook 对账三分支各自断言（已接受 → 清除且不重发；明确未接受 → 同 seq 补报、inbox 无双份；未知 → 显示「结果未知」、无自动新 ID 重发）。原子写前 kill → 后续轮次序号连续、**丢失不可检测为合格行为**、断言无任何假发布/假送达记录；原子写后 kill 且 pending 被覆写 → 跳号 → gap 事件 → 显式重发恢复。hook 上报响应丢失 → 同 seq 重试幂等。send 同 ID 幂等；相同正文跨轮不误判去重。
8. FIFO + 持久性：并发投递保序；queued 跨 Service 重启存活。
9. 绑定安全：占用槽普通 bind 拒绝并提示 replace；同身份幂等恢复；抢绑 hook（无 nonce）不关联不取件；nonce 重放拒绝；replace 后旧 generation 凭据与消息全拒；跨 slot/跨房凭据调用拒绝；零 hook 环境 bind → 显式拒绝 + 指引。
10. hook 认证：第二次及后续 Stop hook 仅凭凭据文件 + 官方 session_id 认证取件，零模型参与。
11. 落盘面：模型上下文/Event Log/API 响应/日志/argv 零凭据；slot 目录仅 §D7 穷尽清单；零按消息散落文件（pending 为状态文件内单条有界字段）。
12. schema：新建 embedded/native 房均 11/prov4 且按 host_mode 激活；存量 10/prov3 房照常激活为 embedded 且事件字节零变化；旧二进制遇 11 房 replay 前拒绝；混合目录 + 旧二进制 → Service fail-closed 拒启且指引可操作。
13. checkpoint：新二进制产生的 checkpoint 被旧二进制严格读取路径（`DisallowUnknownFields`）正常接受；无 Room 的已注册 Project 跨升级/回滚保留；host_mode 可从 Room 事件重建。
14. 会话引用：`pairroom relay peer` 返回对方 session_id/transcript_path（关联上报后）；缺失/不可访问时接力不受影响；envelope 预算静态测试不超限。
15. Stop 缺口：用户中断轮次不发布且无假送达记录；StopFailure 产生可见失败事件、不中继内容。
16. native 房 UI：无 Interrupt 按钮；Cancel 仅清 queued；「在线」措辞符合 §9；UI/文档不声称跨路径语义去重。
17. embedded 全量回归绿：`make check`、`make smoke`、`make browser-check`；v6 协议与 byte-budget 测试不变。

## 12. 风险

- park UX（会话显示忙、原生输入需打断）与再武装 token 成本。
- Codex 关联通道不确定（Phase 0 ① 硬 gate）。
- 8-block 上限约束长讨论链（条件化 A1 已接受）。
- 用户中断轮次的内容不达 Room（诚实缺口）。
- 原子写前崩溃的发布丢失不可检测（诚实限制；agent `relay status` 自查为非保证缓解）。
- 发布内容重复风险仅存在于「结果未知」分支被人工误判重发的情形；自动路径由幂等键封闭。
- 同轮双发布为可见重复，非静默去重（依赖 bootstrap 纪律）。
- 凭据文件的同用户暴露面（威胁边界已诚实声明）。
- 厂商 CLI/hook 语义升级破坏 W2（SUPPORT.md 兼容性政策；Phase 0 记录实测版本号）。

## 13. Rollout / Rollback

- **Rollout**：Phase 0 结论写回本文档 → 实现 → 全量回归 + 真实 CLI 场景复验 → 发布。升级后所有新建房（含 embedded）为 schema 11，旧二进制不能打开——已接受的 S3' 代价；存量 schema-10 房零变化。**若 Phase 0 ① Codex gate 失败，发布结论必须明确声明「Claude + Codex 这组目标体验本期未实现」，Claude 单侧成功不构成 A1 或整体验收通过。**
- **Rollback**：① 首选恢复匹配版本完整备份（匹配二进制随备份保留——现有 UPGRADING 政策）；② 无备份：停止所有 owner → 备份 → 按现有 UPGRADING 隔离程序将全部 schema-11 房目录移出 `rooms/` 发现根 → 旧二进制正常启动管理存量房，checkpoint 无需处理（schema 2 不动）；③ native 专项：`pairroom relay unbind --purge-hooks` 卸载 hooks、删除工作区 `.pairroom/`、native 房按 retired-room 规则由匹配二进制拒绝或显式归档；④ 任何路径不自动迁移、不删数据。

## 14. 剩余 gap（实现设计阶段关闭，决定记录于 PR）

- park timeout / 再武装策略默认参数（Phase 0 产出）。
- pending 新鲜度上限取值。
- report_seq 原子认领与并发细则。
- SessionStart 先行关联优化取舍。
- Codex 关联载荷结论（Phase 0 ① 产出）。
- relay 技能在 Codex Desktop 的安装投影细节。
- `.pairroom/` 状态文件原子写与并发细则。
- 桌面通知缓解空闲降级（可选，一期外）。
- 实现落地的 `CONTEXT.md` 新术语条目（建议：宿主模式 host mode、native 房、park 驻留、publication gap 发布缺口、report sequence 上报序号、bind generation 绑定代次；最终命名按术语硬规则以仓库证据收敛）。

## Phase 0 结论（待追加）

（实现方在 Phase 0 完成后将结论、实测版本号与证据追加于本节；gate 失败时按 §13 声明，不得静默降级。）


## Phase 0 结论（2026-09-11，本 PR 实现记录）

- **① 文档/载荷通道成立，非真实 CLI 验收**：2026-09-11 读取 OpenAI 官方 [Hooks](https://developers.openai.com/codex/hooks)（重定向官方 learn.chatgpt.com/docs/hooks）确认项目级 `.codex/hooks.json`、精确 hook 定义信任、共同 `session_id`、Stop 的 `last_assistant_message` / `stop_hook_active`、`decision:block` + `reason` 自动续跑。Claude 官方 [Hooks](https://code.claude.com/docs/en/hooks) 提供同构 Stop 载荷与 StopFailure 仅可见性事件。安装不绕过同意；nonce 经真实 Stop 回传才关联。
- **运行环境边界**：本环境有离线 Go 1.25 源码与依赖，可运行 Mock/HTTP/崩溃窗口测试；没有已认证的 Codex Desktop 或 Claude Code 会话。不得据此声称真实多轮 E2E、厂商版本实测、token 成本实测或完整 A1 已通过。
- **接收策略**：保守默认 park 30 秒，hook timeout 45 秒；最多连续 8 次带真实新 envelope 的 block，无空转再武装。超时/禁用/预算耗尽保持 queued；下一次用户自然轮次重置预算。两个方向的真实时长、打断和 8-block 行为仍须发布前复验。
- **未纳入承诺**：session_crons / ScheduleWakeup、并发 resume、SessionStart 先行关联没有实测，均未用于实现或支持声明。
- **发布门禁**：本 PR 的实现和确定性测试不等于官方双端验收；完成 spec §10 的真实 Codex Desktop ↔ Claude Code 多轮场景之前，不将 Native 标为已验证稳定能力。
