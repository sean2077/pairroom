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

**Provider**:
The credential and configuration source behind a participant slot's Runtime: the native default, or a reference to a supported read-only CC Switch Profile.
_Equivalent (zh-CN)_: Provider

**Provider reference**:
The durable, secret-free pointer to a CC Switch Profile; its `cc-switch:<app_type>/<profile_id>` form is an internal machine reference, not a display name.
_Equivalent (zh-CN)_: Provider 引用
_Avoid (en)_: provider string
_Avoid (zh-CN)_: Provider 字符串

**Provider name**:
The credential-redacted display name of the referenced CC Switch Profile, shown wherever a Provider is rendered; it never replaces the reference in a stored selection.
_Equivalent (zh-CN)_: Provider 名称

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

## Hosting

**Host mode**:
The immutable Room-level choice between PairRoom-owned adapter processes (`embedded`) and user-owned native sessions (`native`), independent of collaboration mode.
_Equivalent (zh-CN)_: 宿主模式

**Native host mode**:
A Room that relays messages and records audit while participants work in their original harnesses.
_Equivalent (zh-CN)_: Native 宿主模式

**Park window**:
The bounded interval during which an approved Stop hook waits for new inbox work before the native response ends.
_Equivalent (zh-CN)_: Park 等待窗口

**Publication gap**:
An observed jump in one binding generation's report sequence, not an inference from missing activity.
_Equivalent (zh-CN)_: 发布缺口
