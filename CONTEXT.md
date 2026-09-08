# PairRoom Language

Defines project-wide identity and collaboration terms.

## Canonical term languages

- `en`
- `zh-CN`

## Collaboration

**Collaboration mode**:
The Room's creation-time instruction choice: default or custom.
_Equivalent (zh-CN)_: 协作模式

**Lead**:
Agent 1's default planning and review responsibility in default mode, independent of its Runtime and tool permissions.
_Equivalent (zh-CN)_: 主导者

**Executor**:
Agent 2's default implementation, verification, and technical feedback responsibility in default mode.
_Equivalent (zh-CN)_: 执行者

**Permission profile**:
A participant's effective native-tool policy selection, separate from collaboration responsibility.
_Equivalent (zh-CN)_: 权限配置

**Agent pair profile**:
A named, reusable template for the two Agent slots' Runtime and native configuration overrides, optionally selected as the Service default for new Rooms.
_Equivalent (zh-CN)_: Agent 组合配置

## Language

**Participant slot**:
One of the Room's two stable participant identities, persisted as ActorID `claude` for Agent 1 or `codex` for Agent 2 independently of the selected native runtime.
_Equivalent (zh-CN)_: Agent 槽位

**Runtime**:
The official Claude Code, Codex, or Grok Build harness currently selected for a participant slot.
_Equivalent (zh-CN)_: 原生运行时

**Mention handle**:
The public runtime-derived `@` identifier used to address a Room participant, including a stable `0/1` suffix when both participant slots use the same Runtime.
_Equivalent (zh-CN)_: 点名句柄
_Avoid (en)_: slot alias
_Avoid (zh-CN)_: 槽位别名

**Agent relay**:
Delivery of one Agent's complete visible response to the other participant after the current native Turn boundary.
_Equivalent (zh-CN)_: Agent 接力
_Avoid (en)_: handoff
_Avoid (zh-CN)_: 交接
