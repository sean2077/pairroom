# PairRoom Language

Defines project-wide identity and collaboration terms.

## Canonical term languages

- `en`
- `zh-CN`

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
